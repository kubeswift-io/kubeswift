package thinpool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	// BlockSectors is the pool's allocation block, in 512-byte sectors.
	// 128 sectors = 64 KiB.
	//
	// This is the granularity of both sharing and copy-on-write, and it is
	// fixed for the life of a pool — it cannot be changed without destroying
	// and rebuilding it. 64 KiB is what every measurement in the design doc
	// was taken at: a 1 MiB guest write allocated exactly 1 MiB (16 blocks,
	// no amplification), and metadata came to 48 bytes per mapping.
	BlockSectors = 128

	// MetadataBytesPerMapping is what one mapping costs in the metadata
	// device, measured rather than quoted: 256 mappings cost exactly 3
	// metadata blocks, linear across 20 guests.
	//
	// Dense sequential writes pack better (~17 B) and scattered writes are far
	// worse (a leaf node per touched region, ~4 KiB), so this is a sizing
	// floor, not a budget.
	MetadataBytesPerMapping = 48

	// lowWaterBlocks is the free-space threshold at which the kernel raises a
	// dm event. Zero disables it; the pool then reaches out_of_data_space with
	// no prior notice.
	lowWaterBlocks = 16384 // 1 GiB at 64 KiB blocks
)

// MetadataSectorsFor returns a metadata device size for a pool holding
// dataBytes of allocated data, in 512-byte sectors.
//
// At 48 B/mapping and 64 KiB blocks this is ~0.75 MiB of metadata per GiB of
// data — about 0.07% — so it is sized generously on purpose. Metadata
// exhaustion has no grace window and no warning state (see ModeReadOnly), and
// the cost of over-provisioning it is nil.
func MetadataSectorsFor(dataBytes uint64) uint64 {
	mappings := dataBytes / (BlockSectors * 512)
	bytes := mappings * MetadataBytesPerMapping
	// Double it, then floor at 64 MiB. The doubling covers scattered-write
	// workloads, which cost far more per mapping than the sequential case the
	// constant is derived from.
	bytes = bytes*2 + 64*1024*1024
	return (bytes + 511) / 512
}

// Runner executes device-mapper commands. Injected so the command construction
// can be tested without device-mapper, and so the integration test can run the
// real thing.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execRunner struct{}

// ExecRunner runs commands for real. Backing.Resolve takes a Runner explicitly
// (unlike Manager, which defaults to this), so callers outside the package need
// a way to name it.
func ExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// The C locale, so an error reads the same on every node: DeleteThin
	// recognises one by its text (see ErrNoSuchThin).
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Manager drives one node's thin pool.
type Manager struct {
	// Pool is the device-mapper name of the pool (e.g. "kubeswift-pool").
	Pool string
	// DataDev and MetaDev are the backing devices.
	//
	// These must be real block devices or LVs. A sparse file "works" and will
	// mislead you: discard passdown punches holes in it, so freed blocks read
	// back as zeros and a block-reuse leak that a real device would expose
	// stays invisible. That mistake is written up in the design doc §7.8.
	DataDev, MetaDev string

	// Run defaults to exec.
	Run Runner
}

func (m *Manager) runner() Runner {
	if m.Run != nil {
		return m.Run
	}
	return execRunner{}
}

// dmsetup runs a dmsetup subcommand with udev synchronisation DISABLED.
//
// --noudevsync is not a tuning flag, it is the difference between working and
// hanging forever. dmsetup creates a semaphore and waits for udev to signal
// that it has finished processing the new device. Inside a container there is
// no udev to answer, so the call blocks in __do_semtimedop and never returns —
// observed on a dev node as `dmsetup create` wedged indefinitely while the
// kernel-side device had in fact been created, leaving a pool nothing would
// clean up.
//
// Every launcher and node component here runs in a pod, so this applies to all
// of them, always. The cost is that /dev/mapper nodes are created by
// device-mapper itself rather than by udev rules, which is what we want on a
// node whose udev knows nothing about these devices anyway.
func (m *Manager) dmsetup(ctx context.Context, args ...string) (string, error) {
	return m.runner().Run(ctx, "dmsetup", append([]string{"--noudevsync"}, args...)...)
}

// poolTable builds the thin-pool table line.
//
// The feature arguments are the settled ones and each is load-bearing:
//
//   - block zeroing is ON. `skip_block_zeroing` hands a newly provisioned block
//     to a guest still holding the previous owner's bytes — measured at 3824
//     occurrences of a prior tenant's pattern in a single 64 KiB block. It costs
//     ~6% on first-touch partial-block writes and nothing on full-block writes.
//     Do not add it back for throughput.
//   - queue_if_no_space is the default and stays. It gives a full data device a
//     60 s grace window in which growing the pool loses nothing, and makes the
//     guest see ENOSPC rather than EIO if the window closes.
//   - discard passdown is left at its default (on). It is what makes a deleted
//     guest's space actually return to the pool, and it is why dm-thin was
//     chosen over dm-snapshot.
func (m *Manager) poolTable(lengthSectors uint64) string {
	return fmt.Sprintf("0 %d thin-pool %s %s %d %d",
		lengthSectors, m.MetaDev, m.DataDev, BlockSectors, lowWaterBlocks)
}

