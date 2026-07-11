package guestagent

import (
	"bytes"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	frames := []struct {
		typ byte
		pay []byte
	}{
		{FrameStdout, []byte("hello\n")},
		{FrameStderr, []byte("oops")},
		{FrameStdout, nil}, // zero-length payload is legal
		{FrameExit, ExitCode(7)},
	}
	for _, f := range frames {
		if err := WriteFrame(&buf, f.typ, f.pay); err != nil {
			t.Fatalf("write %d: %v", f.typ, err)
		}
	}
	for i, want := range frames {
		typ, pay, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		if typ != want.typ || !bytes.Equal(pay, want.pay) {
			t.Fatalf("frame %d = (%d,%q), want (%d,%q)", i, typ, pay, want.typ, want.pay)
		}
	}
	if _, _, err := ReadFrame(&buf); err != io.EOF {
		t.Fatalf("clean end must be io.EOF, got %v", err)
	}
	if got := DecodeExitCode(ExitCode(7)); got != 7 {
		t.Fatalf("exit code round-trip = %d", got)
	}
}

func TestReadFrameTruncated(t *testing.T) {
	// header claims 10 bytes but only 3 follow -> ErrUnexpectedEOF, not EOF.
	var buf bytes.Buffer
	buf.Write([]byte{FrameStdout, 0, 0, 0, 10})
	buf.WriteString("abc")
	if _, _, err := ReadFrame(&buf); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated frame err = %v, want ErrUnexpectedEOF", err)
	}
}

func TestWriteFrameRejectsOversize(t *testing.T) {
	if err := WriteFrame(io.Discard, FrameStdout, make([]byte, MaxFramePayload+1)); err == nil {
		t.Fatal("oversize payload must be rejected")
	}
}
