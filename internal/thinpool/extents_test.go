package thinpool

import (
	"os"
	"path/filepath"
	"testing"
)

const poolBlock = BlockSectors * 512

// sparseImage is a size-byte file holding data only at the given offsets.
func sparseImage(t *testing.T, size int64, at ...int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "image.raw")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for _, off := range at {
		if _, err := f.WriteAt([]byte{0x5a}, off); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// A mostly-empty raw image needs the blocks its data touches, not its size.
func TestPoolBytesForFile_CountsOnlyBlocksWithData(t *testing.T) {
	size := int64(64 << 20)
	got, err := PoolBytesForFile(sparseImage(t, size, 5<<20+3), uint64(size))
	if err != nil {
		t.Fatal(err)
	}
	// The filesystem allocates at its own granularity (4 KiB here), which sits
	// inside one 64 KiB pool block.
	if got != poolBlock {
		t.Errorf("need = %d, want one pool block (%d)", got, poolBlock)
	}
}

// Data in two pool blocks costs two, and never more than the image's size.
func TestPoolBytesForFile_IsAnUpperBound(t *testing.T) {
	size := int64(8 << 20)
	got, err := PoolBytesForFile(sparseImage(t, size, 0, 3*poolBlock+10), uint64(size))
	if err != nil {
		t.Fatal(err)
	}
	if got != 2*poolBlock {
		t.Errorf("need = %d, want two pool blocks", got)
	}
	full := sparseImage(t, 1<<20)
	if err := os.WriteFile(full, make([]byte, 1<<20), 0o644); err != nil { // fully allocated
		t.Fatal(err)
	}
	if got, err := PoolBytesForFile(full, 1<<20); err != nil || got != 1<<20 {
		t.Errorf("fully allocated image: need = %d (%v), want its size", got, err)
	}
}
