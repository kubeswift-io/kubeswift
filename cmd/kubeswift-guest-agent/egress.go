package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// The brokered egress forwarder (spec.network.mode: brokered). The guest has
// no network device; its workload reaches the outside only through an HTTP
// proxy on its own loopback (HTTP_PROXY=http://127.0.0.1:3128). The forwarder
// accepts each connection there and carries its bytes unchanged over vsock to
// the host (CID 2), where swiftletd's broker parses the request and enforces
// the sandbox's egress policy. The forwarder decides nothing and parses
// nothing: the guest is untrusted, so the host side is the one that must
// parse. A workload that dials the vsock port itself gains nothing over it.

// maxEgressConns bounds the connections the forwarder carries at once.
const maxEgressConns = 1024

// egressSpec is --egress-forward: "<listen addr>,vsock:<port>".
type egressSpec struct {
	listen string
	port   uint32
}

func parseEgressSpec(s string) (egressSpec, error) {
	listen, vs, ok := strings.Cut(s, ",")
	if !ok {
		return egressSpec{}, fmt.Errorf("%q: want <addr:port>,vsock:<port>", s)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return egressSpec{}, fmt.Errorf("%q: %v", listen, err)
	}
	// Loopback only: the guest has no NIC in brokered mode, and the
	// forwarder must never serve anything else if it gains one.
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return egressSpec{}, fmt.Errorf("%q: the forwarder listens on loopback only", listen)
	}
	ps, ok := strings.CutPrefix(vs, "vsock:")
	if !ok {
		return egressSpec{}, fmt.Errorf("%q: want vsock:<port>", vs)
	}
	p, err := strconv.ParseUint(ps, 10, 32)
	if err != nil || p == 0 {
		return egressSpec{}, fmt.Errorf("%q: not a vsock port", vs)
	}
	return egressSpec{listen: listen, port: uint32(p)}, nil
}

// dialHost opens a vsock stream to the host (CID 2) on port; Cloud
// Hypervisor's hybrid vsock connects it to <socket>_<port> on the host side.
func dialHost(port uint32) (*os.File, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: port}); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "vsock"), nil
}

// serveEgress runs the forwarder until the listener fails.
func serveEgress(spec egressSpec, dial func(uint32) (io.ReadWriteCloser, error)) error {
	ln, err := net.Listen("tcp", spec.listen)
	if err != nil {
		return err
	}
	log.Printf("kubeswift-guest-agent: egress forwarder on %s -> vsock host:%d", spec.listen, spec.port)
	return forwardEgress(ln, spec.port, dial)
}

func forwardEgress(ln net.Listener, port uint32, dial func(uint32) (io.ReadWriteCloser, error)) error {
	slots := make(chan struct{}, maxEgressConns)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			log.Printf("kubeswift-guest-agent: egress: too many connections, closing one")
			c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			pipeEgress(c, port, dial)
		}()
	}
}

func pipeEgress(c net.Conn, port uint32, dial func(uint32) (io.ReadWriteCloser, error)) {
	defer c.Close()
	up, err := dial(port)
	if err != nil {
		log.Printf("kubeswift-guest-agent: egress: vsock host:%d: %v", port, err)
		return
	}
	defer up.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(up, c)
		halfClose(up)
	}()
	_, _ = io.Copy(c, up)
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	wg.Wait()
}

// halfClose ends the write side of the vsock stream so the host sees EOF
// while the reply still flows.
func halfClose(w io.ReadWriteCloser) {
	switch v := w.(type) {
	case *os.File:
		if rc, err := v.SyscallConn(); err == nil {
			_ = rc.Control(func(fd uintptr) { _ = unix.Shutdown(int(fd), unix.SHUT_WR) })
		}
	case interface{ CloseWrite() error }:
		_ = v.CloseWrite()
	}
}
