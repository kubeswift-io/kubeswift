package thinpool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// poolStatus is a thin-pool status line with used of total data blocks.
func poolStatus(used, total uint64) string {
	return fmt.Sprintf("0 1 thin-pool 0 1/2 %d/%d - rw discard_passdown queue_if_no_space - 1", used, total)
}

// evictRig is a materializer whose pool says what the test wants, holding
// bases a, b and c, used in that order (a least recently).
func evictRig(t *testing.T, used, total uint64) (*Materializer, *fakeRunner) {
	t.Helper()
	m, f := testManager()
	dir := t.TempDir()
	x := &Materializer{M: m, Reg: NewRegistry(filepath.Join(dir, "registry.json")), LockDir: filepath.Join(dir, "locks")}
	for _, k := range []string{"a", "b", "c"} {
		if _, _, err := x.Reg.AllocateBase(k); err != nil {
			t.Fatal(err)
		}
		if err := x.Reg.MarkBaseReady(k); err != nil {
			t.Fatal(err)
		}
		if err := x.Reg.TouchBase(k); err != nil {
			t.Fatal(err)
		}
	}
	f.out["dmsetup --noudevsync status kstest"] = poolStatus(used, total)
	f.out["dmsetup --noudevsync ls"] = ""
	return x, f
}

func bases(t *testing.T, x *Materializer) string {
	t.Helper()
	got, err := x.Reg.Bases()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(got, ",")
}

// Room already: nothing is evicted. A base costs only space, and space it has.
func TestMakeRoomFor_KeepsEverythingWhenThereIsRoom(t *testing.T) {
	x, f := evictRig(t, 0, 1000) // 1000 blocks free = 62.5 MiB
	if err := x.makeRoomFor(context.Background(), 1<<20, "d"); err != nil {
		t.Fatal(err)
	}
	if got := bases(t, x); got != "a,b,c" {
		t.Errorf("bases = %s, want all three kept", got)
	}
	for _, c := range f.joined() {
		if strings.Contains(c, "delete") {
			t.Errorf("something was evicted although the pool had room: %s", c)
		}
	}
}

// Under pressure the least recently used base goes first, and only as many as
// the new base needs.
func TestMakeRoomFor_EvictsLeastRecentlyUsedUntilItFits(t *testing.T) {
	x, f := evictRig(t, 1000, 1000) // full
	// Each eviction frees 400 blocks (25 MiB); one is enough for a 20 MiB base.
	freed := uint64(0)
	f.onCall = func(name string, args ...string) {
		if strings.Contains(strings.Join(args, " "), "delete") {
			freed += 400
			f.out["dmsetup --noudevsync status kstest"] = poolStatus(1000-freed, 1000)
		}
	}
	if err := x.makeRoomFor(context.Background(), 20<<20, "c"); err != nil {
		t.Fatal(err)
	}
	if got := bases(t, x); got != "b,c" {
		t.Errorf("bases = %s; want a evicted first (least recently used) and b kept once it fit", got)
	}
}

// The base being built is never a candidate, however old it looks.
func TestMakeRoomFor_NeverEvictsTheBaseBeingBuilt(t *testing.T) {
	x, f := evictRig(t, 1000, 1000)
	f.onCall = func(name string, args ...string) {
		if strings.Contains(strings.Join(args, " "), "delete") {
			f.out["dmsetup --noudevsync status kstest"] = poolStatus(0, 1000)
		}
	}
	// "a" is the least recently used, and would go first if it were allowed to.
	if err := x.makeRoomFor(context.Background(), 1<<20, "a"); err != nil {
		t.Fatal(err)
	}
	if _, known, err := x.Reg.BaseID("a"); err != nil {
		t.Fatal(err)
	} else if !known {
		t.Errorf("the base being built was evicted (bases = %s)", bases(t, x))
	}
}

