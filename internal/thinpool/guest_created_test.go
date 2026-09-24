package thinpool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// idPool is a fake dmsetup that remembers which thin device ids exist, so
// activating an id the pool does not hold fails the way the real pool does.
type idPool struct {
	ids      map[uint32]bool
	mapped   map[string]bool
	snaps    []uint32 // ids create_snap succeeded for, in order
	failSnap error    // one-shot create_snap failure
}

func newIDPool(baseID uint32) *idPool {
	return &idPool{ids: map[uint32]bool{baseID: true}, mapped: map[string]bool{}}
}

func (p *idPool) Run(_ context.Context, _ string, args ...string) (string, error) {
	a := strings.Join(args, " ")
	switch {
	case strings.Contains(a, "info -c --noheadings"):
		return "252:3\n", nil
	case strings.HasSuffix(a, " ls"):
		out := ""
		for n := range p.mapped {
			out += n + "\t(252:9)\n"
		}
		return out, nil
	case strings.Contains(a, "create_snap"):
		if p.failSnap != nil {
			e := p.failSnap
			p.failSnap = nil
			return "No data available", e
		}
		f := strings.Fields(a)
		id, _ := strconv.Atoi(f[len(f)-2])
		p.ids[uint32(id)] = true
		p.snaps = append(p.snaps, uint32(id))
		return "", nil
	case strings.Contains(a, " create ") && strings.Contains(a, "--table"):
		f := strings.Fields(a)
		id, _ := strconv.Atoi(f[len(f)-1])
		if !p.ids[uint32(id)] {
			return "", fmt.Errorf("dmsetup create: thin device %d does not exist in pool", id)
		}
		p.mapped[f[2]] = true
		return "", nil
	case strings.Contains(a, "remove "):
		f := strings.Fields(a)
		delete(p.mapped, f[len(f)-1])
		return "", nil
	}
	return "", nil
}

// createdRig: a registry with one ready base "img" (device 1) and a materializer
// over the stateful fake pool.
func createdRig(t *testing.T) (*Materializer, *idPool) {
	t.Helper()
	dir := t.TempDir()
	pool := newIDPool(firstDeviceID)
	x := &Materializer{
		M:       &Manager{Pool: "kstest", Run: pool},
		Reg:     NewRegistry(filepath.Join(dir, "registry.json")),
		LockDir: filepath.Join(dir, "locks"),
	}
	if _, _, err := x.Reg.AllocateBase("img"); err != nil {
		t.Fatal(err)
	}
	if err := x.Reg.MarkBaseReady("img"); err != nil {
		t.Fatal(err)
	}
	return x, pool
}

// A create_snap failure must not strand the guest. It used to leave the guest
// recorded against an id with no device, so every retry took the reactivate
// path and failed with "does not exist in pool" until the guest was deleted.
func TestEnsureGuest_FailedSnapshotDoesNotStrandTheGuest(t *testing.T) {
	x, pool := createdRig(t)
	ctx := context.Background()

	pool.failSnap = errors.New("exit status 1")
	if _, err := x.EnsureGuest(ctx, "img", "ns/g/uid", "ks-g-uid", 1<<30); err == nil {
		t.Fatal("first attempt should surface the create_snap failure")
	}
	if known, _ := x.HasGuest("ns/g/uid"); known {
		t.Error("a guest whose snapshot failed must not stay recorded against a device that does not exist")
	}

	if _, err := x.EnsureGuest(ctx, "img", "ns/g/uid", "ks-g-uid", 1<<30); err != nil {
		t.Fatalf("retry after a failed snapshot must create the disk, got: %v", err)
	}
	if created, _ := x.Reg.GuestCreated("ns/g/uid"); !created {
		t.Error("guest should be marked created once its snapshot exists")
	}
}

// A process that died after allocating the id but before create_snap (or before
// recording success) leaves the guest allocated-not-created. The next attempt
// creates it with a FRESH id — never activating the old one, which could belong
// to another guest if the registry is behind the pool.
func TestEnsureGuest_CrashAfterAllocateRecoversWithAFreshID(t *testing.T) {
	x, pool := createdRig(t)
	ctx := context.Background()

	stale, fresh, err := x.Reg.AllocateGuest("ns/g/uid") // the crash: allocated, never snapshotted
	if err != nil || !fresh {
		t.Fatalf("allocate: id=%d fresh=%v err=%v", stale, fresh, err)
	}

	if _, err := x.EnsureGuest(ctx, "img", "ns/g/uid", "ks-g-uid", 1<<30); err != nil {
		t.Fatalf("a pending guest must be created, not stranded: %v", err)
	}
	id, _, _ := x.Reg.GuestID("ns/g/uid")
	if id == stale {
		t.Errorf("recovered guest reused the stale id %d; it must take a fresh one", stale)
	}
	if len(pool.snaps) != 1 || pool.snaps[0] != id {
		t.Errorf("expected exactly one create_snap for the fresh id %d, got %v", id, pool.snaps)
	}
}

// A CREATED guest whose device is gone lost real data: it must still fail
// loudly, never be handed a fresh empty disk.
func TestEnsureGuest_CreatedGuestWithMissingDeviceStillFails(t *testing.T) {
	x, pool := createdRig(t)
	ctx := context.Background()
	if _, err := x.EnsureGuest(ctx, "img", "ns/g/uid", "ks-g-uid", 1<<30); err != nil {
		t.Fatal(err)
	}
	id, _, _ := x.Reg.GuestID("ns/g/uid")
	delete(pool.ids, id)            // the pool lost the device
	delete(pool.mapped, "ks-g-uid") // and it is no longer mapped
	_, err := x.EnsureGuest(ctx, "img", "ns/g/uid", "ks-g-uid", 1<<30)
	if err == nil || !strings.Contains(err.Error(), "reactivating") {
		t.Fatalf("a created guest with no device must fail on reactivation, got %v", err)
	}
}

// A registry written before the created phase existed has no "created" key:
// every guest it names is treated as created, keeping reactivation's old
// behaviour for them. A registry written now keeps a pending guest pending
// across a reload (the key is present even when empty).
func TestRegistry_CreatedPhaseMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacy, []byte(`{"nextID": 5, "guests": {"ns/old/uid": 3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := NewRegistry(legacy).GuestCreated("ns/old/uid"); err != nil || !created {
		t.Errorf("legacy guest should read as created, got created=%v err=%v", created, err)
	}

	current := NewRegistry(filepath.Join(dir, "current.json"))
	if _, _, err := current.AllocateGuest("ns/new/uid"); err != nil {
		t.Fatal(err)
	}
	reloaded := NewRegistry(filepath.Join(dir, "current.json"))
	if created, err := reloaded.GuestCreated("ns/new/uid"); err != nil || created {
		t.Errorf("a pending guest must stay pending across a reload, got created=%v err=%v", created, err)
	}
}
