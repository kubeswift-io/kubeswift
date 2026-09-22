package thinpool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Materializer gets an image into a node's pool once, and gives each guest a
// thin snapshot of it.
//
// Two properties carry the whole design, and each has a test that fails
// without it:
//
//   - A guest is never snapshotted from a base that is still being written.
//     Several guests start on a node at once, and a base is gigabytes; a second
//     guest seeing the base's id mid-copy would boot from half an image.
//   - A guest's disk is never re-snapshotted. On a launcher restart or a node
//     reboot its existing device is reactivated. A fresh snapshot of the base
//     would hand it a pristine disk: every write it had made, silently gone.
type Materializer struct {
	M   *Manager
	Reg *Registry
	// LockDir holds one lock file per base key.
	LockDir string
}

// maxIDRetries bounds recovery from a registry that has fallen behind the pool
// (see Registry.Advance). A handful of collisions means the registry was
// rebuilt; dozens means something is wrong, and looping would hide it.
const maxIDRetries = 8

// EnsureBase makes key's base present and FULLY written, populating it from
// open only if no completed base exists, and returns its device id.
//
// key is an opaque content identity. It must change whenever the content does:
// two different images under one key would give guests of the second the
// first's disk. What it is derived from is the caller's decision.
//
// Concurrent callers for the same key serialise on a per-key lock, so exactly
// one writes and the rest wait and then use what it wrote. Callers for
// different keys do not block each other.
func (x *Materializer) EnsureBase(ctx context.Context, key string, sizeBytes uint64, open func() (io.ReadCloser, error)) (uint32, error) {
	if key == "" {
		return 0, errors.New("thinpool: empty base key")
	}
	unlock, err := x.lockKey(key)
	if err != nil {
		return 0, err
	}
	defer unlock()

	// Under the per-key lock, "ready" cannot change underneath us.
	if ready, err := x.Reg.BaseReady(key); err != nil {
		return 0, err
	} else if ready {
		id, _, err := x.Reg.BaseID(key)
		return id, err
	}

	sectors := bytesToSectors(sizeBytes)
	name := baseDeviceName(key)

	id, err := x.createBase(ctx, key, name, sectors)
	if err != nil {
		return 0, err
	}
	// From here, a failure must leave the base UNmarked, so the next caller
	// rewrites it rather than snapshotting a partial one.
	writeErr := x.populate(ctx, name, sizeBytes, open)
	rmErr := x.M.RemoveDevice(ctx, name)
	if writeErr != nil {
		return 0, fmt.Errorf("writing base %s: %w", key, writeErr)
	}
	if rmErr != nil {
		return 0, fmt.Errorf("deactivating base %s after writing it: %w", key, rmErr)
	}
	if err := x.Reg.MarkBaseReady(key); err != nil {
		return 0, err
	}
	return id, nil
}

