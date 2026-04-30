package swiftguest

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
)

// W9 Component 2 tests for the launcher pod builder + clone-grow-init
// Block path. Coverage:
//
//  1. rootDiskMount helper: Filesystem returns VolumeMount only, Block
//     returns VolumeDevice only. Centralised source of truth.
//  2. AddVolumeMounts: branches root-disk on rg.Storage.VolumeMode;
//     other volumes (run, runtime-intent, dev-kvm, seed) unchanged.
//  3. cloneGrowInitContainer: Filesystem byte-identical script + mount,
//     Block uses VolumeDevices + sgdisk-only script (no qemu-img resize).
//  4. BuildPod end-to-end: Block guest produces a launcher container
//     with VolumeDevices for root-disk and no /var/lib/kubeswift/disks/root
//     filesystem mount for that volume.
//  5. Mixed Block-root + Filesystem-data (architect Q4): launcher
//     container has both VolumeDevices + VolumeMounts with no overlap.
//  6. Constant agreement: controller-side and runtimeintent-side
//     DiskRootDevicePath constants must be byte-identical (cross-package
//     contract; the two packages can't share constants without a cycle).

func TestRootDiskMount_FilesystemReturnsVolumeMountOnly(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: "Filesystem"}}
	mount, device := rootDiskMount(rg)
	if mount == nil {
		t.Fatal("Filesystem mode should return non-nil VolumeMount")
	}
	if device != nil {
		t.Errorf("Filesystem mode should return nil VolumeDevice; got %+v", device)
	}
	if mount.Name != "root-disk" || mount.MountPath != DisksRootPath {
		t.Errorf("VolumeMount = %+v, want {root-disk, %s}", mount, DisksRootPath)
	}
}

func TestRootDiskMount_BlockReturnsVolumeDeviceOnly(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: "Block"}}
	mount, device := rootDiskMount(rg)
	if mount != nil {
		t.Errorf("Block mode should return nil VolumeMount; got %+v", mount)
	}
	if device == nil {
		t.Fatal("Block mode should return non-nil VolumeDevice")
	}
	if device.Name != "root-disk" || device.DevicePath != DiskRootDevicePath {
		t.Errorf("VolumeDevice = %+v, want {root-disk, %s}", device, DiskRootDevicePath)
	}
}

func TestRootDiskMount_EmptyVolumeModeDefaultsToFilesystem(t *testing.T) {
	// Pre-W9 SwiftGuests have no spec.storage; resolution defaults to
	// Filesystem, but the resolved struct may pass through empty in
	// edge cases (e.g. partial resolution during cluster restore).
	// Treat empty as Filesystem to match the kubelet's default behaviour.
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{}}
	mount, device := rootDiskMount(rg)
	if mount == nil || device != nil {
		t.Errorf("empty VolumeMode should resolve to Filesystem (mount=%+v device=%+v)", mount, device)
	}
}

// TestAddVolumeMounts_FilesystemUnchanged is the regression gate for
// the launcher container's mount surface. Pre-W9 callers got 4 mounts
// (run, root-disk, runtime-intent, dev-kvm) plus optionally seed, with
// no VolumeDevices. The W9 signature changed but Filesystem-mode
// callers must still get the same 4-or-5 mounts and zero VolumeDevices.
func TestAddVolumeMounts_FilesystemUnchanged(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: "Filesystem"}}

	t.Run("no seed", func(t *testing.T) {
		var mounts []corev1.VolumeMount
		var devices []corev1.VolumeDevice
		AddVolumeMounts(&mounts, &devices, rg, false)
		assertMountNames(t, mounts, []string{"run", "root-disk", "runtime-intent", "dev-kvm"})
		if len(devices) != 0 {
			t.Errorf("Filesystem-mode AddVolumeMounts should produce zero VolumeDevices; got %v", devices)
		}
		// root-disk must mount at the filesystem path, not the device path.
		for _, m := range mounts {
			if m.Name == "root-disk" {
				if m.MountPath != DisksRootPath {
					t.Errorf("root-disk MountPath = %q, want %q", m.MountPath, DisksRootPath)
				}
				if m.MountPath == DiskRootDevicePath {
					t.Errorf("root-disk Filesystem mount must NOT be the device path")
				}
			}
		}
	})

	t.Run("with seed", func(t *testing.T) {
		var mounts []corev1.VolumeMount
		var devices []corev1.VolumeDevice
		AddVolumeMounts(&mounts, &devices, rg, true)
		assertMountNames(t, mounts, []string{"run", "root-disk", "runtime-intent", "dev-kvm", "seed"})
		if len(devices) != 0 {
			t.Errorf("Filesystem-mode AddVolumeMounts should produce zero VolumeDevices; got %v", devices)
		}
	})
}

