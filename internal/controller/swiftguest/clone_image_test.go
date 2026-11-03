package swiftguest

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// The clone Job and clone-grow-init used to run a stock ubuntu and
// `apt-get install qemu-utils gdisk` on every clone, putting a network
// round-trip and an apt mirror in the path of every guest creation. Both now
// run the launcher image, which ships those tools.
func TestCloneJob_RunsLauncherImageAndInstallsNothing(t *testing.T) {
	// Both branches: the Filesystem script (cp + qemu-img resize) and the Block
	// script (qemu-img convert) each had their own apt-get, so covering only one
	// would leave the other free to reintroduce it.
	for _, mode := range []string{"Filesystem", "Block"} {
		t.Run(mode, func(t *testing.T) { assertCloneJobClean(t, mode) })
	}
}

func assertCloneJobClean(t *testing.T, volumeMode string) {
	t.Helper()
	scheme := rootdiskScheme(t)
	guest := makeGuestObj("g1", "default")
	src := makeSourceImagePVC("src-img", "default", "longhorn", "40Gi")
	clone := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: RootDiskCloneName("g1"), Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, schema.GroupVersionKind{
				Group: "swift.kubeswift.io", Version: "v1alpha1", Kind: "SwiftGuest",
			})},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, src, clone).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme}
	rg := &resolved.ResolvedGuest{
		PreparedImage: resolved.PreparedImage{PVCName: "src-img"},
		RootDisk:      resolved.RootDisk{Size: resource.MustParse("40Gi")},
		Storage:       resolved.Storage{VolumeMode: volumeMode},
	}
	_, _ = r.EnsureRootDiskClone(context.Background(), guest, rg)

	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Name: CloneJobName("g1"), Namespace: "default"}, &job); err != nil {
		t.Fatalf("clone Job not created: %v", err)
	}
	ctr := job.Spec.Template.Spec.Containers[0]
	if ctr.Image != LauncherImage() {
		t.Errorf("clone Job image = %q, want the launcher image %q", ctr.Image, LauncherImage())
	}
	script := strings.Join(ctr.Command, " ")
	if strings.Contains(script, "apt-get") {
		t.Errorf("clone Job still installs packages at run time:\n%s", script)
	}
	// The tools it relies on must actually be invoked, so the test fails if a
	// future edit drops them along with the install.
	for _, tool := range []string{"qemu-img", "sgdisk"} {
		if !strings.Contains(script, tool) {
			t.Errorf("%s clone script no longer uses %s; the image requirement may have drifted", volumeMode, tool)
		}
	}
	assertSgdiskRetried(t, script)
}

// assertSgdiskRetried checks #875 and #912: every `sgdisk -e X` is the shared
// growPartitionTable step, which retries a failure once and leaves a disk
// without a GPT unchanged.
func assertSgdiskRetried(t *testing.T, script string) {
	t.Helper()
	m := regexp.MustCompile(`sgdisk -e (\S+) 2>&1`).FindStringSubmatch(script)
	if m == nil || strings.Count(script, "sgdisk -e") != 2 || !strings.Contains(script, growPartitionTable(m[1])) {
		t.Errorf("sgdisk -e must run through growPartitionTable (#875, #912):\n%s", script)
	}
}

// clone-grow-init runs inside the launcher pod, so reusing the launcher image
// costs no extra pull at all.
func TestCloneGrowInit_RunsLauncherImageAndInstallsNothing(t *testing.T) {
	for _, mode := range []string{"Filesystem", "Block"} {
		isBlock := mode == "Block"
		rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: mode}}
		ctr := cloneGrowInitContainer(rg, 42*1024*1024*1024)
		if ctr.Image != LauncherImage() {
			t.Errorf("block=%v: image = %q, want the launcher image %q", isBlock, ctr.Image, LauncherImage())
		}
		script := strings.Join(ctr.Command, " ")
		if strings.Contains(script, "apt-get") {
			t.Errorf("block=%v: clone-grow-init still installs packages at run time:\n%s", isBlock, script)
		}
		if !strings.Contains(script, "sgdisk") {
			t.Errorf("block=%v: script no longer uses sgdisk", isBlock)
		}
		assertSgdiskRetried(t, script)
	}
}

