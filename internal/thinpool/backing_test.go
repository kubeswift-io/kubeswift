package thinpool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// THE data-integrity guard. A loop device writes through the host page cache
// unless direct I/O is on, so a guest's O_DIRECT write — a database commit, a
// journal flush — is acknowledged while the bytes are still in host RAM and is
// lost if the node crashes. It measures as a 2.8x speedup, which is exactly
// what makes it dangerous.
//
// losetup can accept --direct-io=on and still leave it off (the backing
// filesystem may not support O_DIRECT), so the flag is not evidence. Only the
// read-back is.
func TestEnsureLoop_RefusesWhenDirectIODidNotTakeEffect(t *testing.T) {
	f := &fakeRunner{out: map[string]string{}, fail: map[string]error{}}
	f.out["losetup -j /img --noheadings -O NAME"] = ""
	f.out["losetup --find --show --direct-io=on /img"] = "/dev/loop7\n"
	f.out["losetup -l --noheadings -O DIO /dev/loop7"] = "0\n" // the silent downgrade

	_, err := EnsureLoop(context.Background(), f, "/img")
	if err == nil {
		t.Fatal("a loop device with direct I/O off was accepted; guest writes would be lost on a node crash")
	}
	for _, want := range []string{"direct I/O", "page cache", "crash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should explain the consequence (%q): %v", want, err)
		}
	}
	// It must not leave a half-configured device behind: one that "works" while
	// silently caching is worse than none at all.
	var detached bool
	for _, c := range f.joined() {
		if strings.Contains(c, "losetup -d /dev/loop7") {
			detached = true
		}
	}
	if !detached {
		t.Error("the rejected loop device was left attached")
	}
}

func TestEnsureLoop_AcceptsWhenDirectIOIsOn(t *testing.T) {
	f := &fakeRunner{out: map[string]string{}, fail: map[string]error{}}
	f.out["losetup -j /img --noheadings -O NAME"] = ""
	f.out["losetup --find --show --direct-io=on /img"] = "/dev/loop7\n"
	f.out["losetup -l --noheadings -O DIO /dev/loop7"] = "1\n"

	dev, err := EnsureLoop(context.Background(), f, "/img")
	if err != nil {
		t.Fatalf("EnsureLoop: %v", err)
	}
	if dev != "/dev/loop7" {
		t.Errorf("device = %q, want /dev/loop7", dev)
	}
}

// An unreadable DIO column must not be treated as "probably on". Guessing here
// is guessing about whether the guest's data is durable.
func TestEnsureLoop_RefusesAnUnreadableDirectIOState(t *testing.T) {
	f := &fakeRunner{out: map[string]string{}, fail: map[string]error{}}
	f.out["losetup -j /img --noheadings -O NAME"] = ""
	f.out["losetup --find --show --direct-io=on /img"] = "/dev/loop7\n"
	f.out["losetup -l --noheadings -O DIO /dev/loop7"] = "(unknown)\n"

	if _, err := EnsureLoop(context.Background(), f, "/img"); err == nil {
		t.Fatal("an unreadable direct-io state was accepted")
	}
}

// An already-attached file is reused, but still checked: a loop device someone
// else attached without direct I/O is exactly as unsafe as one we attached.
func TestEnsureLoop_ReusesButStillVerifies(t *testing.T) {
	f := &fakeRunner{out: map[string]string{}, fail: map[string]error{}}
	f.out["losetup -j /img --noheadings -O NAME"] = "/dev/loop3\n"
	f.out["losetup -l --noheadings -O DIO /dev/loop3"] = "0\n"

	if _, err := EnsureLoop(context.Background(), f, "/img"); err == nil {
		t.Fatal("an existing loop device with direct I/O off was reused without complaint")
	}
	for _, c := range f.joined() {
		if strings.Contains(c, "--find") {
			t.Errorf("an already-attached file was attached again: %s", c)
		}
	}
}

// Preallocated, never sparse: a sparse file lets the pool believe it has space
// the filesystem cannot honour, and hides block reuse behind discard-punched
// holes.
func TestEnsurePreallocated_AllocatesRealBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "data.img")
	const size = 8 << 20

	if err := ensurePreallocated(path, size); err != nil {
		t.Fatalf("ensurePreallocated: %v", err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != size {
		t.Errorf("size = %d, want %d", st.Size, size)
	}
	// st_blocks counts 512-byte units actually allocated. A sparse file reports
	// near zero here while still claiming the full size.
	if allocated := st.Blocks * 512; allocated < size {
		t.Errorf("file is sparse: %d bytes allocated for a %d byte file", allocated, size)
	}
}

// Shrinking the pool under running guests is not something to do silently.
func TestEnsurePreallocated_RefusesToShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.img")
	if err := ensurePreallocated(path, 8<<20); err != nil {
		t.Fatal(err)
	}
	err := ensurePreallocated(path, 32<<20)
	if err == nil {
		t.Fatal("a file smaller than the configured size was accepted")
	}
	if !strings.Contains(err.Error(), "grow it deliberately") {
		t.Errorf("error should tell the operator what to do: %v", err)
	}
}

// An existing file at or above the configured size is left alone — reruns are
// normal, and rewriting the backing file would destroy every guest on the node.
func TestEnsurePreallocated_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.img")
	if err := ensurePreallocated(path, 8<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".marker", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePreallocated(path, 8<<20); err != nil {
		t.Fatalf("second call: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("the backing file was rewritten on a rerun; every guest on the node would lose its disk")
	}
}

func TestRequireBlockDevice_RejectsARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notadevice")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireBlockDevice(path); err == nil {
		t.Fatal("a regular file was accepted as a backing device")
	}
}

func TestBacking_ResolveRequiresOneOfDeviceOrFile(t *testing.T) {
	f := &fakeRunner{out: map[string]string{}, fail: map[string]error{}}
	if _, err := (Backing{}).Resolve(context.Background(), f); err == nil {
		t.Fatal("an empty Backing resolved to something")
	}
}

// A pool is preallocated in one go, so "it fits" is not enough: a node that
// ends up under the kubelet's eviction threshold starts evicting everything
// else on it. Refusing costs one guest instead.
func TestRoomFor_RefusesWhatWouldFillTheFilesystem(t *testing.T) {
	dir := t.TempDir()
	err := roomFor(dir, 1<<50) // 1 PiB
	if err == nil {
		t.Fatal("a pool larger than the filesystem was accepted")
	}
	for _, want := range []string{"1024.0 TiB", "free of", "evicting"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q should contain %q — the operator needs the numbers", err, want)
		}
	}
}

func TestRoomFor_AllowsASizeThatLeavesHeadroom(t *testing.T) {
	if err := roomFor(t.TempDir(), 1<<20); err != nil {
		t.Fatalf("a 1 MiB pool was refused: %v", err)
	}
}

func TestHuman(t *testing.T) {
	for in, want := range map[uint64]string{
		2 << 40:   "2.0 TiB",
		40 << 30:  "40.0 GiB",
		512 << 20: "512.0 MiB",
		42:        "42 bytes",
	} {
		if got := human(in); got != want {
			t.Errorf("human(%d) = %q, want %q", in, got, want)
		}
	}
}

// A node that is refused keeps nothing: the check runs before the directory is
// made, so an unusable node is left as it was found.
func TestEnsurePreallocated_RefusedLeavesNothingBehind(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "thinpool")
	err := ensurePreallocated(filepath.Join(dir, "data.img"), 1<<50)
	if err == nil {
		t.Fatal("a pool larger than the filesystem was accepted")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("%s was created for a pool that was refused (stat err = %v)", dir, statErr)
	}
}
