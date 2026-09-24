package thinpool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two builds of different images must not both count the same free space:
// each fitting alone, together they filled the pool and stalled every guest.
func TestMakeRoomFor_DoesNotCountAnotherBuildsReservation(t *testing.T) {
	x, _ := evictRig(t, 0, 1000) // 1000 blocks free = 62.5 MiB
	// Nothing to evict for this test.
	for _, k := range []string{"a", "b", "c"} {
		if err := x.Reg.ForgetBase(k); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.makeRoomFor(context.Background(), 40<<20, "img-1"); err != nil {
		t.Fatalf("first build: %v", err)
	}
	err := x.makeRoomFor(context.Background(), 40<<20, "img-2")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("second build err = %v; it must not count space the first build holds", err)
	}
	// Once the first build is done with it, the space is there again.
	if err := x.Reg.Unreserve("img-1"); err != nil {
		t.Fatal(err)
	}
	if err := x.makeRoomFor(context.Background(), 40<<20, "img-2"); err != nil {
		t.Errorf("after release: %v", err)
	}
}

// A claim left by a build that died lapses instead of holding space forever.
func TestReserve_ACrashedBuildsClaimLapses(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	free := func() (uint64, error) { return 100, nil }
	if ok, _, err := reg.Reserve("dead", 100, free); err != nil || !ok {
		t.Fatalf("reserve: ok=%v err=%v", ok, err)
	}
	// Age the claim past the TTL.
	if err := reg.withLock(func(st *registryState) (bool, error) {
		r := st.Reserved["dead"]
		r.Since = time.Now().Add(-reservationTTL - time.Minute).UTC().Format(time.RFC3339Nano)
		st.Reserved["dead"] = r
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	ok, available, err := reg.Reserve("live", 100, free)
	if err != nil || !ok || available != 100 {
		t.Fatalf("reserve after lapse: ok=%v available=%d err=%v", ok, available, err)
	}
}

// A build's own earlier claim (a retry) is replaced, not counted against it.
func TestReserve_ReplacesTheCallersOwnClaim(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	free := func() (uint64, error) { return 100, nil }
	for i := 0; i < 2; i++ {
		if ok, _, err := reg.Reserve("img", 80, free); err != nil || !ok {
			t.Fatalf("attempt %d: ok=%v err=%v", i, ok, err)
		}
	}
}
