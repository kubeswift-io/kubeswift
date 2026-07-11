package main

import (
	"errors"
	"io"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/kubeswift-io/kubeswift/internal/guestagent"
)

// execStream runs argv (chrooted into the sandbox root, with env + cwd) and streams
// its stdout/stderr to the host as frames LIVE, then a terminal Exit frame — unbounded
// output, no cap. When req.Stdin is set it also reads host FrameStdin frames into the
// command's stdin (a plain pipe; the PTY path is execAttach). rw is the framed
// connection back to the host.
func (h *handler) execStream(rw io.ReadWriter, req Request) {
	fc := &frameConn{FrameWriter: guestagent.NewFrameWriter(rw)}
	cmd, err := h.buildExecCmd(req)
	if err != nil {
		fc.exitErr(err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fc.exitErr("stdout pipe: " + err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fc.exitErr("stderr pipe: " + err.Error())
		return
	}
	var stdin io.WriteCloser
	if req.Stdin {
		if stdin, err = cmd.StdinPipe(); err != nil {
			fc.exitErr("stdin pipe: " + err.Error())
			return
		}
	}
	if err := cmd.Start(); err != nil {
		fc.exitErr("exec: " + err.Error())
		return
	}
	if req.Stdin {
		go pumpStdin(rw, stdin) // host FrameStdin -> cmd stdin; closes stdin on host EOF/close
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pumpFrames(fc, guestagent.FrameStdout, stdout, &wg)
	go pumpFrames(fc, guestagent.FrameStderr, stderr, &wg)
	wg.Wait() // both pipes drained (EOF) => the process closed its outputs

	fc.exit(waitCode(cmd, fc))
}

// execAttach runs argv on a PTY (interactive attach): the PTY output streams back as
// FrameStdout, host FrameStdin frames are written to the PTY, and FrameResize frames
// resize it. The command's stdin/stdout/stderr are the PTY slave (inherited across the
// chroot as fds, so /newroot needs no /dev/pts of its own).
func (h *handler) execAttach(rw io.ReadWriter, req Request) {
	fc := &frameConn{FrameWriter: guestagent.NewFrameWriter(rw)}
	cmd, err := h.buildExecCmd(req)
	if err != nil {
		fc.exitErr(err.Error())
		return
	}
	sz := &pty.Winsize{Rows: req.Rows, Cols: req.Cols}
	if sz.Rows == 0 {
		sz.Rows = 24
	}
	if sz.Cols == 0 {
		sz.Cols = 80
	}
	ptmx, err := pty.StartWithSize(cmd, sz)
	if err != nil {
		fc.exitErr("pty: " + err.Error())
		return
	}
	defer func() { _ = ptmx.Close() }()

	// host control frames (stdin, resize) -> PTY. Runs until the host closes the
	// connection (ReadFrame error), which then closes the PTY so the shell sees EOF.
	go func() {
		for {
			typ, payload, err := guestagent.ReadFrame(rw)
			if err != nil {
				_ = ptmx.Close()
				return
			}
			switch typ {
			case guestagent.FrameStdin:
				_, _ = ptmx.Write(payload)
			case guestagent.FrameResize:
				rows, cols := guestagent.DecodeResize(payload)
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
			}
		}
	}()

	// PTY output -> FrameStdout, until the child exits and the master EOFs.
	var wg sync.WaitGroup
	wg.Add(1)
	go pumpFrames(fc, guestagent.FrameStdout, ptmx, &wg)
	wg.Wait()

	fc.exit(waitCode(cmd, fc))
}

// waitCode waits for cmd and returns its exit code, surfacing a non-ExitError spawn/run
// failure as a stderr frame + code 255.
func waitCode(cmd *exec.Cmd, fc *frameConn) int {
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		_ = fc.write(guestagent.FrameStderr, []byte("kubeswift-guest-agent: exec: "+err.Error()+"\n"))
		return 255
	}
	return 0
}

// frameConn adds exit/error helpers over the serialized FrameWriter.
type frameConn struct {
	*guestagent.FrameWriter
}

func (c *frameConn) write(typ byte, payload []byte) error { return c.Write(typ, payload) }

func (c *frameConn) exit(code int) { _ = c.Write(guestagent.FrameExit, guestagent.ExitCode(code)) }

// exitErr surfaces a pre-run failure (exec disabled, empty argv, spawn error) on the
// stream: a stderr frame plus a non-zero Exit frame, so the host prints it and exits
// non-zero — never a silent stall.
func (c *frameConn) exitErr(msg string) {
	_ = c.Write(guestagent.FrameStderr, []byte("kubeswift-guest-agent: "+msg+"\n"))
	c.exit(255)
}

// pumpFrames copies r into typ-tagged frames until EOF.
func pumpFrames(fc *frameConn, typ byte, r io.Reader, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := fc.write(typ, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pumpStdin copies host FrameStdin frames into w (the command's stdin), closing w on a
// FrameStdinClose (host stdin hit EOF) or when the host closes the connection — so a
// command like `cat` sees EOF and exits instead of hanging.
func pumpStdin(r io.Reader, w io.WriteCloser) {
	defer w.Close()
	for {
		typ, payload, err := guestagent.ReadFrame(r)
		if err != nil {
			return
		}
		switch typ {
		case guestagent.FrameStdin:
			if _, werr := w.Write(payload); werr != nil {
				return
			}
		case guestagent.FrameStdinClose:
			return // defer closes the command's stdin
		}
	}
}

// fdWriter adapts a raw vsock fd to io.Writer, retrying short writes.
type fdWriter struct{ fd int }

func (w fdWriter) Write(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := unix.Write(w.fd, p[total:])
		if n > 0 {
			total += n
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// fdReadWriter is the full-duplex framed connection over a raw vsock fd. It replays any
// bytes the request-line read consumed past the newline (rest) before reading the fd,
// so the first host frame is never lost.
type fdReadWriter struct {
	fd   int
	rest []byte
}

func (rw *fdReadWriter) Read(p []byte) (int, error) {
	if len(rw.rest) > 0 {
		n := copy(p, rw.rest)
		rw.rest = rw.rest[n:]
		return n, nil
	}
	return unix.Read(rw.fd, p)
}

func (rw *fdReadWriter) Write(p []byte) (int, error) { return fdWriter{rw.fd}.Write(p) }
