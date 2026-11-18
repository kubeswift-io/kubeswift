package main

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEgressSpec(t *testing.T) {
	s, err := parseEgressSpec("127.0.0.1:3128,vsock:1080")
	if err != nil || s.listen != "127.0.0.1:3128" || s.port != 1080 {
		t.Fatalf("got %+v %v", s, err)
	}
	for _, bad := range []string{
		"", "127.0.0.1:3128", "0.0.0.0:3128,vsock:1080", "10.0.0.1:3128,vsock:1080",
		"127.0.0.1:3128,tcp:1080", "127.0.0.1:3128,vsock:0", "127.0.0.1:3128,vsock:x",
		"localhost:3128,vsock:1080",
	} {
		if _, err := parseEgressSpec(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

// The forwarder carries bytes both ways unchanged and passes the client's
// half-close on, so a request/response over one connection completes.
func TestForwardEgress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// The "host": a TCP echo server standing in for the vsock peer. It
	// reads until EOF (the half-close), then answers.
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	got := make(chan []byte, 1)
	go func() {
		c, err := host.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- b
		_, _ = c.Write(append([]byte("echo:"), b...))
	}()
	var dialed atomic.Uint32
	dial := func(p uint32) (io.ReadWriteCloser, error) {
		dialed.Store(p)
		return net.Dial("tcp", host.Addr().String())
	}
	go func() { _ = forwardEgress(ln, 1080, dial) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := []byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if b := <-got; !bytes.Equal(b, req) {
		t.Fatalf("host got %q", b)
	}
	if want := append([]byte("echo:"), req...); !bytes.Equal(resp, want) {
		t.Fatalf("client got %q", resp)
	}
	if p := dialed.Load(); p != 1080 {
		t.Fatalf("dialed port %d", p)
	}
}