// TestAddVolumeMounts_BlockUsesVolumeDevices is the forward contract.
// Block destinations get root-disk on the VolumeDevices list (DevicePath
// = /dev/kubeswift-root) and NOT on VolumeMounts. Other mounts (run,
// runtime-intent, dev-kvm, seed) are unchanged.
func TestAddVolumeMounts_BlockUsesVolumeDevices(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: "Block"}}

	t.Run("no seed", func(t *testing.T) {
		var mounts []corev1.VolumeMount
		var devices []corev1.VolumeDevice
		AddVolumeMounts(&mounts, &devices, rg, false)
		// root-disk is NOT in VolumeMounts.
		assertMountNames(t, mounts, []string{"run", "runtime-intent", "dev-kvm"})
		// root-disk IS in VolumeDevices.
		if len(devices) != 1 {
			t.Fatalf("VolumeDevices = %d, want 1 (root-disk)", len(devices))
		}
		if devices[0].Name != "root-disk" || devices[0].DevicePath != DiskRootDevicePath {
			t.Errorf("VolumeDevice = %+v, want {root-disk, %s}", devices[0], DiskRootDevicePath)
		}
	})

	t.Run("with seed", func(t *testing.T) {
		var mounts []corev1.VolumeMount
		var devices []corev1.VolumeDevice
		AddVolumeMounts(&mounts, &devices, rg, true)
		assertMountNames(t, mounts, []string{"run", "runtime-intent", "dev-kvm", "seed"})
		if len(devices) != 1 {
			t.Errorf("VolumeDevices = %d, want 1 (root-disk)", len(devices))
		}
	})
}

// TestCloneGrowInit_FilesystemUnchanged is the regression gate for the
// clone-grow-init init container. Filesystem path is byte-identical to
// pre-W9: qemu-img resize + sgdisk -e, with the volume mounted at the
// filesystem path. No VolumeDevices.
func TestCloneGrowInit_FilesystemUnchanged(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: "Filesystem"}}
	c := cloneGrowInitContainer(rg, 42949672960) // 40 GiB

	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != "root-disk" || c.VolumeMounts[0].MountPath != DisksRootPath {
		t.Errorf("Filesystem clone-grow-init VolumeMounts = %+v, want [{root-disk, %s}]", c.VolumeMounts, DisksRootPath)
	}
	if len(c.VolumeDevices) != 0 {
		t.Errorf("Filesystem clone-grow-init must have zero VolumeDevices; got %v", c.VolumeDevices)
	}
	script := strings.Join(c.Command, " ")
	for _, want := range []string{"qemu-img resize -f raw " + DisksRootPath + "/image.raw", "sgdisk -e " + DisksRootPath + "/image.raw"} {
		if !strings.Contains(script, want) {
			t.Errorf("Filesystem clone-grow-init script missing %q; got:\n%s", want, script)
		}
	}
	if strings.Contains(script, DiskRootDevicePath) {
		t.Errorf("Filesystem clone-grow-init script must NOT reference %s; got:\n%s", DiskRootDevicePath, script)
	}
}

// TestCloneGrowInit_BlockUsesVolumeDevicesAndSkipsResize is the forward
// contract: Block-mode clone-grow-init uses VolumeDevices for root-disk,
// runs sgdisk -e on the device path, and DOES NOT run qemu-img resize
// (no-op on block devices, scoping doc decision (c)).
func TestCloneGrowInit_BlockUsesVolumeDevicesAndSkipsResize(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: "Block"}}
	c := cloneGrowInitContainer(rg, 42949672960)

	if len(c.VolumeMounts) != 0 {
		t.Errorf("Block clone-grow-init must have zero VolumeMounts; got %v", c.VolumeMounts)
	}
	if len(c.VolumeDevices) != 1 || c.VolumeDevices[0].Name != "root-disk" || c.VolumeDevices[0].DevicePath != DiskRootDevicePath {
		t.Errorf("Block clone-grow-init VolumeDevices = %+v, want [{root-disk, %s}]", c.VolumeDevices, DiskRootDevicePath)
	}
	script := strings.Join(c.Command, " ")
	if !strings.Contains(script, "sgdisk -e "+DiskRootDevicePath) {
		t.Errorf("Block clone-grow-init script must run sgdisk -e %s; got:\n%s", DiskRootDevicePath, script)
	}
	// qemu-img resize is a no-op on block devices and is skipped per
	// scoping doc decision (c). If a future commit adds it back as a
	// no-op, this test fails; reviewers should reject the change unless
	// the no-op is justified.
	if strings.Contains(script, "qemu-img resize") {
		t.Errorf("Block clone-grow-init script must NOT include qemu-img resize (no-op on block devices); got:\n%s", script)
	}
}

