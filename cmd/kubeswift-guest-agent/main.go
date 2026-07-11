package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"log"
	"os"
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
	flag.Parse()
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
