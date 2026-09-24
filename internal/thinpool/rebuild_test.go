package thinpool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// rebuildRig is a Materializer whose registry holds a base allocated by a
// population that never finished (allocated, not ready).
func rebuildRig(t *testing.T) (*Materializer, *fakeRunner, uint32) {
	t.Helper()
	m, f := testManager()
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	id, fresh, err := reg.AllocateBase("sha256:img")
	if err != nil || !fresh {
		t.Fatalf("allocate: id=%d fresh=%v err=%v", id, fresh, err)
	}
	return &Materializer{M: m, Reg: reg}, f, id
}

// The unfinished base's device could not be deleted (say it is still busy).
// That must stop the rebuild: create_thin would then find the id still in the
// pool and the "registry fell behind" recovery would forget it, leaking the
// old device with nothing naming it.
func TestCreateBase_ARefusedDeleteOfTheUnfinishedBaseStopsTheRebuild(t *testing.T) {
	x, f, id := rebuildRig(t)
	del := fmt.Sprintf("dmsetup --noudevsync message kstest 0 delete %d", id)
	f.out[del] = "device-mapper: message ioctl on kstest  failed: Device or resource busy\n"
	f.fail[del] = errors.New("exit status 1")

	_, err := x.createBase(context.Background(), "sha256:img", baseDeviceName("sha256:img"), 2048)
	if err == nil || !strings.Contains(err.Error(), "deleting the unfinished base") {
		t.Fatalf("err = %v, want the refused delete", err)
	}
	for _, c := range f.joined() {
		if strings.Contains(c, "create_thin") {
			t.Errorf("created a base after failing to delete the old one: %q", c)
		}
	}
	if got, ok, _ := x.Reg.BaseID("sha256:img"); !ok || got != id {
		t.Errorf("registry base = %d (known %v), want id %d still recorded", got, ok, id)
	}
}

// An unfinished base that is already gone from the pool is rebuilt under the
// same id, as before.
func TestCreateBase_AnAlreadyGoneUnfinishedBaseIsRebuilt(t *testing.T) {
	x, f, id := rebuildRig(t)
	del := fmt.Sprintf("dmsetup --noudevsync message kstest 0 delete %d", id)
	f.out[del] = enodataOutput
	f.fail[del] = errors.New("exit status 1")

	got, err := x.createBase(context.Background(), "sha256:img", baseDeviceName("sha256:img"), 2048)
	if err != nil || got != id {
		t.Fatalf("createBase = %d, %v; want id %d rebuilt", got, err, id)
	}
}