// EnsurePool creates the pool if it does not already exist. Idempotent.
func (m *Manager) EnsurePool(ctx context.Context, lengthSectors uint64) error {
	if ok, err := m.exists(ctx, m.Pool); err != nil {
		return err
	} else if ok {
		return nil
	}
	_, err := m.dmsetup(ctx, "create", m.Pool, "--table", m.poolTable(lengthSectors))
	return err
}

// Status reads and parses the pool's status.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	out, err := m.dmsetup(ctx, "status", m.Pool)
	if err != nil {
		return Status{}, err
	}
	return ParseStatus(out)
}

// CreateBase provisions an empty thin volume for a base image and activates it
// at name, so the image can be written into it.
//
// The caller writes the image and then calls RemoveDevice: a base's device node
// is only needed while populating it. Guests derived from it do not reference
// the device — see SnapshotBase.
func (m *Manager) CreateBase(ctx context.Context, deviceID uint32, name string, sizeSectors uint64) error {
	if _, err := m.dmsetup(ctx, "message", m.Pool, "0",
		fmt.Sprintf("create_thin %d", deviceID)); err != nil {
		return fmt.Errorf("create_thin %d: %w", deviceID, err)
	}
	return m.activate(ctx, name, deviceID, sizeSectors)
}

// SnapshotBase creates a per-guest thin snapshot of a populated base and
// activates it at name.
//
// sizeSectors is the GUEST's requested size and is deliberately independent of
// the base's. A thin device's length is whatever its table says: presenting a
// snapshot larger than its origin is free until written, and the guest then
// sees the image at the front followed by zeros — exactly what a grown raw disk
// looks like today, so the existing clone-grow step applies unchanged.
//
// The origin is NOT suspended. It does not need to be: a base is written once
// and then its device is removed, so there is no active origin to quiesce and
// no per-guest serialisation point across the node. Suspending a shared origin
// for every guest creation would be exactly that.
func (m *Manager) SnapshotBase(ctx context.Context, baseID, deviceID uint32, name string, sizeSectors uint64) error {
	if _, err := m.dmsetup(ctx, "message", m.Pool, "0",
		fmt.Sprintf("create_snap %d %d", deviceID, baseID)); err != nil {
		return fmt.Errorf("create_snap %d from %d: %w", deviceID, baseID, err)
	}
	return m.activate(ctx, name, deviceID, sizeSectors)
}

func (m *Manager) activate(ctx context.Context, name string, deviceID uint32, sizeSectors uint64) error {
	pool, err := m.poolDevRef(ctx)
	if err != nil {
		return err
	}
	table := fmt.Sprintf("0 %d thin %s %d", sizeSectors, pool, deviceID)
	if _, err = m.dmsetup(ctx, "create", name, "--table", table); err != nil {
		return err
	}
	// With udev sync disabled nothing creates /dev/mapper/<name>, and the
	// launcher needs a real path to hand Cloud Hypervisor (--disk path=...).
	// mknodes is the explicit form of what udev would have done.
	_, err = m.dmsetup(ctx, "mknodes", name)
	return err
}

// Activate maps an EXISTING thin device at name, without creating anything in
// the pool.
//
// This is how a guest comes back after its launcher restarts or its node
// reboots. It must never be SnapshotBase: the guest's thin device holds every
// write it has made, and taking a fresh snapshot of the base would hand it a
// pristine disk — its data silently gone, and the guest booting as if new.
func (m *Manager) Activate(ctx context.Context, deviceID uint32, name string, sizeSectors uint64) error {
	return m.activate(ctx, name, deviceID, sizeSectors)
}

// Active reports whether a device-mapper device is currently mapped.
func (m *Manager) Active(ctx context.Context, name string) (bool, error) {
	return m.exists(ctx, name)
}

// DevicePath is where an activated device appears. Valid only after the device
// has been created, because the node is made by mknodes rather than by udev.
func (m *Manager) DevicePath(name string) string { return "/dev/mapper/" + name }

// poolDevRef returns the pool as "major:minor".
//
// Device-mapper accepts either a path or major:minor in a table, and this
// deliberately avoids the path. With --noudevsync (which is mandatory in a
// container, see dmsetup) nothing creates /dev/mapper/<name>, so a table
// referring to that path fails with "not found" on a pool that exists and is
// perfectly healthy. major:minor is what the kernel wants anyway.
func (m *Manager) poolDevRef(ctx context.Context) (string, error) {
	out, err := m.dmsetup(ctx, "info", "-c", "--noheadings", "-o", "major,minor", m.Pool)
	if err != nil {
		return "", fmt.Errorf("resolving pool %s: %w", m.Pool, err)
	}
	ref := strings.TrimSpace(out)
	if ref == "" || !strings.Contains(ref, ":") {
		return "", fmt.Errorf("pool %s: unexpected major,minor %q", m.Pool, ref)
	}
	return ref, nil
}

