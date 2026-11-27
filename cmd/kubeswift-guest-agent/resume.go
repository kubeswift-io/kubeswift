package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// resume-sync: what a guest restored from a snapshot needs that a resume does
// not give it. The VM carries on at the moment it was paused, so its wall
// clock is behind by the whole suspended interval (CH restores kvm-clock, not
// the time of day), and its RNG state is the snapshot's. VMGENID already
// makes the kernel reseed on restore; the host's entropy is a second source.
//
// The host sends its time (unix ns) when it builds the request and, optionally,
// up to maxResumeEntropy random bytes. The agent steps CLOCK_REALTIME and
// mixes the bytes in with RNDADDENTROPY. The reply carries the guest's clock
// offset from the host time before the step, so the host can record it.

// maxResumeEntropy bounds the entropy a request may carry (the kernel's input
// pool is 256 bytes; more adds nothing).
const maxResumeEntropy = 512

// SyncResult is the resume-sync reply.
type SyncResult struct {
	// Synced lists what was done: "clock", "entropy".
	Synced []string `json:"synced,omitempty"`
	// ClockOffsetMs is guest time minus the request's host time before the
	// step (negative: the guest was behind).
	ClockOffsetMs *int64 `json:"clockOffsetMs,omitempty"`
}

func (h *handler) resumeSync(req Request) Response {
	if req.Now <= 0 && len(req.Entropy) == 0 {
		return Response{OK: false, Error: "resume-sync: nothing to do (no now, no entropy)"}
	}
	if len(req.Entropy) > maxResumeEntropy {
		return Response{OK: false, Error: fmt.Sprintf("resume-sync: entropy is %d bytes, at most %d", len(req.Entropy), maxResumeEntropy)}
	}
	res := &SyncResult{}
	if req.Now > 0 {
		host := time.Unix(0, req.Now)
		off := time.Since(host).Milliseconds()
		res.ClockOffsetMs = &off
		if err := h.sys.setClock(host); err != nil {
			return Response{OK: false, Error: "resume-sync: set clock: " + err.Error(), SyncResult: res}
		}
		res.Synced = append(res.Synced, "clock")
	}
	if len(req.Entropy) > 0 {
		if err := h.sys.addEntropy(req.Entropy); err != nil {
			return Response{OK: false, Error: "resume-sync: add entropy: " + err.Error(), SyncResult: res}
		}
		res.Synced = append(res.Synced, "entropy")
	}
	return Response{OK: true, SyncResult: res}
}

func (realSystem) setClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

// addEntropy mixes data into the kernel's input pool and credits it
// (RNDADDENTROPY; struct rand_pool_info {int entropy_count; int buf_size;
// __u32 buf[];}). The host is trusted, so its bytes are credited in full.
func (realSystem) addEntropy(data []byte) error {
	buf := make([]byte, 8+len(data))
	binary.NativeEndian.PutUint32(buf[0:4], uint32(len(data)*8))
	binary.NativeEndian.PutUint32(buf[4:8], uint32(len(data)))
	copy(buf[8:], data)
	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(unix.RNDADDENTROPY), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return errno
	}
	return nil
}
