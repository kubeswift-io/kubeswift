package thinpool

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// recordingWriter remembers which offsets were written.
type recordingWriter struct {
	buf     []byte
	offsets []int64
}

func (w *recordingWriter) WriteAt(p []byte, off int64) (int, error) {
	w.offsets = append(w.offsets, off)
	if end := int(off) + len(p); end > len(w.buf) {
		grown := make([]byte, end)
		copy(grown, w.buf)
		w.buf = grown
	}
	copy(w.buf[off:], p)
	return len(p), nil
}

const blk = BlockSectors * 512

// dm-thin allocates a block on ANY write, zeros included, so a naive copy of a
// mostly-empty raw image provisions its full size. An unwritten thin block reads
// as zeros, so skipping all-zero blocks is invisible to the guest and free.
func TestCopySkippingZeros_WritesOnlyNonZeroBlocks(t *testing.T) {
	img := make([]byte, 8*blk)
	copy(img[0:], []byte("first block has data"))
	copy(img[5*blk+100:], []byte("so does the sixth, mid-block"))

	w := &recordingWriter{}
	n, err := copySkippingZeros(context.Background(), w, bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(img)) {
		t.Errorf("consumed %d bytes, want %d", n, len(img))
	}
	if len(w.offsets) != 2 || w.offsets[0] != 0 || w.offsets[1] != 5*blk {
		t.Errorf("wrote at offsets %v, want exactly [0 %d]", w.offsets, 5*blk)
	}
	// What was written reproduces the image wherever it was non-zero, and a
	// non-zero byte mid-block got its WHOLE block written, aligned.
	if !bytes.Equal(w.buf[5*blk:6*blk], img[5*blk:6*blk]) {
		t.Error("a block with data mid-way was not written whole and aligned")
	}
}

// A fully zero image provisions nothing at all.
func TestCopySkippingZeros_AllZeroImageWritesNothing(t *testing.T) {
	w := &recordingWriter{}
	if _, err := copySkippingZeros(context.Background(), w, bytes.NewReader(make([]byte, 4*blk)), 4*blk); err != nil {
		t.Fatal(err)
	}
	if len(w.offsets) != 0 {
		t.Errorf("an all-zero image wrote %d blocks", len(w.offsets))
	}
}

// The tail of an image that is not block-aligned must still be written when it
// holds data — a short final read is the last partial block, not the end.
func TestCopySkippingZeros_WritesAShortFinalBlock(t *testing.T) {
	img := make([]byte, 2*blk+10)
	copy(img[2*blk:], []byte("tail"))
	w := &recordingWriter{}
	if _, err := copySkippingZeros(context.Background(), w, bytes.NewReader(img), int64(len(img))); err != nil {
		t.Fatal(err)
	}
	if len(w.offsets) != 1 || w.offsets[0] != 2*blk {
		t.Fatalf("wrote at %v, want only the short tail block at %d", w.offsets, 2*blk)
	}
	if !bytes.Equal(w.buf[2*blk:2*blk+10], img[2*blk:]) {
		t.Error("the short tail was not written correctly")
	}
}

// An image longer than it was declared would write past the end of the base
// device. Refuse rather than truncate: a truncated disk image boots, sometimes.
func TestCopySkippingZeros_RefusesAnImageLargerThanDeclared(t *testing.T) {
	img := bytes.Repeat([]byte{1}, 3*blk)
	_, err := copySkippingZeros(context.Background(), &recordingWriter{}, bytes.NewReader(img), 2*blk)
	if err == nil {
		t.Fatal("an image larger than its declared size was accepted")
	}
	if !strings.Contains(err.Error(), "past its declared") {
		t.Errorf("error should say the image overran: %v", err)
	}
}

func TestCopySkippingZeros_HonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copySkippingZeros(ctx, &recordingWriter{}, bytes.NewReader(make([]byte, blk)), blk); err == nil {
		t.Fatal("a cancelled copy ran anyway")
	}
}

// A base is snapshotted from only once it is marked ready, so marking has to
// require an allocation — and forgetting a base has to take its readiness with
// it, or a base re-allocated later for the same key is born "ready" and
// snapshotted before a byte is written.
func TestRegistry_ReadinessFollowsTheBase(t *testing.T) {
	r := tmpRegistry(t)

	if err := r.MarkBaseReady("sha256:never-allocated"); err == nil {
		t.Error("an unallocated base was marked ready")
	}
	if _, _, err := r.AllocateBase("sha256:x"); err != nil {
		t.Fatal(err)
	}
	if ready, _ := r.BaseReady("sha256:x"); ready {
		t.Error("a freshly allocated base reports ready before anything was written")
	}
	if err := r.MarkBaseReady("sha256:x"); err != nil {
		t.Fatal(err)
	}
	if ready, _ := r.BaseReady("sha256:x"); !ready {
		t.Error("MarkBaseReady did not stick")
	}
	if err := r.ForgetBase("sha256:x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.AllocateBase("sha256:x"); err != nil {
		t.Fatal(err)
	}
	if ready, _ := r.BaseReady("sha256:x"); ready {
		t.Error("a re-allocated base inherited readiness from its forgotten predecessor")
	}
}

// Keys may carry ':' and '/', which device-mapper names do not want.
func TestBaseDeviceName_IsSafeAndStable(t *testing.T) {
	a := baseDeviceName("sha256:abc/def")
	if a != baseDeviceName("sha256:abc/def") {
		t.Error("the same key produced two device names")
	}
	if a == baseDeviceName("sha256:abc/deg") {
		t.Error("two keys produced one device name")
	}
	if strings.ContainsAny(a, ":/ ") {
		t.Errorf("device name %q carries characters device-mapper names reject", a)
	}
}
