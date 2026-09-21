package thinpool

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These exercise the real device-mapper thin target rather than the command
// strings, because everything this package encodes is a claim about kernel
// behaviour and the unit tests cannot check any of it. They need root and loop
// devices, so they are opt-in:
//
//	sudo KUBESWIFT_THINPOOL_IT=1 go test ./internal/thinpool/ -run Integration -v
//
// They are NOT part of CI — a GitHub runner has no business creating device
// mapper targets — which is exactly why the properties they verify are also
// written down in docs/design/shared-base-root-disk.md. This is the executable
// copy.

func itEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBESWIFT_THINPOOL_IT") != "1" {
		t.Skip("set KUBESWIFT_THINPOOL_IT=1 (and run as root) for device-mapper integration tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("device-mapper integration tests need root")
	}
}

const (
	itDataSize = 512 << 20
	itMetaSize = 64 << 20
	sector     = 512
)

// rig is one pool on two loop devices, torn down whatever happens.
//
// The pool name is DETERMINISTIC (test name only) and the rig force-removes it
// before starting. Both halves matter, and the combination is deliberate:
//
//   - Without the force-remove, a teardown that loses a race leaves the pool
//     behind, the next run's EnsurePool adopts it, and the test fails on a
//     device id the PREVIOUS run created — pointing at create_thin rather than
//     at the stale pool. That bit twice.
//   - Making the name unique instead (a pid, say) looks like it fixes that and
//     makes it worse: an aborted run's pool can then never be matched by any
//     later run, so orphans accumulate on the node forever.
//
// Deterministic plus force-remove is self-healing: whatever the last run left,
// this one reclaims.
type rig struct {
	t    *testing.T
	pool string
	m    *Manager
	data string
	meta string
	devs []string // activated thin device names, removed in reverse
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()

	// The data device is deliberately PRE-FILLED with a recognisable pattern.
	// A zeroed (or sparse) backing device cannot tell a real zeroing guarantee
	// from an accident of the file system, and that mistake produced a
	// confident "no leak" result during the design work that was simply wrong.
	dataPath := filepath.Join(dir, "data.img")
	writePattern(t, dataPath, itDataSize, []byte("PRIOR-TENANT-BYTES!"))
	metaPath := filepath.Join(dir, "meta.img")
	if err := os.WriteFile(metaPath, make([]byte, itMetaSize), 0o600); err != nil {
		t.Fatal(err)
	}

	pool := "ksit-" + strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '-'
		}
		return r
	}, t.Name())
	// Defensive: never adopt a pool left by an aborted run. A crashed or
	// timed-out test cannot run its own cleanup, and inheriting its device ids
	// produces "File exists" from create_thin with nothing pointing at why.
	_ = exec.Command("dmsetup", "remove", "-f", "--noudevsync", pool).Run()
	r := &rig{t: t, pool: pool, data: losetup(t, dataPath), meta: losetup(t, metaPath)}
	r.m = &Manager{Pool: pool, DataDev: r.data, MetaDev: r.meta}
	t.Cleanup(r.teardown)

	if err := r.m.EnsurePool(context.Background(), itDataSize/sector); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	return r
}

// teardown passes --noudevsync for the same reason every call in the package
// does: without it dmsetup waits on a udev semaphore nothing in a container
// will ever signal. Missing it HERE hangs the test rather than the product,
// which is how it survived a local run and wedged on a node for 18 minutes
// with load average 0.10 — blocked, not slow.
func (r *rig) teardown() {
	for i := len(r.devs) - 1; i >= 0; i-- {
		_ = exec.Command("dmsetup", "remove", "--retry", "--noudevsync", r.devs[i]).Run()
	}
	_ = exec.Command("dmsetup", "remove", "-f", "--noudevsync", r.pool).Run()
	for _, d := range []string{r.data, r.meta} {
		if d != "" {
			_ = exec.Command("losetup", "-d", d).Run()
		}
	}
}

func (r *rig) track(name string) string { r.devs = append(r.devs, name); return "/dev/mapper/" + name }

