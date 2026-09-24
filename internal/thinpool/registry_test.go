package thinpool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func tmpRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(filepath.Join(t.TempDir(), "thinpool", "registry.json"))
}

func TestAllocate_IsStableAndReportsFreshness(t *testing.T) {
	r := tmpRegistry(t)

	id, fresh, err := r.AllocateBase("sha256:aaa")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh {
		t.Error("first allocation should report fresh; the caller has to know whether to populate the base")
	}
	again, fresh, err := r.AllocateBase("sha256:aaa")
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Errorf("second allocation returned %d, want the same %d", again, id)
	}
	if fresh {
		t.Error("second allocation reported fresh; the base would be written again over a live one")
	}

	other, _, err := r.AllocateBase("sha256:bbb")
	if err != nil {
		t.Fatal(err)
	}
	if other == id {
		t.Errorf("two digests share device id %d", id)
	}
}

// Bases and guests share one id space, because they share one pool. Handing the
// same id to both would make a guest's snapshot and a base the same device.
func TestAllocate_BasesAndGuestsShareOneIDSpace(t *testing.T) {
	r := tmpRegistry(t)
	seen := map[uint32]string{}
	for _, k := range []string{"sha256:a", "sha256:b"} {
		id, _, err := r.AllocateBase(k)
		if err != nil {
			t.Fatal(err)
		}
		seen[id] = k
	}
	for _, k := range []string{"ns/one", "ns/two"} {
		id, _, err := r.AllocateGuest(k)
		if err != nil {
			t.Fatal(err)
		}
		if prev, clash := seen[id]; clash {
			t.Fatalf("guest %q got device id %d, already held by %q", k, id, prev)
		}
		seen[id] = k
	}
	if len(seen) != 4 {
		t.Errorf("got %d distinct ids for 4 keys", len(seen))
	}
}

func TestRegistry_PersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thinpool", "registry.json")

	want, _, err := NewRegistry(path).AllocateGuest("ns/guest")
	if err != nil {
		t.Fatal(err)
	}
	// A fresh Registry over the same path is a new process on the same node.
	got, ok, err := NewRegistry(path).GuestID("ns/guest")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != want {
		t.Errorf("after reload: id=%d known=%v, want %d/true — losing this mapping orphans the guest's disk", got, ok, want)
	}
}

// A freed id is never handed out again. If the delete did not actually
// complete, reusing the id would give a new guest the previous one's blocks —
// the same exposure as skipping block zeroing, reached from the other side.
func TestForget_DoesNotRecycleTheID(t *testing.T) {
	r := tmpRegistry(t)
	first, _, err := r.AllocateGuest("ns/gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Forget("ns/gone"); err != nil {
		t.Fatal(err)
	}
	next, _, err := r.AllocateGuest("ns/new")
	if err != nil {
		t.Fatal(err)
	}
	if next == first {
		t.Errorf("device id %d was recycled after Forget", first)
	}
	if _, ok, _ := r.GuestID("ns/gone"); ok {
		t.Error("Forget left the mapping behind")
	}
}

// The pool's metadata is authoritative about which ids exist, not this file. A
// registry that has fallen behind hands out an id the pool already holds and
// create_thin fails with "File exists"; Advance is how that is recovered,
// rather than the node never creating another guest.
func TestAdvance_SkipsIDsThePoolAlreadyHolds(t *testing.T) {
	r := tmpRegistry(t)
	if err := r.Advance(41); err != nil {
		t.Fatal(err)
	}
	id, _, err := r.AllocateGuest("ns/after")
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42 (one past the advanced-to id)", id)
	}
	// Advancing backwards must not rewind and start colliding again.
	if err := r.Advance(3); err != nil {
		t.Fatal(err)
	}
	next, _, err := r.AllocateGuest("ns/later")
	if err != nil {
		t.Fatal(err)
	}
	if next <= id {
		t.Errorf("id went backwards to %d after Advance(3); ids must only move forward", next)
	}
}

// Several guests can start on a node at once, each running its own
// materialisation. Two allocations that raced to the same id would give two
// guests the same disk.
func TestAllocate_ConcurrentCallersGetDistinctIDs(t *testing.T) {
	r := tmpRegistry(t)
	const n = 24

	var wg sync.WaitGroup
	ids := make([]uint32, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _, err := r.AllocateGuest("ns/guest-" + string(rune('a'+i)))
			ids[i], errs[i] = id, err
		}(i)
	}
	wg.Wait()

	seen := map[uint32]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
		if prev, dup := seen[ids[i]]; dup {
			t.Fatalf("callers %d and %d both got device id %d", prev, i, ids[i])
		}
		seen[ids[i]] = i
	}
}

// A corrupt registry beside a pool full of guests must stop the node, not reset
// it. Starting from id 1 again would allocate over devices that already exist,
// and "recovering" by forgetting which device belongs to whom is how a guest
// silently comes up on another guest's disk.
func TestCorruptRegistry_RefusesRatherThanResetting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(path)

	if _, _, err := r.AllocateGuest("ns/g"); err == nil {
		t.Fatal("a corrupt registry allocated an id; it must refuse")
	} else if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error should say the registry is corrupt: %v", err)
	}
}

