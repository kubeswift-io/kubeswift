package thinpool

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// materializer builds a Materializer over a fresh rig.
func materializer(t *testing.T) (*Materializer, *rig) {
	t.Helper()
	r := newRig(t)
	dir := t.TempDir()
	return &Materializer{
		M:       r.m,
		Reg:     NewRegistry(filepath.Join(dir, "registry.json")),
		LockDir: filepath.Join(dir, "locks"),
	}, r
}

// image returns a recognisable image of n bytes with zero regions in it, since
// real raw disks are mostly empty and zero-skipping has to be exercised.
func image(n int) []byte {
	b := make([]byte, n)
	for off := 0; off < n; off += 4 * blk {
		copy(b[off:], []byte("IMAGE-DATA-BLOCK-AT-"+itoa(off)+"!"))
	}
	return b
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for ; i > 0; i /= 10 {
		d = append([]byte{byte('0' + i%10)}, d...)
	}
	return string(d)
}

func opener(b []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

// gateReader serves an image but stops half-way until released, so a test can
// act while a base is part-written.
type gateReader struct {
	r      *bytes.Reader
	half   int64
	read   int64
	atHalf chan struct{}
	resume chan struct{}
	once   sync.Once
}

func (g *gateReader) Read(p []byte) (int, error) {
	if g.read >= g.half {
		g.once.Do(func() { close(g.atHalf) })
		<-g.resume
	}
	n, err := g.r.Read(p)
	g.read += int64(n)
	return n, err
}
func (g *gateReader) Close() error { return nil }

// THE core property. A base is gigabytes and several guests start on a node at
// once; a guest must never be snapshotted from a base that is still being
// written, or it boots from half an image.
func TestIntegration_NoGuestFromAHalfWrittenBase(t *testing.T) {
	itEnv(t)
	x, r := materializer(t)
	ctx := context.Background()

	img := image(16 << 20)
	gate := &gateReader{
		r: bytes.NewReader(img), half: int64(len(img)) / 2,
		atHalf: make(chan struct{}), resume: make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-gate.resume:
		default:
			close(gate.resume)
		}
	})

	done := make(chan error, 1)
	go func() {
		_, err := x.EnsureBase(ctx, "sha256:half", uint64(len(img)),
			func() (io.ReadCloser, error) { return gate, nil })
		done <- err
	}()
	<-gate.atHalf // the base is now exactly half written

	if path, err := x.EnsureGuest(ctx, "sha256:half", "ns/early", "ksit-early", uint64(len(img))); err == nil {
		r.track("ksit-early")
		t.Fatalf("a guest was snapshotted from a half-written base (%s)", path)
	} else if !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("refused, but for the wrong reason: %v", err)
	}

	close(gate.resume)
	if err := <-done; err != nil {
		t.Fatalf("EnsureBase: %v", err)
	}
	path, err := x.EnsureGuest(ctx, "sha256:half", "ns/late", "ksit-late", uint64(len(img)))
	if err != nil {
		t.Fatalf("EnsureGuest after the base finished: %v", err)
	}
	r.track("ksit-late")
	if got := ddRead(t, path, 0, len(img)); !bytes.Equal(got, img) {
		t.Error("a guest snapshotted after the base finished does not see the whole image")
	}
}

// Several guests on the same image starting at once must produce ONE write.
// Without the per-key lock the losers take the "crashed population" path and
// delete the base the winner is still writing.
func TestIntegration_ConcurrentEnsureBaseWritesOnce(t *testing.T) {
	itEnv(t)
	x, r := materializer(t)
	ctx := context.Background()
	img := image(8 << 20)

	var opens int32
	open := func() (io.ReadCloser, error) {
		atomic.AddInt32(&opens, 1)
		return io.NopCloser(bytes.NewReader(img)), nil
	}

	const n = 6
	var wg sync.WaitGroup
	ids := make([]uint32, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = x.EnsureBase(ctx, "sha256:crowd", uint64(len(img)), open)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("caller %d got base id %d, caller 0 got %d", i, ids[i], ids[0])
		}
	}
	if got := atomic.LoadInt32(&opens); got != 1 {
		t.Errorf("the image was opened %d times for one base; want exactly 1", got)
	}
	path, err := x.EnsureGuest(ctx, "sha256:crowd", "ns/g", "ksit-crowd", uint64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.track("ksit-crowd")
	if got := ddRead(t, path, 0, len(img)); !bytes.Equal(got, img) {
		t.Error("the base written under contention is not the image")
	}
}