// The core claim: a guest's device presents the base merged with its own
// writes, the base is untouched, and the guest may be larger than the base.
func TestIntegration_SnapshotPresentsMergedImageAtTheGuestSize(t *testing.T) {
	itEnv(t)
	r := newRig(t)
	ctx := context.Background()

	const baseBytes = 32 << 20
	base := bytes.Repeat([]byte("BASE-IMAGE-CONTENT!!"), baseBytes/20)

	if err := r.m.CreateBase(ctx, 1, "it-base", baseBytes/sector); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	basePath := r.track("it-base")
	ddWrite(t, basePath, base)
	// A populated base needs no device node. Removing it here is what lets
	// SnapshotBase skip suspending a shared origin.
	if err := r.m.RemoveDevice(ctx, "it-base"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	r.devs = r.devs[:len(r.devs)-1]

	// The guest asks for four times the base.
	const guestBytes = 128 << 20
	if err := r.m.SnapshotBase(ctx, 1, 2, "it-guest", guestBytes/sector); err != nil {
		t.Fatalf("SnapshotBase: %v", err)
	}
	guest := r.track("it-guest")

	if got := blockSize(t, guest); got != guestBytes {
		t.Errorf("guest device is %d bytes, want %d — the guest's size must not be the base's", got, guestBytes)
	}
	// len(base), not baseBytes: bytes.Repeat truncates when the pattern does
	// not divide the size, and comparing against the rounded-up length fails
	// on the tail rather than on anything device-mapper did.
	if got := ddRead(t, guest, 0, len(base)); !bytes.Equal(got, base) {
		t.Error("the base's content is not visible at the front of the guest's device")
	}
	// Beyond the base it must be zeros, NOT the prior-tenant pattern the data
	// device was filled with.
	tail := ddRead(t, guest, int64(len(base)), 1<<20)
	if !isAllZero(tail) {
		t.Errorf("space beyond the base is not zeroed — prior tenant data is visible (first bytes: %q)", trunc(tail))
	}

	// The guest writes; the change must be private to it.
	mine := bytes.Repeat([]byte("GUEST-PRIVATE-WRITE!"), (1<<20)/20)
	ddWriteAt(t, guest, mine, 0)
	if got := ddRead(t, guest, 0, len(mine)); !bytes.Equal(got, mine) {
		t.Error("the guest's own write did not stick")
	}
}

// §7.8: a block handed to a new guest must never contain the previous one's
// data. This is the test that would have caught skip_block_zeroing, and the
// pre-dirtied data device is what makes it meaningful.
func TestIntegration_NoDataLeakBetweenGuests(t *testing.T) {
	itEnv(t)
	r := newRig(t)
	ctx := context.Background()

	const size = 16 << 20
	secret := bytes.Repeat([]byte("TENANT-A-SECRET!"), size/16)

	if err := r.m.CreateBase(ctx, 10, "it-a", size/sector); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	ddWrite(t, r.track("it-a"), secret)
	if err := r.m.RemoveDevice(ctx, "it-a"); err != nil {
		t.Fatal(err)
	}
	r.devs = r.devs[:len(r.devs)-1]
	if err := r.m.DeleteThin(ctx, 10); err != nil {
		t.Fatalf("DeleteThin: %v", err)
	}

	// A fresh, unrelated thin volume over the blocks tenant A just freed.
	if err := r.m.CreateBase(ctx, 11, "it-b", size/sector); err != nil {
		t.Fatalf("CreateBase: %v", err)
	}
	b := r.track("it-b")
	// Touch every block so each one is provisioned.
	for off := 0; off < size; off += BlockSectors * sector {
		ddWriteAt(t, b, make([]byte, 4096), int64(off))
	}
	got := ddRead(t, b, 0, size)
	if n := bytes.Count(got, []byte("TENANT-A-SECRET!")); n != 0 {
		t.Fatalf("guest B can read %d occurrences of guest A's data — block zeroing is off", n)
	}
	if n := bytes.Count(got, []byte("PRIOR-TENANT-BYTES!")); n != 0 {
		t.Fatalf("guest B can read %d occurrences of the data device's prior content", n)
	}
}

// §7.5: a base is a convenience for creating the next guest, not a dependency
// of the running ones. Eviction needs no reference tracking because dm-thin
// already refcounts.
func TestIntegration_DeletingTheBaseLeavesItsGuestsIntact(t *testing.T) {
	itEnv(t)
	r := newRig(t)
	ctx := context.Background()

	const size = 16 << 20
	base := bytes.Repeat([]byte("SHARED-BASE-BLOCK!!!"), size/20)

	if err := r.m.CreateBase(ctx, 20, "it-sbase", size/sector); err != nil {
		t.Fatal(err)
	}
	ddWrite(t, r.track("it-sbase"), base)
	if err := r.m.RemoveDevice(ctx, "it-sbase"); err != nil {
		t.Fatal(err)
	}
	r.devs = r.devs[:len(r.devs)-1]

	if err := r.m.SnapshotBase(ctx, 20, 21, "it-g1", size/sector); err != nil {
		t.Fatal(err)
	}
	g1 := r.track("it-g1")
	before := ddRead(t, g1, 0, size)

	// Evict the base while a guest derives from it.
	if err := r.m.DeleteThin(ctx, 20); err != nil {
		t.Fatalf("DeleteThin(base): %v", err)
	}

	if after := ddRead(t, g1, 0, size); !bytes.Equal(before, after) {
		t.Error("deleting the base changed a derived guest's content")
	}
	ddWriteAt(t, g1, bytes.Repeat([]byte("X"), 4096), 0)
	if got := ddRead(t, g1, 0, 8); !bytes.Equal(got, bytes.Repeat([]byte("X"), 8)) {
		t.Error("a guest whose base was deleted can no longer be written")
	}
}

// Status must parse what this kernel actually emits, not what the docs say it
// emits. A parser that is right about captured strings and wrong about the live
// format reports a broken pool as healthy.
func TestIntegration_StatusReflectsTheLivePool(t *testing.T) {
	itEnv(t)
	r := newRig(t)
	ctx := context.Background()

	st, err := r.m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Healthy() {
		t.Errorf("a fresh pool is not Healthy(): %+v", st)
	}
	if st.Mode != ModeReadWrite {
		t.Errorf("mode = %q, want rw", st.Mode)
	}
	if !st.QueueIfNoSpace {
		t.Error("queue_if_no_space is not set — a full pool would error immediately instead of queueing")
	}
	if !st.DiscardPassdown {
		t.Error("discard_passdown is not set — deleted guests would not return their space")
	}
	if st.TotalData == 0 || st.TotalMetadata == 0 {
		t.Errorf("totals not parsed from live status: %+v", st)
	}

	before := st.UsedData
	if err := r.m.CreateBase(ctx, 30, "it-usage", (8<<20)/sector); err != nil {
		t.Fatal(err)
	}
	ddWrite(t, r.track("it-usage"), bytes.Repeat([]byte("z"), 8<<20))
	after, err := r.m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 8 MiB at 64 KiB blocks is 128 blocks, and dm-thin must not amplify it.
	if got := after.UsedData - before; got != 128 {
		t.Errorf("writing 8 MiB allocated %d blocks, want exactly 128 (64 KiB each)", got)
	}
}

// --- helpers -------------------------------------------------------------

func losetup(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("losetup", "--find", "--show", path).Output()
	if err != nil {
		t.Fatalf("losetup %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

func writePattern(t *testing.T, path string, size int, pat []byte) {
	t.Helper()
	buf := bytes.Repeat(pat, size/len(pat)+1)[:size]
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ddWrite(t *testing.T, dev string, data []byte) { t.Helper(); ddWriteAt(t, dev, data, 0) }
func ddWriteAt(t *testing.T, dev string, data []byte, off int64) {
	t.Helper()
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", dev, err)
	}
	defer f.Close()
	if _, err := f.WriteAt(data, off); err != nil {
		t.Fatalf("write %s at %d: %v", dev, off, err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync %s: %v", dev, err)
	}
}

func ddRead(t *testing.T, dev string, off int64, n int) []byte {
	t.Helper()
	f, err := os.Open(dev)
	if err != nil {
		t.Fatalf("open %s: %v", dev, err)
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil {
		t.Fatalf("read %s at %d: %v", dev, off, err)
	}
	return buf
}

func blockSize(t *testing.T, dev string) int {
	t.Helper()
	f, err := os.Open(dev)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := f.Seek(0, 2)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func isAllZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func trunc(b []byte) string {
	if len(b) > 40 {
		b = b[:40]
	}
	return string(b)
}