// TestBuildPod_BlockGuestUsesVolumeDevices is the end-to-end pod-spec
// contract: a SwiftGuest with spec.storage.volumeMode=Block produces a
// launcher pod whose launcher container has VolumeDevices[root-disk]
// at /dev/kubeswift-root, and zero root-disk VolumeMounts. Kubelet
// would otherwise refuse the pod with the W9 error message.
func TestBuildPod_BlockGuestUsesVolumeDevices(t *testing.T) {
	guest := newDiskBootGuest("block-vm")
	rg := newDiskBootResolved("block-vm", "default", resolved.Storage{
		AccessMode: "ReadWriteMany",
		VolumeMode: "Block",
	})
	pod := BuildPod(guest, rg, "block-vm-seed", "block-vm-runtime-intent", nil)
	launcher := pod.Spec.Containers[0]

	for _, m := range launcher.VolumeMounts {
		if m.Name == "root-disk" {
			t.Errorf("Block-mode launcher must NOT have root-disk VolumeMount; got %+v", m)
		}
	}
	foundDevice := false
	for _, d := range launcher.VolumeDevices {
		if d.Name == "root-disk" {
			foundDevice = true
			if d.DevicePath != DiskRootDevicePath {
				t.Errorf("root-disk VolumeDevice DevicePath = %q, want %q", d.DevicePath, DiskRootDevicePath)
			}
		}
	}
	if !foundDevice {
		t.Errorf("Block-mode launcher must have a root-disk VolumeDevice; got mounts=%v devices=%v", launcher.VolumeMounts, launcher.VolumeDevices)
	}

	// No-overlap invariant: same volume name appears at most once across
	// mounts and devices for the launcher container. This is the
	// kubelet-side contract that surfaced as W9.
	assertNoVolumeNameOverlap(t, launcher)
}

// TestBuildPod_BlockRootWithFilesystemDataDisk locks in architect Q4:
// a guest with Block-mode root + Filesystem-mode dataDisk produces a
// launcher container with both VolumeDevices (root) and VolumeMounts
// (data), no overlap. This is the cross-feature compose case the
// architect specifically asked for a unit test on.
func TestBuildPod_BlockRootWithFilesystemDataDisk(t *testing.T) {
	guest := newDiskBootGuest("mixed-vm")
	guest.Spec.DataDiskRef = &corev1.LocalObjectReference{Name: "data-img"}
	rg := newDiskBootResolved("mixed-vm", "default", resolved.Storage{
		AccessMode: "ReadWriteMany",
		VolumeMode: "Block",
	})
	rg.DataDisk = &resolved.PreparedImage{Path: "/var/lib/kubeswift/disks/data/image.raw", PVCName: "data-pvc", Ready: true}
	pod := BuildPod(guest, rg, "mixed-vm-seed", "mixed-vm-runtime-intent", nil)
	launcher := pod.Spec.Containers[0]

	// Root-disk on VolumeDevices.
	hasRootDevice := false
	for _, d := range launcher.VolumeDevices {
		if d.Name == "root-disk" && d.DevicePath == DiskRootDevicePath {
			hasRootDevice = true
		}
	}
	if !hasRootDevice {
		t.Errorf("expected root-disk VolumeDevice at %s; got %v", DiskRootDevicePath, launcher.VolumeDevices)
	}

	// Data-disk on VolumeMounts (Filesystem-mode dataDisk PVC, independent
	// of root-disk volumeMode per PR #32 scope: spec.storage applies to
	// controller-created PVCs only — DataDiskRefs are referenced PVCs
	// with their own volumeMode set elsewhere).
	hasDataMount := false
	for _, m := range launcher.VolumeMounts {
		if m.Name == "data-disk" && m.MountPath == DisksDataPath {
			hasDataMount = true
		}
	}
	if !hasDataMount {
		t.Errorf("expected data-disk VolumeMount at %s; got %v", DisksDataPath, launcher.VolumeMounts)
	}

	assertNoVolumeNameOverlap(t, launcher)
}