// The launcher image is configurable, and the clone path must follow it rather
// than pinning a second image that upgrades separately.
func TestCloneImages_FollowLauncherImageOverride(t *testing.T) {
	t.Setenv(LauncherImageEnv, "registry.example.com/kubeswift/swiftletd:v9.9.9")
	if got := CloneJobImage(); got != os.Getenv(LauncherImageEnv) {
		t.Errorf("CloneJobImage() = %q, want the overridden launcher image", got)
	}
	if got := CloneGrowInitImage(); got != os.Getenv(LauncherImageEnv) {
		t.Errorf("CloneGrowInitImage() = %q, want the overridden launcher image", got)
	}
}

// growPartitionTable on real disks: a grown GPT gets its backup header moved, an
// MBR disk and a hybrid ISO are left byte for byte as they were (#912), and any
// other failure still fails the step. Needs sgdisk, sfdisk and xorriso.
func TestGrowPartitionTable_Disks(t *testing.T) {
	for _, tool := range []string{"sgdisk", "sfdisk", "xorriso", "dash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	grow := func(disk string) ([]byte, error) {
		return exec.Command("dash", "-ec", growPartitionTable(disk)).CombinedOutput()
	}
	grown := func(disk string) {
		t.Helper()
		if err := os.Truncate(disk, 64<<20); err != nil {
			t.Fatal(err)
		}
	}

	gpt := filepath.Join(dir, "gpt.raw")
	run("truncate", "-s", "16M", gpt)
	run("sgdisk", "-n", "1:0:0", gpt)
	grown(gpt)
	if out, err := grow(gpt); err != nil {
		t.Fatalf("gpt: %v\n%s", err, out)
	}
	if out, err := exec.Command("sgdisk", "-v", gpt).CombinedOutput(); err != nil || !strings.Contains(string(out), "No problems found") {
		t.Errorf("gpt not repaired: %v\n%s", err, out)
	}

	mbr := filepath.Join(dir, "mbr.raw")
	run("truncate", "-s", "16M", mbr)
	run("sh", "-c", "echo 'type=83' | sfdisk -q "+mbr)
	iso := filepath.Join(dir, "hybrid.iso")
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "EFI"), 0o755); err != nil {
		t.Fatal(err)
	}
	efi := filepath.Join(dir, "efi.img")
	run("truncate", "-s", "2M", efi)
	run("xorriso", "-as", "mkisofs", "-o", iso, "-append_partition", "2", "0xef", efi, src)
	// An isohybrid ISO (Alpine's, for one): a GPT plus an MBR whose first
	// partition starts at sector 0. sgdisk exits 2, "Invalid partition data".
	isohybrid := filepath.Join(dir, "isohybrid.raw")
	run("truncate", "-s", "16M", isohybrid)
	run("sgdisk", "-n", "1:2048:4095", "-t", "1:ef00", isohybrid)
	mbrEntries := make([]byte, 66)
	mbrEntries[0] = 0x80
	binary.LittleEndian.PutUint32(mbrEntries[12:], 32768)
	mbrEntries[16+4] = 0xef
	binary.LittleEndian.PutUint32(mbrEntries[16+8:], 2048)
	binary.LittleEndian.PutUint32(mbrEntries[16+12:], 2048)
	mbrEntries[64], mbrEntries[65] = 0x55, 0xaa
	f, err := os.OpenFile(isohybrid, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt(mbrEntries, 446)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, disk := range []string{mbr, iso, isohybrid} {
		grown(disk)
		before, _ := os.ReadFile(disk)
		out, err := grow(disk)
		if err != nil || !strings.Contains(string(out), "partition table left unchanged") {
			t.Errorf("%s: %v\n%s", filepath.Base(disk), err, out)
		}
		if after, _ := os.ReadFile(disk); !bytes.Equal(before, after) {
			t.Errorf("%s was modified", filepath.Base(disk))
		}
	}

	if out, err := grow(filepath.Join(dir, "missing.raw")); err == nil {
		t.Errorf("a missing disk must fail the step:\n%s", out)
	}
}