// A base someone else is using right now is skipped rather than waited for:
// waiting is how two builds evicting each other deadlock.
func TestEvictBase_SkipsOneThatIsLocked(t *testing.T) {
	x, _ := evictRig(t, 1000, 1000)
	unlock, err := x.lockKey("a")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	gone, err := x.evictBase(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if gone {
		t.Error("a base held by another writer was evicted")
	}
	if got := bases(t, x); got != "a,b,c" {
		t.Errorf("bases = %s, want all kept", got)
	}
}

// Nothing left to evict: say what it has, what it needs and what it tried.
func TestMakeRoomFor_SaysWhenItStillDoesNotFit(t *testing.T) {
	x, _ := evictRig(t, 1000, 1000)
	err := x.makeRoomFor(context.Background(), 1<<30, "d")
	if err == nil {
		t.Fatal("a base that cannot fit was accepted")
	}
	for _, want := range []string{"still does not fit", "evicted 3 of 3", "larger data device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q should contain %q", err, want)
		}
	}
}

// An eviction that fails for a real reason stops the process: the pool is not
// in the state the caller assumed.
func TestMakeRoomFor_StopsOnARealFailure(t *testing.T) {
	x, f := evictRig(t, 1000, 1000)
	id, _, _ := x.Reg.BaseID("a")
	key := fmt.Sprintf("dmsetup --noudevsync message kstest 0 delete %d", id)
	f.fail[key] = errors.New("exit status 1")
	f.out[key] = "device-mapper: message ioctl on kstest failed: Device or resource busy\n"
	if err := x.makeRoomFor(context.Background(), 1<<20, "d"); err == nil {
		t.Fatal("a failing eviction was treated as progress")
	}
	if got := bases(t, x); got != "a,b,c" {
		t.Errorf("bases = %s; a failed eviction must not forget the base", got)
	}
}

// deriveGuest records guest g as snapshotted from base k.
func deriveGuest(t *testing.T, x *Materializer, g, k string) {
	t.Helper()
	baseID, ok, err := x.Reg.BaseID(k)
	if err != nil || !ok {
		t.Fatalf("base %s: known=%v err=%v", k, ok, err)
	}
	if _, _, err := x.Reg.AllocateGuest(g); err != nil {
		t.Fatal(err)
	}
	if err := x.Reg.MarkGuestCreated(g, baseID); err != nil {
		t.Fatal(err)
	}
}

// A base running guests were snapshotted from shares nearly all its blocks
// with them, so evicting it frees almost nothing and still costs a rebuild.
// The least recently used base used to go first regardless; an unshared one
// goes first now.
func TestMakeRoomFor_EvictsUnsharedBasesBeforeShared(t *testing.T) {
	x, f := evictRig(t, 1000, 1000) // full; a is the least recently used
	deriveGuest(t, x, "ns/vm/uid-1", "a")
	freed := uint64(0)
	f.onCall = func(name string, args ...string) {
		if strings.Contains(strings.Join(args, " "), "delete") {
			freed += 400
			f.out["dmsetup --noudevsync status kstest"] = poolStatus(1000-freed, 1000)
		}
	}
	if err := x.makeRoomFor(context.Background(), 20<<20, "d"); err != nil {
		t.Fatal(err)
	}
	if got := bases(t, x); got != "a,c" {
		t.Errorf("bases = %s; want b evicted (least recently used of the unshared) and a, which a guest shares, kept", got)
	}
}

// Shared bases are the last resort, not off limits: when nothing else frees
// enough they are evicted too, and a failure says how many were shared.
func TestMakeRoomFor_SharedBasesAreTheLastResort(t *testing.T) {
	x, _ := evictRig(t, 1000, 1000) // full, and evicting frees nothing
	deriveGuest(t, x, "ns/vm/uid-1", "a")
	err := x.makeRoomFor(context.Background(), 20<<20, "d")
	if err == nil || !strings.Contains(err.Error(), "evicted 3 of 3 other bases (1 of them shared by running guests") {
		t.Fatalf("err = %v; want every base tried, the shared one counted", err)
	}
}
