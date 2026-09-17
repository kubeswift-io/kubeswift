//go:build unix

package oci

import (
	"os"
	"syscall"
	"testing"
)

// allocatedBytes reports the bytes a file actually occupies on disk, or -1 when
// the platform does not expose it.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return st.Blocks * 512
}
