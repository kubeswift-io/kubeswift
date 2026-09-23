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

// freeAndTotal reports the free and total bytes of the filesystem holding dir.
func freeAndTotal(dir string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	// Bavail, not Bfree: the blocks an unprivileged writer could actually use.
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
