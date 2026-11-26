package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
)

func TestReadTarget(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "annotations")
	write := func(s string) {
		if err := os.WriteFile(f, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a=\"x\"\npodmode.kubeswift.io/workload=\"{\\\"v\\\":1}\"\npodmode.kubeswift.io/target=\"10.1.2.3\"\n")
	if got, err := readTarget(f); err != nil || got != "10.1.2.3" {
		t.Errorf("got %q %v", got, err)
	}
	write("a=\"x\"\n")
	if got, err := readTarget(f); err != nil || got != "" {
		t.Errorf("unset: got %q %v", got, err)
	}
	write("podmode.kubeswift.io/target=\"evil.example:22\"\n")
	if _, err := readTarget(f); err == nil {
		t.Error("a non-IP target was accepted")
	}
	if _, err := readTarget(filepath.Join(dir, "none")); !os.IsNotExist(err) {
		t.Errorf("missing file: %v", err)
	}
}

func TestParsePorts(t *testing.T) {
	if p, err := parsePorts("8080, 9090,"); err != nil || len(p) != 2 || p[1] != 9090 {
		t.Errorf("%v %v", p, err)
	}
	for _, bad := range []string{"0", "65536", "x"} {
		if _, err := parsePorts(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestForwardToTarget(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	port := up.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var target atomic.Value
	target.Store("")
	go servePort(ctx, ln, port, func() string { return target.Load().(string) })
	go func() { <-ctx.Done(); ln.Close() }()

	// No target: the connection is closed at once, not held.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("without a target: %v, want EOF", err)
	}
	c.Close()

	target.Store("127.0.0.1")
	c, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := "hello " + strconv.Itoa(port)
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Errorf("echo %q %v", buf, err)
	}
}

func TestWorkloadExit(t *testing.T) {
	dir := t.TempDir()
	old := annotationsFile
	annotationsFile = filepath.Join(dir, "annotations")
	t.Cleanup(func() { annotationsFile = old })
	write := func(v string) {
		if err := os.WriteFile(annotationsFile, []byte(podmode.AnnotationExit+"="+strconv.Quote(v)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("1:3")
	if _, ok := workloadExit("1:3"); ok {
		t.Error("an exit recorded before the shim started counts")
	}
	if code, ok := workloadExit(""); !ok || code != 3 {
		t.Errorf("exit %d %v", code, ok)
	}
	write("2:256")
	if code, ok := workloadExit("1:3"); !ok || code != 1 {
		t.Errorf("256 maps to %d %v", code, ok)
	}
	write("2:137")
	if code, _ := workloadExit("1:3"); code != 137 {
		t.Errorf("137 maps to %d", code)
	}
}