// createBase allocates a device for key and creates an empty thin volume.
//
// An id already allocated to this key but never marked ready is a previous
// population that did not finish — its writer crashed, was evicted, or timed
// out. Whatever it left is partial, so it is thrown away and the base is
// created afresh under the SAME id. Rewriting in place would also be correct
// for identical content, but "correct because the bytes happen to match" is
// not a property worth depending on.
func (x *Materializer) createBase(ctx context.Context, key, name string, sectors uint64) (uint32, error) {
	for attempt := 0; attempt < maxIDRetries; attempt++ {
		id, fresh, err := x.Reg.AllocateBase(key)
		if err != nil {
			return 0, err
		}
		if !fresh {
			_ = x.M.RemoveDevice(ctx, name) // a crashed writer may have left it mapped
			_ = x.M.DeleteThin(ctx, id)     // absent if the crash came before create_thin
		}
		err = x.M.CreateBase(ctx, id, name, sectors)
		if err == nil {
			return id, nil
		}
		if !isFileExists(err) {
			return 0, err
		}
		// The pool already holds this id and the registry did not know: the
		// registry was rebuilt. Skip the id rather than fail forever.
		if err := x.Reg.ForgetBase(key); err != nil {
			return 0, err
		}
		if err := x.Reg.Advance(id); err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("base %s: %d consecutive device ids already in the pool; the registry is badly out of step with it", key, maxIDRetries)
}

// populate copies the image into the base, skipping every pool block that is
// entirely zero.
//
// Skipping zeros is not only an optimisation. dm-thin allocates a block on any
// write, including a write of zeros, so a naive copy of a 10 GiB raw image
// provisions 10 GiB in the pool even when most of it is empty. An unwritten
// thin block reads as zeros — block zeroing is on (§7.8) — so a skipped block
// is byte-for-byte the same to the guest and costs nothing.
//
// Chunks are exactly one pool block and aligned to one, so a block with any
// non-zero byte in it is written whole and a block with none is never touched.
func (x *Materializer) populate(ctx context.Context, name string, sizeBytes uint64, open func() (io.ReadCloser, error)) error {
	src, err := open()
	if err != nil {
		return fmt.Errorf("opening the image: %w", err)
	}
	defer src.Close()

	dst, err := os.OpenFile(x.M.DevicePath(name), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer dst.Close()

	written, err := copySkippingZeros(ctx, dst, src, int64(sizeBytes))
	if err != nil {
		return err
	}
	if written > int64(sizeBytes) {
		return fmt.Errorf("image is larger than the %d bytes it was declared as", sizeBytes)
	}
	// Durable before it is marked ready. A base marked ready is one guests are
	// snapshotted from; marking it on the strength of the page cache would make
	// a crash between here and the flush a corrupt base that says it is fine.
	return dst.Sync()
}

// copySkippingZeros copies src to dst one pool block at a time, writing only
// blocks that contain a non-zero byte. It returns the bytes consumed from src.
func copySkippingZeros(ctx context.Context, dst io.WriterAt, src io.Reader, limit int64) (int64, error) {
	const chunk = BlockSectors * 512
	buf := make([]byte, chunk)
	zero := make([]byte, chunk)
	var off int64
	for {
		if err := ctx.Err(); err != nil {
			return off, err
		}
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			if off+int64(n) > limit {
				return off + int64(n), fmt.Errorf("image continues past its declared %d bytes", limit)
			}
			if !bytes.Equal(buf[:n], zero[:n]) {
				if _, err := dst.WriteAt(buf[:n], off); err != nil {
					return off, fmt.Errorf("writing at offset %d: %w", off, err)
				}
			}
			off += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			return off, nil
		}
		if rerr != nil {
			return off, rerr
		}
	}
}

// EnsureGuest returns the device path of a guest's disk, snapshotting the base
// the first time and only REACTIVATING it after that.
//
// Whether the guest already has a disk is asked FIRST, before anything about
// the base. Reactivating needs nothing from the base — dm-thin reference-counts
// the blocks they share — and a base may legitimately have been evicted since
// (§7.5). Checking the base first made every guest of an evicted base unable to
// restart, its disk intact and unused. Only a NEW guest needs a ready base.
func (x *Materializer) EnsureGuest(ctx context.Context, baseKey, guestKey, devName string, sizeBytes uint64) (string, error) {
	// Already mapped: the launcher container restarted without the node
	// rebooting. Nothing to do, and creating it again would fail anyway.
	if active, err := x.M.Active(ctx, devName); err != nil {
		return "", err
	} else if active {
		return x.M.DevicePath(devName), nil
	}

	sectors := bytesToSectors(sizeBytes)
	if id, known, err := x.Reg.GuestID(guestKey); err != nil {
		return "", err
	} else if known {
		// The guest has a disk. Reactivate it; never re-snapshot.
		//
		// If the pool no longer holds this id, this fails, and that is the
		// right outcome: the guest's data is gone, and quietly handing it a
		// new empty disk would make that look like a clean first boot.
		if err := x.M.Activate(ctx, id, devName, sectors); err != nil {
			return "", fmt.Errorf("reactivating the disk of guest %s (device %d): %w", guestKey, id, err)
		}
		return x.M.DevicePath(devName), nil
	}

	// A new guest: this is the only case that needs the base, and it must be
	// completely written before anything is snapshotted from it.
	ready, err := x.Reg.BaseReady(baseKey)
	if err != nil {
		return "", err
	}
	if !ready {
		return "", fmt.Errorf("base %s is not ready; it must be fully written before any guest is snapshotted from it", baseKey)
	}
	baseID, ok, err := x.Reg.BaseID(baseKey)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("base %s is marked ready but has no device id", baseKey)
	}

	for attempt := 0; attempt < maxIDRetries; attempt++ {
		id, fresh, err := x.Reg.AllocateGuest(guestKey)
		if err != nil {
			return "", err
		}
		if !fresh {
			// Allocated between the lookup above and here — another caller
			// raced us. Treat it exactly like a known guest.
			if err := x.M.Activate(ctx, id, devName, sectors); err != nil {
				return "", fmt.Errorf("reactivating the disk of guest %s (device %d): %w", guestKey, id, err)
			}
			return x.M.DevicePath(devName), nil
		}
		err = x.M.SnapshotBase(ctx, baseID, id, devName, sectors)
		if err == nil {
			return x.M.DevicePath(devName), nil
		}
		if !isFileExists(err) {
			return "", err
		}
		if err := x.Reg.Forget(guestKey); err != nil {
			return "", err
		}
		if err := x.Reg.Advance(id); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("guest %s: %d consecutive device ids already in the pool; the registry is badly out of step with it", guestKey, maxIDRetries)
}

// HasGuest reports whether this node already holds a disk for guestKey.
func (x *Materializer) HasGuest(guestKey string) (bool, error) {
	_, known, err := x.Reg.GuestID(guestKey)
	return known, err
}

// ReleaseGuest destroys a guest's disk and frees what it alone was using.
//
// Idempotent, so an interrupted release is finished by running it again. That
// is why the thin device is deleted BEFORE its mapping is forgotten: a release
// cut short between the two leaves the registry still naming the id, and the
// retry deletes it (an id already gone counts as deleted) and then forgets it.
// The other order leaks the device for good, because once the mapping is gone
// nothing names its id any more. The mapping can safely outlive its device for
// that moment: the key carries the guest's UID, so no other guest can ever
// look it up.
func (x *Materializer) ReleaseGuest(ctx context.Context, guestKey, devName string) error {
	id, known, err := x.Reg.GuestID(guestKey)
	if err != nil {
		return err
	}
	if active, err := x.M.Active(ctx, devName); err != nil {
		return err
	} else if active {
		if err := x.M.RemoveDevice(ctx, devName); err != nil {
			return fmt.Errorf("unmapping %s: %w", devName, err)
		}
	}
	if !known {
		return nil
	}
	// A read-only pool refuses to delete anything, and says so only as
	// "Operation not supported". Name the actual problem.
	st, err := x.M.Status(ctx)
	if err != nil {
		return fmt.Errorf("reading pool %s: %w", x.M.Pool, err)
	}
	if st.Mode == ModeReadOnly {
		return fmt.Errorf("pool %s is read-only (its metadata device is full, or it needs a check), and a "+
			"read-only pool cannot delete devices; grow the metadata device and reload the pool, then retry", x.M.Pool)
	}
	if err := x.M.DeleteThin(ctx, id); err != nil && !errors.Is(err, ErrNoSuchThin) {
		return fmt.Errorf("deleting the thin device of guest %s: %w", guestKey, err)
	}
	return x.Reg.Forget(guestKey)
}

// lockKey takes an exclusive per-key lock, returning its release.
func (x *Materializer) lockKey(key string) (func(), error) {
	if err := os.MkdirAll(x.LockDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", x.LockDir, err)
	}
	f, err := os.OpenFile(filepath.Join(x.LockDir, keyHash(key)+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// flock, not a marker file: the kernel drops it when the holder dies, so a
	// writer killed mid-copy cannot leave every later guest waiting forever.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking base %s: %w", key, err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// baseDeviceName is the transient mapping a base is written through. Derived
// from a hash because keys may contain characters device-mapper names cannot.
func baseDeviceName(key string) string { return "ks-base-" + keyHash(key)[:16] }

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func bytesToSectors(b uint64) uint64 { return (b + 511) / 512 }

// isFileExists recognises dm-thin refusing a device id the pool already holds.
func isFileExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "File exists")
}
