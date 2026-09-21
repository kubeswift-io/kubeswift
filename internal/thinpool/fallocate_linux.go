package thinpool

import (
	"os"

	"golang.org/x/sys/unix"
)

// fallocate allocates size bytes to f without writing them, and without leaving
// holes.
//
// mode 0 is "allocate and do not mark unwritten" — the file reads as zeros but
// the blocks are really reserved, which is the whole point: a sparse file lets
// the pool believe it has space the filesystem cannot honour, and hides block
// reuse behind discard-punched holes (see ensurePreallocated).
func fallocate(f *os.File, size int64) error {
	return unix.Fallocate(int(f.Fd()), 0, 0, size)
}
