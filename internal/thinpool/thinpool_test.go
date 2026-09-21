package thinpool

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner records every command instead of running it.
type fakeRunner struct {
	calls [][]string
	out   map[string]string // joined args -> stdout
	fail  map[string]error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	if err, ok := f.fail[key]; ok {
		return f.out[key], err
	}
	return f.out[key], nil
}

func (f *fakeRunner) joined() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func testManager() (*Manager, *fakeRunner) {
	f := &fakeRunner{out: map[string]string{}, fail: map[string]error{}}
	return &Manager{Pool: "kstest", DataDev: "/dev/data", MetaDev: "/dev/meta", Run: f}, f
}

// THE security regression guard. skip_block_zeroing hands a guest a block still
// holding the previous owner's data — measured at 3824 occurrences of a prior
// tenant's pattern inside one 64 KiB block. It is a tempting throughput knob and
// both design spikes used it, so this test exists to make putting it back a
// deliberate act rather than an optimisation nobody reviewed.
func TestPoolTable_NeverSkipsBlockZeroing(t *testing.T) {
	m, _ := testManager()
	table := m.poolTable(1 << 20)
	if strings.Contains(table, "skip_block_zeroing") {
		t.Fatalf("pool table enables skip_block_zeroing, which leaks a deleted guest's data into the next one:\n  %s", table)
	}
}

// The block size is fixed for the life of a pool and every measurement behind
// the design was taken at 64 KiB. Changing it silently would invalidate the
// metadata sizing and the no-write-amplification result at once.
func TestPoolTable_Shape(t *testing.T) {
	m, _ := testManager()
	got := m.poolTable(2048)
	want := "0 2048 thin-pool /dev/meta /dev/data 128 16384"
	if got != want {
		t.Errorf("pool table =\n  %s\nwant\n  %s", got, want)
	}
}

func TestEnsurePool_CreatesWhenAbsent(t *testing.T) {
	m, f := testManager()
	f.out["dmsetup ls"] = "some-other-dm-device\t(252:0)\n"

	if err := m.EnsurePool(context.Background(), 4096); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	if len(f.calls) != 2 || f.calls[1][1] != "create" {
		t.Fatalf("expected a list then a create, got:\n  %s", strings.Join(f.joined(), "\n  "))
	}
}

// A pool that already exists must not be recreated: `dmsetup create` on a live
// pool fails, and on a node that is already serving guests a spurious create is
// the difference between a no-op reconcile and an outage.
func TestEnsurePool_IsIdempotent(t *testing.T) {
	m, f := testManager()
	f.out["dmsetup ls"] = "kstest\t(252:3)\nsomething-else\t(252:0)\n"

	if err := m.EnsurePool(context.Background(), 4096); err != nil {
		t.Fatalf("EnsurePool: %v", err)
	}
	for _, c := range f.joined() {
		if strings.Contains(c, "create") {
			t.Errorf("EnsurePool created a pool that already exists: %s", c)
		}
	}
}

// A guest's snapshot must not suspend the base. Every guest on the node shares
// one base, so suspending it per creation would serialise guest starts behind a
// single lock — and it is unnecessary, because a populated base has no active
// device to quiesce.
func TestSnapshotBase_DoesNotSuspendTheOrigin(t *testing.T) {
	m, f := testManager()
	if err := m.SnapshotBase(context.Background(), 1, 7, "guest-7", 8192); err != nil {
		t.Fatalf("SnapshotBase: %v", err)
	}
	for _, c := range f.joined() {
		if strings.Contains(c, "suspend") || strings.Contains(c, "resume") {
			t.Errorf("SnapshotBase suspended the shared origin, serialising every guest start: %s", c)
		}
	}
	want := []string{
		"dmsetup message /dev/mapper/kstest 0 create_snap 7 1",
		"dmsetup create guest-7 --table 0 8192 thin /dev/mapper/kstest 7",
	}
	got := f.joined()
	if len(got) != len(want) {
		t.Fatalf("got %d calls, want %d:\n  %s", len(got), len(want), strings.Join(got, "\n  "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d =\n  %s\nwant\n  %s", i, got[i], want[i])
		}
	}
}

// A guest may ask for more than the base holds. The thin device's length is its
// own, and the oversize costs nothing until written.
func TestSnapshotBase_HonoursTheGuestSize(t *testing.T) {
	m, f := testManager()
	const guestSectors = 1 << 20 // far larger than any base
	if err := m.SnapshotBase(context.Background(), 1, 9, "big", guestSectors); err != nil {
		t.Fatal(err)
	}
	last := f.joined()[len(f.calls)-1]
	if !strings.Contains(last, "0 1048576 thin") {
		t.Errorf("guest device not created at the requested size: %s", last)
	}
}

// A failed reload must not leave the pool suspended: every guest on the node is
// mapped through it, so a suspended pool is a node-wide stall.
func TestReload_ResumesEvenWhenReloadFails(t *testing.T) {
	m, f := testManager()
	key := "dmsetup reload kstest --table " + m.poolTable(9999)
	f.fail[key] = errNotFound{}

	if err := m.GrowData(context.Background(), 9999); err == nil {
		t.Fatal("GrowData should surface the reload failure")
	}
	var resumed bool
	for _, c := range f.joined() {
		if strings.HasPrefix(c, "dmsetup resume") {
			resumed = true
		}
	}
	if !resumed {
		t.Error("pool left suspended after a failed reload — every guest on the node would stall")
	}
}

func TestMetadataSectorsFor(t *testing.T) {
	// 100 GiB of data: 1638400 mappings x 48 B = ~75 MiB, doubled and floored
	// at 64 MiB. The exact figure matters less than the properties below.
	got := MetadataSectorsFor(100<<30) * 512

	if got < 64<<20 {
		t.Errorf("metadata %d B is below the 64 MiB floor", got)
	}
	// Generous, but not absurd: metadata should stay a rounding error against
	// the data it describes.
	if got > 1<<30 {
		t.Errorf("metadata %d B for 100 GiB is more than 1%%; the sizing is wrong", got)
	}
	// A tiny pool still gets the floor rather than a few kilobytes.
	if small := MetadataSectorsFor(1<<20) * 512; small < 64<<20 {
		t.Errorf("small pool got %d B of metadata, below the floor", small)
	}
	// Monotonic: more data never asks for less metadata.
	if MetadataSectorsFor(200<<30) <= MetadataSectorsFor(100<<30) {
		t.Error("metadata sizing is not monotonic in data size")
	}
}

type errNotFound struct{}

func (errNotFound) Error() string { return "Device kstest not found" }
