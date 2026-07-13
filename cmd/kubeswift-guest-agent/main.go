package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	maxRequestBytes = 64 * 1024
	recvTimeoutSecs = 30
)

func main() {
	port := flag.Int("port", DefaultPort, "AF_VSOCK port to listen on")
	execRoot := flag.String("exec-root", "", "chroot dir for the exec op (empty = agent's own root; SwiftSandbox passes /newroot)")
	runMode := flag.Bool("run", false, "one-shot: exec the argv after -- chrooted into --exec-root at --cwd, inheriting stdio; exit with the child's code. Used by the sandbox bridge to honor spec.workingDir without a guest shell.")
	runCwd := flag.String("cwd", "", "working directory INSIDE --exec-root for --run (must exist in the rootfs)")
	flag.Parse()

	// One-shot local exec (the sandbox cold-boot path): chroot + chdir + exec via
	// the same buildExecCmd the vsock exec op uses (in-chroot PATH resolution, works
	// for distroless — no guest shell). The bridge runs this as a FOREGROUND child so
	// it stays PID 1 and still captures the exit code + powers the VM off.
	if *runMode {
		os.Exit(runOneShot(*execRoot, *runCwd, flag.Args()))
	}
	if env := os.Getenv("KUBESWIFT_GUEST_AGENT_PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil {
			*port = p
		}
	}

	h := &handler{sys: realSystem{}, version: version, execRoot: *execRoot}

	lfd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		log.Fatalf("kubeswift-guest-agent: AF_VSOCK socket: %v", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(*port)}
	if err := unix.Bind(lfd, sa); err != nil {
		log.Fatalf("kubeswift-guest-agent: bind vsock port %d: %v", *port, err)
	}
	if err := unix.Listen(lfd, 16); err != nil {
		log.Fatalf("kubeswift-guest-agent: listen: %v", err)
	}
	log.Printf("kubeswift-guest-agent %s listening on AF_VSOCK port %d", version, *port)

	for {
		nfd, _, err := unix.Accept(lfd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Printf("kubeswift-guest-agent: accept: %v", err)
			continue
		}
		go serve(h, nfd)
	}
}

// runOneShot chroots into root, chdirs to cwd, and execs argv as a foreground
// child inheriting the caller's stdio, returning the child's exit code. Env is the
// caller's own environment (the bridge exports the config-disk ENV before invoking
// this), matching what a plain `chroot root argv` would inherit — buildExecCmd's
// synthetic PATH/HOME defaults are deliberately overridden. Returns 127 when the
// command cannot be built or started (mirrors the shell "command not found" code).
func runOneShot(root, cwd string, argv []string) int {
	h := &handler{sys: realSystem{}, version: version, execRoot: root}
	cmd, err := h.buildExecCmd(Request{Argv: argv, Cwd: cwd})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubeswift-guest-agent --run:", err)
		return 127
	}
	cmd.Env = os.Environ() // inherit the bridge's env, exactly like `chroot root argv` does
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "kubeswift-guest-agent --run:", err)
		return 127
	}
	return 0
}

// serve reads one newline-delimited JSON request and dispatches it. Single-shot ops
// write one JSON response and close. A streaming exec (op=exec, stream=true) hands the
// connection to execStream, which writes framed stdout/stderr/exit for the command's
// lifetime (see internal/guestagent).
func serve(h *handler, nfd int) {
	defer unix.Close(nfd)
	// bound the request read so a stuck/hostile client cannot pin a goroutine
	tv := unix.Timeval{Sec: recvTimeoutSecs}
	_ = unix.SetsockoptTimeval(nfd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	line, rest := readRequestLine(nfd)

	var req Request
	if json.Unmarshal(line, &req) == nil && req.Op == "exec" && req.Stream {
		// clear the read deadline — a streamed command may run far longer than the
		// request-read timeout, and stdin/attach keeps the host sending frames.
		_ = unix.SetsockoptTimeval(nfd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{})
		rw := &fdReadWriter{fd: nfd, rest: rest}
		if req.TTY {
			h.execAttach(rw, req)
		} else {
			h.execStream(rw, req)
		}
		return
	}

	writeAll(nfd, h.handle(line))
}

// readRequestLine reads one newline-delimited request (bounded by maxRequestBytes) and
// returns the line plus any bytes already read past the newline (the start of the frame
// stream for a streaming/attach request).
func readRequestLine(nfd int) (line, rest []byte) {
	var buf []byte
	tmp := make([]byte, 4096)
	for len(buf) < maxRequestBytes {
		n, err := unix.Read(nfd, tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				return buf[:i], buf[i+1:]
			}
		}
		if err != nil || n == 0 {
			break
		}
	}
	return buf, nil
}

func writeAll(nfd int, resp []byte) {
	for len(resp) > 0 {
		n, err := unix.Write(nfd, resp)
		if n > 0 {
			resp = resp[n:]
		}
		if err != nil {
			break
		}
	}
}
