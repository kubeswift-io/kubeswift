package thinpool

import (
	"context"
	"strings"
	"testing"
)

// A base is never evicted while a guest is being snapshotted from it: the
// eviction used to be able to delete it between the readiness check and
// create_snap, failing the guest's creation.
func TestEnsureGuest_HoldsItsBaseAgainstEviction(t *testing.T) {
	x, f := evictRig(t, 1000, 1000)
	evictedDuringSnapshot, tried := false, false
	f.onCall = func(name string, args ...string) {
		if tried || !strings.Contains(strings.Join(args, " "), "create_snap") {
			return
		}
		tried = true
		gone, err := x.evictBase(context.Background(), "a")
		if err != nil {
			t.Errorf("evict during snapshot: %v", err)
		}
		evictedDuringSnapshot = gone
	}
	if _, err := x.EnsureGuest(context.Background(), "a", "ns/g/uid", "ks-g-uid", 1<<20); err != nil {
		t.Fatalf("EnsureGuest: %v", err)
	}
	if !tried {
		t.Fatal("create_snap was never issued")
	}
	if evictedDuringSnapshot {
		t.Error("the base was evicted while a guest was being snapshotted from it")
	}
}
