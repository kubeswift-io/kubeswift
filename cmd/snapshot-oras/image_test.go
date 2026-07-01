package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"oras.land/oras-go/v2/content/memory"
)

func writeDisk(t *testing.T, path string, windows [][]byte) {
	t.Helper()
	var buf bytes.Buffer
	for _, w := range windows {
		buf.Write(w)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

const testChunk = 4096

func mkWindows() (a, b, short, zero []byte) {
	return bytes.Repeat([]byte{0xAB}, testChunk),
		bytes.Repeat([]byte{0xCD}, testChunk),
		bytes.Repeat([]byte{0xEF}, 100),
		make([]byte, testChunk)
}

// A disk of 6 windows: data, zero, data, zero, zero, short-data. 3 non-zero
// chunks stored; 3 zero windows skipped; total = 5*chunk + 100.
func TestChunkRoundTrip_ByteIdentical_ZeroSkip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "image.raw")
	a, b, short, zero := mkWindows()
	writeDisk(t, src, [][]byte{a, zero, b, zero, zero, short})
	orig, _ := os.ReadFile(src)

	store := memory.New()
	_, stats, err := chunkAndPush(context.Background(), src, store, "v1", testChunk, "raw", "linux")
	if err != nil {
		t.Fatal(err)
	}
	wantTransferred := int64(2*testChunk + 100) // a + b + short; zeros skipped
	wantTotal := int64(5*testChunk + 100)
	if stats.TransferredBytes != wantTransferred {
		t.Errorf("transferred = %d, want %d (zero windows must not be pushed)", stats.TransferredBytes, wantTransferred)
	}
	if stats.SkippedBytes != 0 {
		t.Errorf("skipped = %d, want 0 on a fresh store", stats.SkippedBytes)
	}
	if stats.TotalBytes != wantTotal {
		t.Errorf("total = %d, want %d", stats.TotalBytes, wantTotal)
	}

	dst := filepath.Join(dir, "out.raw")
	if _, err := pullAndReassemble(context.Background(), store, "v1", dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, orig) {
		t.Errorf("reassembled disk differs from original (len got=%d orig=%d)", len(got), len(orig))
	}
}

// Re-pushing a v1.1 that changed ONE chunk transfers only that chunk; the
// unchanged chunks (same digest) are deduped as skipped. This is the ~97%
// cross-version dedup in miniature.
func TestChunkDedup_ChangedChunkOnly(t *testing.T) {
	dir := t.TempDir()
	a, b, short, zero := mkWindows()
	store := memory.New()

	v1 := filepath.Join(dir, "v1.raw")
	writeDisk(t, v1, [][]byte{a, zero, b, zero, zero, short})
	if _, _, err := chunkAndPush(context.Background(), v1, store, "v1", testChunk, "raw", "linux"); err != nil {
		t.Fatal(err)
	}

	// v1.1: same layout, but the middle data chunk changed in place.
	bChanged := bytes.Repeat([]byte{0x12}, testChunk)
	v2 := filepath.Join(dir, "v2.raw")
	writeDisk(t, v2, [][]byte{a, zero, bChanged, zero, zero, short})
	_, stats2, err := chunkAndPush(context.Background(), v2, store, "v11", testChunk, "raw", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if stats2.TransferredBytes != int64(testChunk) {
		t.Errorf("v1.1 transferred = %d, want %d (only the one changed chunk)", stats2.TransferredBytes, testChunk)
	}
	if stats2.SkippedBytes != int64(testChunk+100) {
		t.Errorf("v1.1 skipped = %d, want %d (a + short deduped)", stats2.SkippedBytes, testChunk+100)
	}

	// v1.1 still reassembles correctly.
	out := filepath.Join(dir, "v2out.raw")
	if _, err := pullAndReassemble(context.Background(), store, "v11", out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	orig, _ := os.ReadFile(v2)
	if !bytes.Equal(got, orig) {
		t.Error("v1.1 reassembled disk differs from original")
	}
}

// An all-zero disk stores nothing but still round-trips to the right size.
func TestChunkAllZero_NoLayers(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sparse.raw")
	if err := os.WriteFile(src, make([]byte, 3*testChunk), 0o644); err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	_, stats, err := chunkAndPush(context.Background(), src, store, "z", testChunk, "raw", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if stats.TransferredBytes != 0 {
		t.Errorf("all-zero disk transferred = %d, want 0", stats.TransferredBytes)
	}
	if stats.TotalBytes != int64(3*testChunk) {
		t.Errorf("total = %d, want %d", stats.TotalBytes, 3*testChunk)
	}
	out := filepath.Join(dir, "zout.raw")
	if _, err := pullAndReassemble(context.Background(), store, "z", out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, make([]byte, 3*testChunk)) {
		t.Error("all-zero reassembly is not all-zero / wrong size")
	}
}
