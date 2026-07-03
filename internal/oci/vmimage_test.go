package oci

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/content/oci"
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
	_, stats, err := ChunkAndPush(context.Background(), src, store, "v1", testChunk, "raw", "linux")
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
	if _, err := PullAndReassemble(context.Background(), store, "v1", dst); err != nil {
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
	if _, _, err := ChunkAndPush(context.Background(), v1, store, "v1", testChunk, "raw", "linux"); err != nil {
		t.Fatal(err)
	}

	// v1.1: same layout, but the middle data chunk changed in place.
	bChanged := bytes.Repeat([]byte{0x12}, testChunk)
	v2 := filepath.Join(dir, "v2.raw")
	writeDisk(t, v2, [][]byte{a, zero, bChanged, zero, zero, short})
	_, stats2, err := ChunkAndPush(context.Background(), v2, store, "v11", testChunk, "raw", "linux")
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
	if _, err := PullAndReassemble(context.Background(), store, "v11", out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	orig, _ := os.ReadFile(v2)
	if !bytes.Equal(got, orig) {
		t.Error("v1.1 reassembled disk differs from original")
	}
}

// A chunk larger than oras's 32 MiB content.FetchAll cap must still round-trip
// (regression for the streaming fix — FetchAll refuses >32 MiB blobs, so the
// download path streams instead). Uses a 33 MiB chunk = one layer.
func TestChunkLargerThan32MiB_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	const big = 33 * 1024 * 1024 // > maxDescriptorSize (32 MiB)
	data := make([]byte, big)
	for i := range data {
		data[i] = byte(i % 251) // non-zero, non-uniform
	}
	src := filepath.Join(dir, "big.raw")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Disk-backed store: memory.Store caps blobs at 32 MiB (like content.FetchAll),
	// but a real registry does not — oci.Store models that.
	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, stats, err := ChunkAndPush(context.Background(), src, store, "big", big, "raw", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if stats.TransferredBytes != int64(big) {
		t.Errorf("transferred = %d, want %d", stats.TransferredBytes, big)
	}
	out := filepath.Join(dir, "big-out.raw")
	if _, err := PullAndReassemble(context.Background(), store, "big", out); err != nil {
		t.Fatalf("reassemble of a >32MiB chunk failed (FetchAll cap regression?): %v", err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, data) {
		t.Error(">32MiB chunk did not round-trip byte-identical")
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
	_, stats, err := ChunkAndPush(context.Background(), src, store, "z", testChunk, "raw", "linux")
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
	if _, err := PullAndReassemble(context.Background(), store, "z", out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, make([]byte, 3*testChunk)) {
		t.Error("all-zero reassembly is not all-zero / wrong size")
	}
}

// PullAndReassemble opens the destination without O_TRUNC (os.Create implies it)
// so a Block-mode PVC — where Truncate() returns EINVAL — is never truncated.
// Guard the regular-file consequence: pulling over a PRE-EXISTING, LARGER stale
// destination still yields byte-identical content (the explicit Truncate to
// TotalSize discards the stale tail). This is the file-path half of the
// Block-device fix (the block half is cluster-validated — no loop device in CI).
func TestPull_OverExistingStaleDestination_ByteIdentical(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "image.raw")
	a, b, _, zero := mkWindows()
	writeDisk(t, src, [][]byte{a, zero, b}) // 3 windows, middle zero-skipped
	orig, _ := os.ReadFile(src)

	store := memory.New()
	if _, _, err := ChunkAndPush(context.Background(), src, store, "v1", testChunk, "raw", "linux"); err != nil {
		t.Fatal(err)
	}

	// Pre-create the destination LARGER than the disk with stale non-zero bytes.
	dst := filepath.Join(dir, "out.raw")
	if err := os.WriteFile(dst, bytes.Repeat([]byte{0x99}, 5*testChunk), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PullAndReassemble(context.Background(), store, "v1", dst); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, orig) {
		t.Errorf("pull over a stale destination must be byte-identical to source (len got=%d orig=%d); the stale tail must be truncated away", len(got), len(orig))
	}
}