// A writer that died part-way leaves a base allocated but never marked ready.
// The next caller must throw it away and write it again, not use it.
func TestIntegration_CrashedPopulationIsRewritten(t *testing.T) {
	itEnv(t)
	x, r := materializer(t)
	ctx := context.Background()
	img := image(8 << 20)
	const key = "sha256:crashed"

	// Reproduce a dead writer: allocated, created, half-written, never marked.
	id, _, err := x.Reg.AllocateBase(key)
	if err != nil {
		t.Fatal(err)
	}
	name := baseDeviceName(key)
	if err := x.M.CreateBase(ctx, id, name, bytesToSectors(uint64(len(img)))); err != nil {
		t.Fatal(err)
	}
	ddWrite(t, x.M.DevicePath(name), bytes.Repeat([]byte("GARBAGE!"), len(img)/16))
	// Left mapped on purpose — a crashed writer does not clean up.

	if _, err := x.EnsureBase(ctx, key, uint64(len(img)), opener(img)); err != nil {
		t.Fatalf("EnsureBase over a crashed population: %v", err)
	}
	path, err := x.EnsureGuest(ctx, key, "ns/g", "ksit-recovered", uint64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.track("ksit-recovered")
	got := ddRead(t, path, 0, len(img))
	if bytes.Contains(got, []byte("GARBAGE!")) {
		t.Fatal("the crashed writer's partial base survived into a guest")
	}
	if !bytes.Equal(got, img) {
		t.Error("the rewritten base is not the image")
	}
}

// Zero blocks are skipped, so an image provisions only what it holds. On a
// real raw disk that is most of the difference between the image's nominal
// size and its cost in the pool.
func TestIntegration_ZeroSkippingProvisionsOnlyData(t *testing.T) {
	itEnv(t)
	x, _ := materializer(t)
	ctx := context.Background()

	const blocks = 256 // 16 MiB
	img := make([]byte, blocks*blk)
	for _, b := range []int{0, 7, 100, 255} { // four blocks with data
		copy(img[b*blk:], []byte("data"))
	}

	before, err := x.M.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.EnsureBase(ctx, "sha256:sparse", uint64(len(img)), opener(img)); err != nil {
		t.Fatal(err)
	}
	after, err := x.M.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.UsedData - before.UsedData; got != 4 {
		t.Errorf("a 16 MiB image with 4 non-zero blocks provisioned %d blocks, want 4", got)
	}
}

// A launcher restart must bring the guest back to ITS disk. Re-snapshotting the
// base would hand it a pristine one, every write gone, and it would boot as if
// new — which is indistinguishable from success unless something checks.
func TestIntegration_RestartReactivatesAndNeverResnapshots(t *testing.T) {
	itEnv(t)
	x, r := materializer(t)
	ctx := context.Background()
	img := image(8 << 20)

	if _, err := x.EnsureBase(ctx, "sha256:restart", uint64(len(img)), opener(img)); err != nil {
		t.Fatal(err)
	}
	path, err := x.EnsureGuest(ctx, "sha256:restart", "ns/g", "ksit-restart", uint64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	r.track("ksit-restart")
	mine := bytes.Repeat([]byte("WRITTEN-BY-THE-GUEST!!!!"), 4096/24+1)[:4096]
	ddWriteAt(t, path, mine, 0)

	// Idempotent while mapped: a second call is a no-op, not an error.
	if _, err := x.EnsureGuest(ctx, "sha256:restart", "ns/g", "ksit-restart", uint64(len(img))); err != nil {
		t.Fatalf("second EnsureGuest while mapped: %v", err)
	}

	// The container restarts: the mapping goes, the thin device stays.
	if err := x.M.RemoveDevice(ctx, "ksit-restart"); err != nil {
		t.Fatal(err)
	}
	path, err = x.EnsureGuest(ctx, "sha256:restart", "ns/g", "ksit-restart", uint64(len(img)))
	if err != nil {
		t.Fatalf("EnsureGuest after restart: %v", err)
	}
	if got := ddRead(t, path, 0, len(mine)); !bytes.Equal(got, mine) {
		t.Fatal("after a restart the guest came back WITHOUT its writes — it was re-snapshotted from the base")
	}
}

// The whole node goes down: every device-mapper device and every loop device is
// gone, and only the backing files and the registry survive. Coming back must
// REOPEN the pool's metadata, not format it — formatting would destroy every
// guest on the node at once.
func TestIntegration_NodeRebootPreservesGuests(t *testing.T) {
	itEnv(t)
	dir := t.TempDir()
	ctx := context.Background()
	pool := "ksit-reboot"
	_ = exec.Command("dmsetup", "remove", "-f", "--noudevsync", pool).Run()

	data := Backing{File: filepath.Join(dir, "data.img"), FileBytes: 256 << 20}
	meta := Backing{File: filepath.Join(dir, "meta.img"), FileBytes: 64 << 20}
	boot := func() *Materializer {
		t.Helper()
		dd, err := data.Resolve(ctx, execRunner{})
		if err != nil {
			t.Fatalf("data backing: %v", err)
		}
		md, err := meta.Resolve(ctx, execRunner{})
		if err != nil {
			t.Fatalf("meta backing: %v", err)
		}
		m := &Manager{Pool: pool, DataDev: dd, MetaDev: md}
		if err := m.EnsurePool(ctx, (256<<20)/sector); err != nil {
			t.Fatalf("EnsurePool: %v", err)
		}
		return &Materializer{M: m, Reg: NewRegistry(DefaultRegistryPath(dir)), LockDir: filepath.Join(dir, "locks")}
	}
	shutdown := func() {
		for _, d := range []string{"ksit-reboot-g", pool} {
			_ = exec.Command("dmsetup", "remove", "-f", "--retry", "--noudevsync", d).Run()
		}
		for _, f := range []string{data.File, meta.File} {
			out, _ := exec.Command("losetup", "-j", f, "--noheadings", "-O", "NAME").Output()
			for _, l := range strings.Fields(string(out)) {
				_ = exec.Command("losetup", "-d", l).Run()
			}
		}
	}
	t.Cleanup(shutdown)

	x := boot()
	img := image(8 << 20)
	if _, err := x.EnsureBase(ctx, "sha256:reboot", uint64(len(img)), opener(img)); err != nil {
		t.Fatal(err)
	}
	path, err := x.EnsureGuest(ctx, "sha256:reboot", "ns/g", "ksit-reboot-g", uint64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	mine := bytes.Repeat([]byte("SURVIVES-A-REBOOT!"), 4096/18+1)[:4096]
	ddWriteAt(t, path, mine, 0)

	shutdown()
	if _, err := os.Stat("/dev/mapper/" + pool); err == nil {
		t.Fatal("the pool is still mapped after shutdown; this is not a reboot")
	}

	x = boot() // nothing carried over but the backing files and the registry
	path, err = x.EnsureGuest(ctx, "sha256:reboot", "ns/g", "ksit-reboot-g", uint64(len(img)))
	if err != nil {
		t.Fatalf("EnsureGuest after reboot: %v — the pool metadata may have been reformatted", err)
	}
	if got := ddRead(t, path, 0, len(mine)); !bytes.Equal(got, mine) {
		t.Fatal("the guest's writes did not survive the reboot")
	}
	if got := ddRead(t, path, int64(len(mine)), 64); !bytes.Equal(got, img[len(mine):len(mine)+64]) {
		t.Error("the base under the guest did not survive the reboot")
	}
}

// Deleting a guest frees its blocks, and a new guest under the SAME name gets a
// fresh disk rather than tripping over the old one's leftovers.
func TestIntegration_ReleaseGuestFreesAndAllowsRecreate(t *testing.T) {
	itEnv(t)
	x, r := materializer(t)
	ctx := context.Background()
	img := image(8 << 20)

	if _, err := x.EnsureBase(ctx, "sha256:rel", uint64(len(img)), opener(img)); err != nil {
		t.Fatal(err)
	}
	path, err := x.EnsureGuest(ctx, "sha256:rel", "ns/g", "ksit-rel", uint64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	ddWriteAt(t, path, bytes.Repeat([]byte("OLD-GUEST-DATA!!"), 64*1024/16), 0)

	before, _ := x.M.Status(ctx)
	if err := x.ReleaseGuest(ctx, "ns/g", "ksit-rel"); err != nil {
		t.Fatalf("ReleaseGuest: %v", err)
	}
	after, _ := x.M.Status(ctx)
	if after.UsedData >= before.UsedData {
		t.Errorf("releasing the guest freed nothing (%d -> %d blocks)", before.UsedData, after.UsedData)
	}

	path, err = x.EnsureGuest(ctx, "sha256:rel", "ns/g", "ksit-rel", uint64(len(img)))
	if err != nil {
		t.Fatalf("recreating a guest under a released name: %v", err)
	}
	r.track("ksit-rel")
	if bytes.Contains(ddRead(t, path, 0, 64*1024), []byte("OLD-GUEST-DATA!!")) {
		t.Fatal("a guest recreated under a released name got the previous guest's data")
	}
}
