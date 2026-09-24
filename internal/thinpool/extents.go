package thinpool

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// PoolBytesForFile bounds the pool space writing the image at path into a
// base will allocate: the pool blocks its data extents touch.
//
// populate writes only pool blocks holding a non-zero byte, and every such
// byte lies in a data extent of the file, so this is an upper bound -- never
// an underestimate, which could fill the pool mid-write and stall every guest
// on the node. A mostly-empty raw image costs a fraction of its size; asking
// for the full size refused bases that fit and evicted others for nothing.
//
// Falls back to sizeBytes, the old bound, where the filesystem cannot report
// extents.
func PoolBytesForFile(path string, sizeBytes uint64) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	const block = BlockSectors * 512
	fd := int(f.Fd())
	var blocks uint64
	lastCounted := int64(-1) // index of the last block counted
	for off := int64(0); ; {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // no data past off
		}
		if err != nil {
			return sizeBytes, nil // extents not reported here
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return sizeBytes, nil
		}
		first, last := data/block, (hole-1)/block
		if first <= lastCounted {
			first = lastCounted + 1
		}
		if last >= first {
			blocks += uint64(last - first + 1)
			lastCounted = last
		}
		off = hole
	}
	need := blocks * block
	if need > sizeBytes {
		need = sizeBytes
	}
	return need, nil
}
