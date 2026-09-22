package thinpool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// What dmsetup prints when dm-thin answers a delete of an unknown id (ENODATA).
const enodataOutput = "device-mapper: message ioctl on kstest  failed: No data available\nCommand failed.\n"

const (
	rwStatus = "0 1 thin-pool 0 1/2 1/2 - rw discard_passdown queue_if_no_space - 1"
	roStatus = "0 1 thin-pool 0 2/2 1/2 - ro discard_passdown queue_if_no_space - 1"
)

func TestDeleteThin_AnUnknownIDIsErrNoSuchThin(t *testing.T) {
	m, f := testManager()
	f.out["dmsetup --noudevsync message kstest 0 delete 7"] = enodataOutput
	f.fail["dmsetup --noudevsync message kstest 0 delete 7"] = errors.New("exit status 1")
	if err := m.DeleteThin(context.Background(), 7); !errors.Is(err, ErrNoSuchThin) {
		t.Fatalf("err = %v, want ErrNoSuchThin", err)
	}
}

// Only ENODATA means "already gone". Anything else is a real failure and must
// not be mistaken for success — that would forget a disk still in the pool.
func TestDeleteThin_OtherFailuresAreNotErrNoSuchThin(t *testing.T) {
	m, f := testManager()
	f.out["dmsetup --noudevsync message kstest 0 delete 7"] = "device-mapper: message ioctl on kstest  failed: Device or resource busy\n"
	f.fail["dmsetup --noudevsync message kstest 0 delete 7"] = errors.New("exit status 1")
	err := m.DeleteThin(context.Background(), 7)
	if err == nil || errors.Is(err, ErrNoSuchThin) {
		t.Fatalf("err = %v, want a failure that is NOT ErrNoSuchThin", err)
	}
}

func TestRemoveDevice_RemovesItsNode(t *testing.T) {
	m, f := testManager()
	if err := m.RemoveDevice(context.Background(), "g"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.joined(), " | ")
	want := "dmsetup --noudevsync remove --retry g | dmsetup --noudevsync mknodes g"
	if got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

// releaseRig is a Materializer with a real registry holding one guest, and a
// device-mapper that says whatever the test sets.
func releaseRig(t *testing.T) (*Materializer, *fakeRunner, uint32) {
	t.Helper()
	m, f := testManager()
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"))
	id, _, err := reg.AllocateGuest("ns/g/uid")
	if err != nil {
		t.Fatal(err)
	}
	f.out["dmsetup --noudevsync ls"] = "ks-g-uid\t(252:4)\n"
	f.out["dmsetup --noudevsync status kstest"] = rwStatus
	return &Materializer{M: m, Reg: reg}, f, id
}

func known(t *testing.T, x *Materializer) bool {
	t.Helper()
	_, ok, err := x.Reg.GuestID("ns/g/uid")
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestReleaseGuest_UnmapsDeletesThenForgets(t *testing.T) {
	x, f, id := releaseRig(t)
	if err := x.ReleaseGuest(context.Background(), "ns/g/uid", "ks-g-uid"); err != nil {
		t.Fatal(err)
	}
	if known(t, x) {
		t.Error("the guest is still in the registry after a release")
	}
	calls := strings.Join(f.joined(), " | ")
	for _, want := range []string{"remove --retry ks-g-uid", "delete " + fmt.Sprint(id)} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in %s", want, calls)
		}
	}
}

// The property the ordering buys: a delete that failed leaves the mapping in
// place, so the retry still knows which id to delete. Forgetting first would
// leak the device's blocks for good.
func TestReleaseGuest_KeepsTheMappingWhenTheDeleteFails(t *testing.T) {
	x, f, id := releaseRig(t)
	key := "dmsetup --noudevsync message kstest 0 delete " + fmt.Sprint(id)
	f.fail[key] = errors.New("exit status 1")
	f.out[key] = "device-mapper: message ioctl on kstest  failed: Device or resource busy\n"
	if err := x.ReleaseGuest(context.Background(), "ns/g/uid", "ks-g-uid"); err == nil {
		t.Fatal("release succeeded although the delete failed")
	}
	if !known(t, x) {
		t.Fatal("the mapping was dropped although the device was not deleted; a retry can no longer find it")
	}
}

// A release interrupted after the delete: the retry finds the id already gone
// and finishes by forgetting it.
func TestReleaseGuest_FinishesAnInterruptedRelease(t *testing.T) {
	x, f, id := releaseRig(t)
	f.out["dmsetup --noudevsync ls"] = "" // unmapped by the first attempt
	key := "dmsetup --noudevsync message kstest 0 delete " + fmt.Sprint(id)
	f.fail[key] = errors.New("exit status 1")
	f.out[key] = enodataOutput
	if err := x.ReleaseGuest(context.Background(), "ns/g/uid", "ks-g-uid"); err != nil {
		t.Fatalf("retrying an interrupted release: %v", err)
	}
	if known(t, x) {
		t.Error("the guest is still in the registry")
	}
}

func TestReleaseGuest_RefusesAReadOnlyPoolByName(t *testing.T) {
	x, f, _ := releaseRig(t)
	f.out["dmsetup --noudevsync status kstest"] = roStatus
	err := x.ReleaseGuest(context.Background(), "ns/g/uid", "ks-g-uid")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("err = %v, want one naming the read-only pool", err)
	}
	if !known(t, x) {
		t.Error("the mapping was dropped although nothing was deleted")
	}
	for _, c := range f.joined() {
		if strings.Contains(c, " delete ") {
			t.Errorf("a delete was sent to a read-only pool: %s", c)
		}
	}
}

func TestReleaseGuest_AnUnknownGuestIsANoOp(t *testing.T) {
	x, f, _ := releaseRig(t)
	f.out["dmsetup --noudevsync ls"] = ""
	if err := x.ReleaseGuest(context.Background(), "ns/other/uid2", "ks-g-uid2"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.joined() {
		if strings.Contains(c, " delete ") || strings.Contains(c, " remove ") {
			t.Errorf("released something for a guest this node never held: %s", c)
		}
	}
}
