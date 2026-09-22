package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	base := config{mode: modeReactivate, guestKey: "ns/g/uid", device: "ks-g-uid", guestBytes: 1 << 30, poolDataBytes: 1 << 30}

	if err := base.validate(); err != nil {
		t.Errorf("a complete reactivate config was rejected: %v", err)
	}

	create := base
	create.mode = modeCreate
	if err := create.validate(); err == nil || !strings.Contains(err.Error(), "--base-key") || !strings.Contains(err.Error(), "--image") {
		t.Errorf("create mode without --base-key/--image should name both: %v", err)
	}

	bad := base
	bad.mode = "materialise"
	if err := bad.validate(); err == nil {
		t.Error("an unknown mode was accepted")
	}

	missing := config{mode: modeReactivate}
	err := missing.validate()
	if err == nil {
		t.Fatal("an empty config was accepted")
	}
	for _, f := range []string{"--guest-key", "--device", "--guest-bytes", "--pool-data-bytes"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("missing-flags error should name %s: %v", f, err)
		}
	}
}

// --- device-mapper integration: the command exactly as the Job and the init
// container run it, against a real pool. Root only, and not in CI.

func itEnv(t *testing.T) {
	t.Helper()
	if os.Getenv("KUBESWIFT_THINPOOL_IT") != "1" {
		t.Skip("set KUBESWIFT_THINPOOL_IT=1 (and run as root) for device-mapper integration tests")
	}
	if os.Geteuid() != 0 {
		t.Skip("device-mapper integration tests need root")
	}
}

// node is one node's state in a temp directory, torn down afterwards.
func node(t *testing.T) config {
	t.Helper()
	root := t.TempDir()
	pool := "ksit-cmd-" + strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '-'
		}
		return r
	}, t.Name())
	_ = exec.Command("dmsetup", "remove", "--retry", "-f", "--noudevsync", pool).Run()
	t.Cleanup(func() {
		out, _ := exec.Command("dmsetup", "ls").Output()
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], "ksit-cmd-dev-") {
				_ = exec.Command("dmsetup", "remove", "--retry", "-f", "--noudevsync", f[0]).Run()
			}
		}
		_ = exec.Command("dmsetup", "remove", "--retry", "-f", "--noudevsync", pool).Run()
		for _, f := range []string{"data.img", "meta.img"} {
			out, _ := exec.Command("losetup", "-j", filepath.Join(root, "thinpool", f), "--noheadings", "-O", "NAME").Output()
			for _, l := range strings.Fields(string(out)) {
				_ = exec.Command("losetup", "-d", l).Run()
			}
		}
	})
	return config{root: root, pool: pool, poolDataBytes: 256 << 20}
}

// gptImage writes a real GPT disk image, so the grow path can be checked with
// sgdisk's own verifier rather than by assumption.
func gptImage(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "image.raw")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, args := range [][]string{{"-o", p}, {"-n", "1:0:0", p}} {
		if out, err := exec.Command("sgdisk", args...).CombinedOutput(); err != nil {
			t.Fatalf("sgdisk %v: %v: %s", args, err, out)
		}
	}
	// Recognisable data in the partition so the guest's view can be checked.
	f, err = os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("IMAGE-PARTITION-DATA"), 2<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func read(t *testing.T, path string, off int64, n int) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	return b
}

func write(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}

