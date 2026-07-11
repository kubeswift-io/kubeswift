package main

import (
	"errors"
	"io"
	"os/exec"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/kubeswift-io/kubeswift/internal/guestagent"
)

// execStream runs argv (chrooted into the sandbox root, with env + cwd) and streams
// its stdout/stderr to w as frames LIVE, then a terminal Exit frame with the code —
// unbounded output, no 1 MiB cap. Used by `swiftctl sandbox exec` and the foundation
// interactive attach builds on. w is the (frame-serialized) connection back to the host.
func (h *handler) execStream(w io.Writer, req Request) {
	fc := &frameConn{w: w}
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
	if err := cmd.Start(); err != nil {
		fc.exitErr("exec: " + err.Error())
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go pumpFrames(fc, guestagent.FrameStdout, stdout, &wg)
	go pumpFrames(fc, guestagent.FrameStderr, stderr, &wg)
	wg.Wait() // both pipes drained (EOF) => the process has closed its outputs

	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			_ = fc.write(guestagent.FrameStderr, []byte("kubeswift-guest-agent: exec: "+err.Error()+"\n"))
			code = 255
		}
	}
	fc.exit(code)
}

// frameConn serializes frame writes across the two output pumps so a reader always
// sees whole frames on the shared connection.
type frameConn struct {
	w  io.Writer
	mu sync.Mutex
}

func (c *frameConn) write(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return guestagent.WriteFrame(c.w, typ, payload)
}

func (c *frameConn) exit(code int) { _ = c.write(guestagent.FrameExit, guestagent.ExitCode(code)) }

// exitErr surfaces a pre-run failure (exec disabled, empty argv, spawn error) on the
// stream: a stderr frame plus a non-zero Exit frame, so the host prints it and exits
// non-zero — never a silent stall.
func (c *frameConn) exitErr(msg string) {
	_ = c.write(guestagent.FrameStderr, []byte("kubeswift-guest-agent: "+msg+"\n"))
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
