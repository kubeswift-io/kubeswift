package main

import (
	"bytes"
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
	flag.Parse()
	if env := os.Getenv("KUBESWIFT_GUEST_AGENT_PORT"); env != "" {
		if p, err := strconv.Atoi(env); err == nil {
			*port = p
		}
	}

	h := &handler{sys: realSystem{}, version: version}

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

// serve reads one newline-delimited JSON request, dispatches it, writes the JSON
// response, and closes the connection (one request/response per connection).
func serve(h *handler, nfd int) {
	defer unix.Close(nfd)
	// bound the connection so a stuck/hostile client cannot pin a goroutine
	tv := unix.Timeval{Sec: recvTimeoutSecs}
	_ = unix.SetsockoptTimeval(nfd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)

	var buf []byte
	tmp := make([]byte, 4096)
	for len(buf) < maxRequestBytes {
		n, err := unix.Read(nfd, tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[:i]
				break
			}
		}
		if err != nil || n == 0 {
			break
		}
	}

	resp := h.handle(buf)
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