// create builds a guest disk from the image, larger than the image, with a GPT
// that sgdisk itself accepts — i.e. the backup header was moved to the end.
func TestIntegration_CreateBuildsAGrownValidGPTDisk(t *testing.T) {
	itEnv(t)
	cfg := node(t)
	cfg.mode = modeCreate
	cfg.baseKey = "uid-img/uid-pvc"
	cfg.image = gptImage(t, 16<<20)
	cfg.guestKey = "ns/g/uid-1"
	cfg.device = "ksit-cmd-dev-1"
	cfg.guestBytes = 64 << 20 // four times the image

	var out bytes.Buffer
	if err := run(context.Background(), cfg, &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(out.String(), "created") {
		t.Errorf("output should say created: %q", out.String())
	}
	dev := "/dev/mapper/" + cfg.device
	if got := read(t, dev, 2<<20, 20); string(got) != "IMAGE-PARTITION-DATA" {
		t.Errorf("the image is not visible on the guest's disk: %q", got)
	}
	// Assert the POSITIVE verdict. sgdisk also prints cautions that contain the
	// word "problems" — "may result in problems with some disk encryption
	// tools", about partition alignment — so looking for "problem" matched a
	// healthy disk during node validation, and looking for "Problem" only works
	// because of its capital letter. "No problems found" is the verdict; with
	// the backup header left mid-disk it is replaced by "Problem: The secondary
	// header's self-pointer...".
	msg, err := exec.Command("sgdisk", "-v", dev).CombinedOutput()
	if err != nil || !strings.Contains(string(msg), "No problems found") {
		t.Errorf("the grown disk's GPT does not verify: %v\n%s", err, msg)
	}
}

// A retried materialise Job finds the guest already built and must reactivate
// it, not build it again.
func TestIntegration_CreateTwiceReactivates(t *testing.T) {
	itEnv(t)
	cfg := node(t)
	cfg.mode = modeCreate
	cfg.baseKey = "uid-img/uid-pvc"
	cfg.image = gptImage(t, 16<<20)
	cfg.guestKey = "ns/g/uid-2"
	cfg.device = "ksit-cmd-dev-2"
	cfg.guestBytes = 32 << 20

	if err := run(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	dev := "/dev/mapper/" + cfg.device
	write(t, dev, 8<<20, []byte("WRITTEN-BEFORE-THE-RETRY"))
	if out, err := exec.Command("dmsetup", "remove", "--retry", "--noudevsync", cfg.device).CombinedOutput(); err != nil {
		t.Fatalf("unmapping: %v: %s", err, out)
	}

	var out bytes.Buffer
	if err := run(context.Background(), cfg, &out); err != nil {
		t.Fatalf("second create: %v", err)
	}
	if !strings.Contains(out.String(), "reactivated") {
		t.Errorf("a retried create should reactivate: %q", out.String())
	}
	if got := read(t, dev, 8<<20, 24); string(got) != "WRITTEN-BEFORE-THE-RETRY" {
		t.Fatalf("the retry rebuilt the disk and lost what was written: %q", got)
	}
}

// THE property of reactivate mode. The launcher only starts after the disk was
// built, so a node with no record of the guest has LOST its disk — and the
// guest may have run and written. A fresh disk here would boot it as if new and
// discard everything, looking like success.
func TestIntegration_ReactivateNeverCreatesAFreshDisk(t *testing.T) {
	itEnv(t)
	cfg := node(t)
	cfg.mode = modeReactivate
	cfg.guestKey = "ns/g/never-built"
	cfg.device = "ksit-cmd-dev-3"
	cfg.guestBytes = 32 << 20

	err := run(context.Background(), cfg, &bytes.Buffer{})
	if err == nil {
		t.Fatal("reactivate created a disk for a guest this node has no record of")
	}
	if !strings.Contains(err.Error(), "refusing to create a fresh one") {
		t.Errorf("refused, but without saying why: %v", err)
	}
	if _, statErr := os.Stat("/dev/mapper/" + cfg.device); statErr == nil {
		t.Error("a device was mapped despite the refusal")
	}
}

// The normal restart path, including after a node reboot, needs no image.
func TestIntegration_ReactivateRestoresTheGuestsOwnDisk(t *testing.T) {
	itEnv(t)
	cfg := node(t)
	cfg.mode = modeCreate
	cfg.baseKey = "uid-img/uid-pvc"
	cfg.image = gptImage(t, 16<<20)
	cfg.guestKey = "ns/g/uid-4"
	cfg.device = "ksit-cmd-dev-4"
	cfg.guestBytes = 32 << 20
	if err := run(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	dev := "/dev/mapper/" + cfg.device
	write(t, dev, 8<<20, []byte("GUEST-DATA-ACROSS-A-RESTART"))
	if out, err := exec.Command("dmsetup", "remove", "--retry", "--noudevsync", cfg.device).CombinedOutput(); err != nil {
		t.Fatalf("unmapping: %v: %s", err, out)
	}

	re := cfg
	re.mode = modeReactivate
	re.baseKey, re.image = "", "" // the launcher has neither
	if err := run(context.Background(), re, &bytes.Buffer{}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if got := read(t, dev, 8<<20, 27); string(got) != "GUEST-DATA-ACROSS-A-RESTART" {
		t.Fatalf("reactivation did not restore the guest's own disk: %q", got)
	}
}

// A guest cannot start smaller than the image it is built from.
func TestIntegration_CreateRefusesToShrinkBelowTheImage(t *testing.T) {
	itEnv(t)
	cfg := node(t)
	cfg.mode = modeCreate
	cfg.baseKey = "uid-img/uid-pvc"
	cfg.image = gptImage(t, 32<<20)
	cfg.guestKey = "ns/g/uid-5"
	cfg.device = "ksit-cmd-dev-5"
	cfg.guestBytes = 16 << 20
	err := run(context.Background(), cfg, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "smaller than its image") {
		t.Fatalf("a disk smaller than its image was accepted: %v", err)
	}
}

// A first attempt that snapshotted the disk and then died before fixing the GPT
// leaves the guest KNOWN to this node. The retry must still move the backup
// header, or it stays mid-disk forever. Create mode only runs before the
// guest's VM ever has, so the partition table is still the image's and this is
// always safe here.
func TestIntegration_RetryAfterPartialCreateStillFixesTheGPT(t *testing.T) {
	itEnv(t)
	cfg := node(t)
	cfg.mode = modeCreate
	cfg.baseKey = "uid-img/uid-pvc"
	cfg.image = gptImage(t, 16<<20)
	cfg.guestKey = "ns/g/uid-partial"
	cfg.device = "ksit-cmd-dev-partial"
	cfg.guestBytes = 64 << 20

	// Reproduce the dead first attempt: base built and guest snapshotted at the
	// grown size, but no sgdisk -e.
	x, err := openNode(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	imgBytes, _ := fileSize(cfg.image)
	if _, err := x.EnsureBase(context.Background(), cfg.baseKey, imgBytes,
		func() (io.ReadCloser, error) { return os.Open(cfg.image) }); err != nil {
		t.Fatal(err)
	}
	if _, err := x.EnsureGuest(context.Background(), cfg.baseKey, cfg.guestKey, cfg.device, cfg.guestBytes); err != nil {
		t.Fatal(err)
	}
	dev := "/dev/mapper/" + cfg.device
	if msg, _ := exec.Command("sgdisk", "-v", dev).CombinedOutput(); strings.Contains(string(msg), "No problems found") {
		t.Fatal("setup did not reproduce a mid-disk backup header; the test would prove nothing")
	}

	if err := run(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatalf("the retried create: %v", err)
	}
	if msg, err := exec.Command("sgdisk", "-v", dev).CombinedOutput(); err != nil || !strings.Contains(string(msg), "No problems found") {
		t.Errorf("the retry left the backup GPT header mid-disk:\n%s", msg)
	}
}

// Raising the configured pool size must not stop existing guests restarting.
// The pool is the size its backing file already is; the setting only sizes a
// pool that does not exist yet.
func TestIntegration_ChangingThePoolSizeDoesNotBreakRestarts(t *testing.T) {
	itEnv(t)
	cfg := node(t)
	cfg.mode = modeCreate
	cfg.baseKey = "uid-img/uid-pvc"
	cfg.image = gptImage(t, 16<<20)
	cfg.guestKey = "ns/g/uid-resize"
	cfg.device = "ksit-cmd-dev-resize"
	cfg.guestBytes = 32 << 20
	if err := run(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("dmsetup", "remove", "--retry", "--noudevsync", cfg.device).CombinedOutput(); err != nil {
		t.Fatalf("unmapping: %v: %s", err, out)
	}

	re := cfg
	re.mode = modeReactivate
	re.baseKey, re.image = "", ""
	re.poolDataBytes = cfg.poolDataBytes * 4 // the operator raised the setting
	if err := run(context.Background(), re, &bytes.Buffer{}); err != nil {
		t.Fatalf("a restart failed after the pool-size setting was raised: %v", err)
	}
}
