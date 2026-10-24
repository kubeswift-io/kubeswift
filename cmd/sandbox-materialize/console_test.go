package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// syncBuffer is a bytes.Buffer safe for the follower and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func waitFor(t *testing.T, out *syncBuffer, want []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Equal(out.Bytes(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %q, want %q", out.Bytes(), want)
}

// The console container copies a file that does not exist yet (a warm slot's
// workload log appears at checkout), as it grows, byte for byte (binary
// output too), starts over when it is truncated, and copies what is left when
// it is stopped.
func TestFollowConsole(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workload.log")
	out := &syncBuffer{}
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- follow(path, out, 200*time.Millisecond, stop) }()

	time.Sleep(50 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("start\n"))
	waitFor(t, out, []byte("start\n"))
	f.Write([]byte{0, 0xff, 0xfe, '\n'})
	waitFor(t, out, []byte("start\n\x00\xff\xfe\n"))

	// Truncated and rewritten: followed from the start again.
	f.Close()
	if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, []byte("start\n\x00\xff\xfe\nx\n"))

	// Written just before the stop: still copied.
	f, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	f.Write([]byte("last\n"))
	f.Close()
	stop <- os.Interrupt
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
	if got := out.Bytes(); !bytes.HasSuffix(got, []byte("last\n")) {
		t.Errorf("output before the stop lost: %q", got)
	}
}

// Stopped before the file ever appeared (a warm slot never checked out).
func TestFollowConsole_StopWithoutAFile(t *testing.T) {
	stop := make(chan os.Signal, 1)
	stop <- os.Interrupt
	out := &syncBuffer{}
	if err := follow(filepath.Join(t.TempDir(), "never"), out, 50*time.Millisecond, stop); err != nil || len(out.Bytes()) != 0 {
		t.Fatalf("err=%v out=%q", err, out.Bytes())
	}
}

// The console container stops when its launcher's FIFO closes, however the
// launcher ended, copying what the launcher wrote first, and exits 0.
func TestFollowConsole_UntilTheLauncherCloses(t *testing.T) {
	dir := t.TempDir()
	file, fifo := filepath.Join(dir, "serial.log"), filepath.Join(dir, ".launcher")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	launcher, err := os.OpenFile(fifo, os.O_RDWR, 0) // what swiftletd holds
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan os.Signal, 2)
	go func() {
		untilClosed(fifo, time.Second, 20*time.Millisecond)
		stop <- os.Interrupt
	}()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- follow(file, out, 50*time.Millisecond, stop) }()

	if err := os.WriteFile(file, []byte("last words\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("stopped while the launcher lives")
	case <-time.After(300 * time.Millisecond):
	}
	launcher.Close() // the launcher exits (or is killed)
	select {
	case err := <-done:
		if err != nil || !bytes.Equal(out.Bytes(), []byte("last words\n")) {
			t.Fatalf("err=%v out=%q", err, out.Bytes())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop when the launcher closed its FIFO")
	}

	// A launcher that never makes one: give up after the wait.
	start := time.Now()
	untilClosed(filepath.Join(dir, "none"), 100*time.Millisecond, 20*time.Millisecond)
	if time.Since(start) > 2*time.Second {
		t.Error("waited past the limit for a missing FIFO")
	}
}