// RemoveDevice deactivates a thin device's node. The data is untouched; only
// the mapping goes away.
//
// --retry is not optional. A remove issued straight after writing to the device
// fails with EBUSY while udev still holds it open, and the window is short
// enough to look like flakiness: in one integration run this hit exactly one of
// the four tests that all write-then-remove, and passed the other three.
// dmsetup's own retry loop is the fix rather than a sleep, because the
// condition is "udev has let go", not "some duration has passed".
//
// The device's /dev/mapper node goes with it. activate made that node itself
// (mknodes), not udev, so udev does not remove it either: measured, it outlives
// the device, left pointing at a device number the kernel will give to the next
// device it maps. mknodes for a name that is no longer mapped removes the node.
func (m *Manager) RemoveDevice(ctx context.Context, name string) error {
	if _, err := m.dmsetup(ctx, "remove", "--retry", name); err != nil {
		return err
	}
	_, err := m.dmsetup(ctx, "mknodes", name)
	return err
}

// DeleteThin destroys a thin device inside the pool, freeing every block
// nothing else references.
//
// Safe to call on a base that guests derive from. dm-thin reference-counts:
// deleting a base leaves its snapshots byte-identical and writable, and returns
// only the blocks no snapshot still points at. A base is a convenience for
// creating the NEXT guest, not a dependency of the running ones — which is why
// eviction needs no policy beyond "oldest unused first".
//
// An id the pool does not hold is ErrNoSuchThin, so a caller finishing an
// interrupted delete can tell "already gone" from a real failure.
func (m *Manager) DeleteThin(ctx context.Context, deviceID uint32) error {
	out, err := m.dmsetup(ctx, "message", m.Pool, "0",
		fmt.Sprintf("delete %d", deviceID))
	if err != nil && strings.Contains(out, "No data available") {
		return fmt.Errorf("%w: id %d: %w", ErrNoSuchThin, deviceID, err)
	}
	return err
}

// ErrNoSuchThin is DeleteThin's error for an id the pool does not hold.
//
// dm-thin answers a delete of an unknown id with ENODATA, which dmsetup reports
// only as text ("No data available"). ExecRunner pins the C locale so that
// text is stable.
var ErrNoSuchThin = errors.New("the pool holds no thin device with that id")

// GrowData reloads the pool at a larger length after the data device has been
// extended.
//
// This is the recovery for ModeOutOfDataSpace, and inside no_space_timeout it
// loses nothing: queued writes complete against the new space.
func (m *Manager) GrowData(ctx context.Context, newLengthSectors uint64) error {
	return m.reload(ctx, newLengthSectors)
}

// GrowMetadata reloads the pool after the metadata device has been extended.
//
// This is the recovery for ModeReadOnly, and it works online — measured,
// returning the pool to rw with writes succeeding immediately, no offline
// thin_repair. The table is unchanged; the reload is what makes the kernel
// re-read the metadata device's new size.
func (m *Manager) GrowMetadata(ctx context.Context, lengthSectors uint64) error {
	return m.reload(ctx, lengthSectors)
}

func (m *Manager) reload(ctx context.Context, lengthSectors uint64) error {
	if _, err := m.dmsetup(ctx, "suspend", m.Pool); err != nil {
		return err
	}
	if _, err := m.dmsetup(ctx, "reload", m.Pool, "--table", m.poolTable(lengthSectors)); err != nil {
		// Leaving the pool suspended would wedge every guest on the node, so
		// resume even on a failed reload: the old table is still loaded and
		// serving.
		_, _ = m.dmsetup(ctx, "resume", m.Pool)
		return err
	}
	_, err := m.dmsetup(ctx, "resume", m.Pool)
	return err
}

// exists reports whether a device-mapper device is active.
//
// It lists devices rather than asking about one, because `dmsetup info <name>`
// exits non-zero for a device that is merely absent and the only way to tell
// that apart from a real failure is its English message — which is "Device does
// not exist.", not the "not found" an earlier version of this guessed at. That
// guess made EnsurePool treat a missing pool as an error and never create it.
// Matching prose from a tool is a bug waiting for a locale or a version bump;
// `dmsetup ls` exits 0 either way and absence is simply a line that is not
// there.
func (m *Manager) exists(ctx context.Context, name string) (bool, error) {
	out, err := m.dmsetup(ctx, "ls")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		// "<name>\t(major:minor)"
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return true, nil
		}
	}
	return false, nil
}
