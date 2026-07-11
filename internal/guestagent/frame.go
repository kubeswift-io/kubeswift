// Package guestagent defines the host<->guest streaming wire protocol shared by
// the in-guest agent (cmd/kubeswift-guest-agent) and swiftctl for SwiftSandbox
// `exec` (streaming) and, later, interactive `attach`.
//
// A streaming exchange begins with the same newline-JSON request the single-shot
// ops use (so the CH hybrid-vsock CONNECT/OK handshake is unchanged); once the
// request sets `stream:true` the connection switches to a sequence of length-
// prefixed frames in BOTH directions:
//
//	[type:1][len:4 big-endian][payload:len]
//
// Guest->host: Stdout, Stderr, Exit (payload = 4-byte exit code). Host->guest:
// Stdin and StdinClose (for -i and attach), Resize (attach). Frames on one
// connection are serialized by the writer, so a reader always sees whole frames.
package guestagent

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Frame types.
const (
	FrameStdout     byte = 1 // guest->host: workload stdout
	FrameStderr     byte = 2 // guest->host: workload stderr
	FrameExit       byte = 3 // guest->host: terminal; payload = int32 exit code, big-endian
	FrameStdin      byte = 4 // host->guest: workload stdin (attach / -i)
	FrameResize     byte = 5 // host->guest: TTY resize (attach); payload = rows:2 + cols:2, big-endian
	FrameStdinClose byte = 6 // host->guest: stdin reached EOF; close the command's stdin (keep the connection for output)
)

// MaxFramePayload bounds a single frame's payload so a hostile/broken peer cannot
// force an unbounded allocation on the reader.
const MaxFramePayload = 1 << 20 // 1 MiB

// WriteFrame writes one frame ([type][len:4 BE][payload]) to w in a single logical
// message. Callers that share a writer across goroutines must serialize WriteFrame.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads one frame from r. It returns io.EOF only when the stream ends
// cleanly on a frame boundary (a truncated frame yields io.ErrUnexpectedEOF).
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFramePayload {
		return 0, nil, fmt.Errorf("frame payload too large: %d", n)
	}
	if n == 0 {
		return hdr[0], nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return hdr[0], buf, nil
}

// ExitCode encodes an exit code as a 4-byte big-endian FrameExit payload.
func ExitCode(code int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(int32(code)))
	return b[:]
}

// DecodeExitCode reads a FrameExit payload back into an int (0 if malformed).
func DecodeExitCode(payload []byte) int {
	if len(payload) < 4 {
		return 0
	}
	return int(int32(binary.BigEndian.Uint32(payload)))
}

// ResizePayload encodes a FrameResize payload (rows:2 + cols:2, big-endian).
func ResizePayload(rows, cols uint16) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], rows)
	binary.BigEndian.PutUint16(b[2:4], cols)
	return b[:]
}

// DecodeResize reads a FrameResize payload back into (rows, cols); (0,0) if malformed.
func DecodeResize(payload []byte) (rows, cols uint16) {
	if len(payload) < 4 {
		return 0, 0
	}
	return binary.BigEndian.Uint16(payload[0:2]), binary.BigEndian.Uint16(payload[2:4])
}

// FrameWriter serializes WriteFrame across goroutines that share one connection —
// both the agent (stdout/stderr/exit) and swiftctl (stdin/resize) write frames
// concurrently, and a frame's header+payload must not interleave with another's.
type FrameWriter struct {
	w  io.Writer
	mu sync.Mutex
}

// NewFrameWriter wraps w so Write is safe for concurrent use.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write emits one frame atomically with respect to other Write callers.
func (f *FrameWriter) Write(typ byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return WriteFrame(f.w, typ, payload)
}
