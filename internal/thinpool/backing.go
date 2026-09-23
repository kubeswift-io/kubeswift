package thinpool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backing describes where a node's pool keeps its bytes.
//
// Exactly one shape is used per device. A block device or LV is the production
// shape — no loop layer, no second filesystem underneath, and discard reaches
// the hardware. A preallocated file is the fallback, because a spare device per
// node is an ask many clusters cannot meet; it costs about 18% and is otherwise
// correct, PROVIDED the rules in EnsureLoop are followed.
type Backing struct {
	// Device is an existing block device or LV. Takes precedence over File.
	Device string
	// File is a path to a backing file, created preallocated at FileBytes if
	// absent and attached to a loop device.
	File string
	// FileBytes is the size to preallocate. Ignored when Device is set.
	FileBytes uint64
}

// Resolve returns the block device path to hand device-mapper, attaching a loop
// device first when this is file-backed.
func (b Backing) Resolve(ctx context.Context, r Runner) (string, error) {
	if b.Device != "" {
		if err := requireBlockDevice(b.Device); err != nil {
			return "", err
		}
		return b.Device, nil
	}
	if b.File == "" {
		return "", fmt.Errorf("thinpool backing: neither Device nor File is set")
	}
	if err := ensurePreallocated(b.File, b.FileBytes); err != nil {
		return "", err
	}
	return EnsureLoop(ctx, r, b.File)
}

// EnsureLoop attaches path to a loop device with direct I/O enabled, reusing an
// existing attachment if there is one, and VERIFIES that direct I/O actually
// took effect.
//
// --direct-io=on is not a performance flag. A loop device writes through the
// host page cache by default, so a guest issuing an O_DIRECT write — a database
// commit, a journal flush — gets completion back while the bytes are still in
// host RAM, and a node crash loses writes the guest was told were durable. It
// measures as a 2.8x speedup (1405 MB/s against 494 MB/s on the same file),
// which is what makes it dangerous: the fast number looks like good news.
//
// The verification matters as much as the flag. losetup accepts --direct-io=on
// and can still leave DIO off — the backing filesystem may not support it —
// and a silent downgrade here is silent data loss. So the value is read back
// and a mismatch is an error, not a warning.
func EnsureLoop(ctx context.Context, r Runner, path string) (string, error) {
	if dev, err := existingLoop(ctx, r, path); err != nil {
		return "", err
	} else if dev != "" {
		if err := verifyDirectIO(ctx, r, dev); err != nil {
			return "", err
		}
		return dev, nil
	}

	out, err := r.Run(ctx, "losetup", "--find", "--show", "--direct-io=on", path)
	if err != nil {
		return "", fmt.Errorf("attaching %s to a loop device: %w", path, err)
	}
	dev := strings.TrimSpace(out)
	if dev == "" {
		return "", fmt.Errorf("losetup attached %s but reported no device", path)
	}
	if err := verifyDirectIO(ctx, r, dev); err != nil {
		// Leave nothing half-configured: a loop device with the page cache in
		// front of it is worse than no pool at all, because it works.
		_, _ = r.Run(ctx, "losetup", "-d", dev)
		return "", err
	}
	return dev, nil
}

// verifyDirectIO fails unless the loop device reports DIO=1.
func verifyDirectIO(ctx context.Context, r Runner, dev string) error {
	out, err := r.Run(ctx, "losetup", "-l", "--noheadings", "-O", "DIO", dev)
	if err != nil {
		return fmt.Errorf("reading direct-io state of %s: %w", dev, err)
	}
	switch strings.TrimSpace(out) {
	case "1":
		return nil
	case "0":
		return fmt.Errorf(
			"loop device %s has direct I/O DISABLED; writes would be acknowledged from the host page "+
				"cache and lost on a node crash. Use a block device for the pool, or a filesystem that "+
				"supports O_DIRECT", dev)
	default:
		return fmt.Errorf("loop device %s reported an unreadable direct-io state %q; refusing to "+
			"assume it is enabled", dev, strings.TrimSpace(out))
	}
}

// existingLoop returns the loop device already backing path, or "".
func existingLoop(ctx context.Context, r Runner, path string) (string, error) {
	out, err := r.Run(ctx, "losetup", "-j", path, "--noheadings", "-O", "NAME")
	if err != nil {
		// losetup -j on an unattached file is not an error on any version seen,
		// but treat a failure as "not attached" only when it printed nothing:
		// a real failure with output is a failure.
		if strings.TrimSpace(out) == "" {
			return "", nil
		}
		return "", fmt.Errorf("looking up a loop device for %s: %w", path, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			return f[0], nil
		}
	}
	return "", nil
}

// evictionHeadroom is the fraction of a filesystem a node must keep free.
//
// The kubelet evicts pods below 10% free on the filesystem it runs from
// (nodefs.available<10%, its default), and a pool file is preallocated in one
// go — so a pool sized to "what is free" does not fill the disk slowly, it puts
// the node over the line at once, and everything else on it starts being
// evicted. Refusing is the kinder failure: it costs this guest, not the node.
const evictionHeadroom = 10

// roomFor refuses to preallocate size bytes in dir when doing so would leave
// the filesystem under the eviction headroom.
func roomFor(dir string, size uint64) error {
	free, total, err := freeAndTotal(dir)
	if err != nil {
		return fmt.Errorf("checking the space in %s: %w", dir, err)
	}
	keep := total / evictionHeadroom
	if free < size+keep {
		return fmt.Errorf("a pool of %s does not fit in %s: %s free of %s, and a node must keep %s "+
			"(%d%%) free or the kubelet starts evicting pods. Use a node with more room, or a smaller pool size",
			human(size), dir, human(free), human(total), human(keep), evictionHeadroom)
	}
	return nil
}

// human renders bytes as TiB/GiB/MiB, for messages an operator reads.
func human(b uint64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1f TiB", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", b)
}

// ensurePreallocated creates path at size bytes if absent, fully allocated.
//
// PREALLOCATED, never sparse. A sparse backing file makes it impossible to
// reason about block reuse: discard passdown punches holes in it, so freed
// blocks read back as zeros from the FILE rather than from any guarantee
// device-mapper is making, and a real leak hides behind a reassuring test
// result. It also lets the pool believe it has space the filesystem cannot
// honour, turning a disk-full event into guests taking EIO.
func ensurePreallocated(path string, size uint64) error {
	if size == 0 {
		return fmt.Errorf("thinpool backing file %s: size must be set", path)
	}
	if st, err := os.Stat(path); err == nil {
		if st.Size() < int64(size) {
			return fmt.Errorf("thinpool backing file %s is %d bytes, smaller than the configured %d; "+
				"grow it deliberately rather than letting the pool shrink under running guests",
				path, st.Size(), size)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := roomFor(filepath.Dir(path), size); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()
	// Fallocate, not Truncate: Truncate produces a sparse file, which is the
	// shape this must never be.
	if err := fallocate(f, int64(size)); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("preallocating %s to %d bytes: %w", path, size, err)
	}
	return f.Sync()
}

// requireBlockDevice rejects anything that is not one, rather than letting
// device-mapper fail later with a less specific message.
func requireBlockDevice(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("thinpool backing device %s: %w", path, err)
	}
	if st.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("thinpool backing device %s is not a block device", path)
	}
	return nil
}