// An interrupted write must leave the previous registry intact, never a
// truncated one — the whole reason store() writes to a temp file and renames.
func TestStore_LeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	r := NewRegistry(path)
	if _, _, err := r.AllocateGuest("ns/first"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temp file survived a successful write: %s", e.Name())
		}
	}
	// And the file that is there is complete and readable.
	if _, ok, err := r.GuestID("ns/first"); err != nil || !ok {
		t.Errorf("registry unreadable after write: ok=%v err=%v", ok, err)
	}
}

func TestDefaultRegistryPath(t *testing.T) {
	if got := DefaultRegistryPath("/var/lib/kubeswift"); got != "/var/lib/kubeswift/thinpool/registry.json" {
		t.Errorf("DefaultRegistryPath = %q", got)
	}
}

func TestRegistry_LeastRecentUseOrdersEviction(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	for _, k := range []string{"first", "second", "never-used"} {
		if _, _, err := r.AllocateBase(k); err != nil {
			t.Fatal(err)
		}
	}
	// Used in this order; "never-used" is never touched.
	for _, k := range []string{"first", "second"} {
		if err := r.TouchBase(k); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond) // distinct stamps, without waiting on a clock tick
	}
	got, err := r.BasesByLeastRecentUse()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"never-used", "first", "second"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v — a base nothing has used goes first", got, want)
	}
}

func TestRegistry_ForgettingABaseForgetsItsUse(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if _, _, err := r.AllocateBase("b"); err != nil {
		t.Fatal(err)
	}
	if err := r.TouchBase("b"); err != nil {
		t.Fatal(err)
	}
	if err := r.ForgetBase("b"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"b"`) {
		t.Errorf("the registry still mentions the forgotten base: %s", raw)
	}
}

// Touching a base nothing allocated must not invent one.
func TestRegistry_TouchingAnUnknownBaseDoesNothing(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err := r.TouchBase("ghost"); err != nil {
		t.Fatal(err)
	}
	got, err := r.Bases()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("bases = %v, want none", got)
	}
}

// EvictionOrder: bases no live guest came from first, least recently used
// first within each group.
func TestRegistry_EvictionOrderPutsSharedBasesLast(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	for _, k := range []string{"old", "mid", "new"} {
		if _, _, err := r.AllocateBase(k); err != nil {
			t.Fatal(err)
		}
		if err := r.TouchBase(k); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	oldID, _, _ := r.BaseID("old")
	if _, _, err := r.AllocateGuest("ns/g/1"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkGuestCreated("ns/g/1", oldID); err != nil {
		t.Fatal(err)
	}
	order, shared, err := r.EvictionOrder()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "mid,new,old" || shared != 1 {
		t.Errorf("order = %s shared = %d; want mid,new,old with 1 shared", got, shared)
	}

	// The guest goes: its base is no longer shared.
	if err := r.Forget("ns/g/1"); err != nil {
		t.Fatal(err)
	}
	order, shared, err = r.EvictionOrder()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "old,mid,new" || shared != 0 {
		t.Errorf("after the guest is released: order = %s shared = %d; want plain LRU", got, shared)
	}
}

// A guest shares blocks with the base DEVICE it came from. Once that base is
// evicted and the digest rebuilt, the new device shares nothing with the
// older guest, and must not be held back on its account.
func TestRegistry_ARebuiltBaseIsNotSharedByOlderGuests(t *testing.T) {
	r := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if _, _, err := r.AllocateBase("img"); err != nil {
		t.Fatal(err)
	}
	oldID, _, _ := r.BaseID("img")
	if _, _, err := r.AllocateGuest("ns/g/1"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkGuestCreated("ns/g/1", oldID); err != nil {
		t.Fatal(err)
	}
	if err := r.ForgetBase("img"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.AllocateBase("img"); err != nil {
		t.Fatal(err)
	}
	if _, shared, err := r.EvictionOrder(); err != nil || shared != 0 {
		t.Errorf("shared = %d err = %v; the rebuilt base shares nothing with the older guest", shared, err)
	}
}

// Guests created before the record existed have no entry; they are not
// counted, which is how eviction behaved before, and the file round-trips.
func TestRegistry_DerivedIsOptionalAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	legacy := `{"nextID":3,"bases":{"img":1},"guests":{"ns/g/1":2},"created":{"ns/g/1":true},"ready":{"img":true}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(path)
	if _, shared, err := r.EvictionOrder(); err != nil || shared != 0 {
		t.Fatalf("legacy registry: shared = %d err = %v", shared, err)
	}
	if _, _, err := r.AllocateGuest("ns/g/2"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkGuestCreated("ns/g/2", 1); err != nil {
		t.Fatal(err)
	}
	if _, shared, err := NewRegistry(path).EvictionOrder(); err != nil || shared != 1 {
		t.Errorf("after reload: shared = %d err = %v; want the recorded guest to hold its base", shared, err)
	}
}