// TestBuildPod_FilesystemGuestRegression confirms the default path is
// byte-identical to pre-W9: launcher has VolumeMounts including
// root-disk at the filesystem path, zero root-disk VolumeDevices.
// Existing TestBuildPod_* tests in pod_test.go also exercise this; this
// adds an explicit Block-vs-Filesystem A/B contrast.
func TestBuildPod_FilesystemGuestRegression(t *testing.T) {
	guest := newDiskBootGuest("fs-vm")
	rg := newDiskBootResolved("fs-vm", "default", resolved.Storage{
		AccessMode: "ReadWriteOnce",
		VolumeMode: "Filesystem",
	})
	pod := BuildPod(guest, rg, "fs-vm-seed", "fs-vm-runtime-intent", nil)
	launcher := pod.Spec.Containers[0]

	hasRootMount := false
	for _, m := range launcher.VolumeMounts {
		if m.Name == "root-disk" && m.MountPath == DisksRootPath {
			hasRootMount = true
		}
	}
	if !hasRootMount {
		t.Errorf("Filesystem-mode launcher must have root-disk VolumeMount at %s; got %v", DisksRootPath, launcher.VolumeMounts)
	}
	for _, d := range launcher.VolumeDevices {
		if d.Name == "root-disk" {
			t.Errorf("Filesystem-mode launcher must NOT have root-disk VolumeDevice; got %+v", d)
		}
	}
	assertNoVolumeNameOverlap(t, launcher)
}

// TestDiskRootDevicePath_AgreesAcrossPackages locks in the cross-package
// constant invariant. The runtimeintent package and the swiftguest
// controller package each define DiskRootDevicePath because they
// cannot import each other (controller imports runtimeintent, never
// the other way around). Drift between the two is a runtime hazard:
// the launcher pod would attach the device at one path while
// swiftletd (which reads the runtimeintent constant via the JSON
// payload's RootDisk.Path) tells CH to open it at the other.
func TestDiskRootDevicePath_AgreesAcrossPackages(t *testing.T) {
	if DiskRootDevicePath != runtimeintent.DiskRootDevicePath {
		t.Errorf("DiskRootDevicePath drift: controller=%q runtimeintent=%q",
			DiskRootDevicePath, runtimeintent.DiskRootDevicePath)
	}
}

// --- helpers ---

func newDiskBootGuest(name string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("guest-uid-" + name),
		},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "ubuntu"},
			GuestClassRef: corev1.LocalObjectReference{Name: "default"},
		},
	}
}

func newDiskBootResolved(name, namespace string, storage resolved.Storage) *resolved.ResolvedGuest {
	return &resolved.ResolvedGuest{
		Meta:      resolved.Meta{Name: name, Namespace: namespace, UID: types.UID("guest-uid-" + name)},
		Resources: resolved.Resources{CPU: 2, Memory: 2048},
		RootDisk:  resolved.RootDisk{Size: resource.MustParse("40Gi"), Format: "raw"},
		PreparedImage: resolved.PreparedImage{
			PVCName: "swiftguest-root-" + name,
			Format:  "raw",
			Size:    10737418240,
			Ready:   true,
		},
		Lifecycle: resolved.Lifecycle{RunPolicy: "Running"},
		Network:   true,
		Storage:   storage,
	}
}

func assertMountNames(t *testing.T, mounts []corev1.VolumeMount, want []string) {
	t.Helper()
	got := make([]string, 0, len(mounts))
	for _, m := range mounts {
		got = append(got, m.Name)
	}
	if !sameStringSlice(got, want) {
		t.Errorf("mount names = %v, want %v", got, want)
	}
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertNoVolumeNameOverlap(t *testing.T, c corev1.Container) {
	t.Helper()
	seen := map[string]string{}
	for _, m := range c.VolumeMounts {
		seen[m.Name] = "VolumeMount"
	}
	for _, d := range c.VolumeDevices {
		if existing, ok := seen[d.Name]; ok {
			t.Errorf("volume %q appears as %s and VolumeDevice — kubelet rejects this", d.Name, existing)
		}
		seen[d.Name] = "VolumeDevice"
	}
}
