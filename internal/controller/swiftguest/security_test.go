package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

// assertNotPrivileged checks that the container is not running privileged.
func assertNotPrivileged(t *testing.T, c corev1.Container) {
	t.Helper()
	if c.SecurityContext == nil {
		t.Fatalf("%s: SecurityContext is nil", c.Name)
	}
	if c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
		t.Errorf("%s: Privileged should be false", c.Name)
	}
}

// assertDropsAll checks that the container drops ALL capabilities.
func assertDropsAll(t *testing.T, c corev1.Container) {
	t.Helper()
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		t.Fatalf("%s: SecurityContext or Capabilities is nil", c.Name)
	}
	found := false
	for _, d := range c.SecurityContext.Capabilities.Drop {
		if d == "ALL" {
			found = true
		}
	}
	if !found {
		t.Errorf("%s: should drop ALL capabilities, got: %v", c.Name, c.SecurityContext.Capabilities.Drop)
	}
}

// assertHasCap checks that the container adds a specific capability.
func assertHasCap(t *testing.T, c corev1.Container, cap corev1.Capability) {
	t.Helper()
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		t.Fatalf("%s: SecurityContext or Capabilities is nil", c.Name)
	}
	for _, a := range c.SecurityContext.Capabilities.Add {
		if a == cap {
			return
		}
	}
	t.Errorf("%s: missing capability %s, got: %v", c.Name, cap, c.SecurityContext.Capabilities.Add)
}

// assertNoCap checks that the container does NOT add a specific capability.
func assertNoCap(t *testing.T, c corev1.Container, cap corev1.Capability) {
	t.Helper()
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return // no caps is fine
	}
	for _, a := range c.SecurityContext.Capabilities.Add {
		if a == cap {
			t.Errorf("%s: should NOT have capability %s", c.Name, cap)
		}
	}
}

func TestNetworkInitSecurityContext(t *testing.T) {
	c := networkInitContainer()

	assertNotPrivileged(t, c)
	assertDropsAll(t, c)
	assertHasCap(t, c, "NET_ADMIN")
	assertHasCap(t, c, "NET_RAW")

	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false")
	}

	// Should NOT have SYS_ADMIN — network-init only manipulates network interfaces.
	assertNoCap(t, c, "SYS_ADMIN")
}

func TestGPUInitSecurityContext(t *testing.T) {
	sc := gpuInitSecurityContext()

	if sc.Privileged != nil && *sc.Privileged {
		t.Error("gpu-init should not be privileged")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false")
	}

	foundDrop := false
	for _, d := range sc.Capabilities.Drop {
		if d == "ALL" {
			foundDrop = true
		}
	}
	if !foundDrop {
		t.Error("should drop ALL capabilities")
	}

	foundSysAdmin := false
	for _, a := range sc.Capabilities.Add {
		if a == "SYS_ADMIN" {
			foundSysAdmin = true
		}
	}
	if !foundSysAdmin {
		t.Error("gpu-init should have SYS_ADMIN capability")
	}
}

func TestLauncherSecurityContext_NoGPU(t *testing.T) {
	sc := launcherSecurityContext(false)

	if sc.Privileged != nil && *sc.Privileged {
		t.Error("launcher should not be privileged")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation should be false")
	}

	// Non-GPU launcher needs NET_ADMIN + SYS_ADMIN.
	caps := map[corev1.Capability]bool{}
	for _, a := range sc.Capabilities.Add {
		caps[a] = true
	}
	if !caps["NET_ADMIN"] {
		t.Error("missing NET_ADMIN")
	}
	if !caps["SYS_ADMIN"] {
		t.Error("missing SYS_ADMIN")
	}
	// Should NOT have GPU-specific capabilities.
	if caps["DAC_OVERRIDE"] {
		t.Error("non-GPU launcher should not have DAC_OVERRIDE")
	}
	if caps["SYS_RESOURCE"] {
		t.Error("non-GPU launcher should not have SYS_RESOURCE")
	}
}

func TestLauncherSecurityContext_WithGPU(t *testing.T) {
	sc := launcherSecurityContext(true)

	if sc.Privileged != nil && *sc.Privileged {
		t.Error("launcher should not be privileged")
	}

	caps := map[corev1.Capability]bool{}
	for _, a := range sc.Capabilities.Add {
		caps[a] = true
	}
	if !caps["NET_ADMIN"] {
		t.Error("missing NET_ADMIN")
	}
	if !caps["SYS_ADMIN"] {
		t.Error("missing SYS_ADMIN")
	}
	if !caps["SYS_RESOURCE"] {
		t.Error("GPU launcher missing SYS_RESOURCE")
	}
	if !caps["DAC_OVERRIDE"] {
		t.Error("GPU launcher missing DAC_OVERRIDE")
	}
}

func TestNonGPUPod_NoGPUInit(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "vanilla", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "test-seed", "test-intent")

	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == "gpu-init" {
			t.Error("non-GPU pod should not have gpu-init container")
		}
	}
}

func TestDiskBootPod_NetworkInitHardened(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "test-seed", "test-intent")

	if len(pod.Spec.InitContainers) < 1 {
		t.Fatal("expected at least 1 init container")
	}
	ni := pod.Spec.InitContainers[0]
	if ni.Name != "network-init" {
		t.Fatalf("expected network-init, got %s", ni.Name)
	}

	assertNotPrivileged(t, ni)
	assertDropsAll(t, ni)
	assertHasCap(t, ni, "NET_ADMIN")
	assertHasCap(t, ni, "NET_RAW")
}

func TestDiskBootPod_LauncherHardened(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
		},
	}
	rg := &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 2, Memory: 2048},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}

	pod := BuildPod(guest, rg, "test-seed", "test-intent")

	launcher := pod.Spec.Containers[0]
	assertNotPrivileged(t, launcher)
	assertDropsAll(t, launcher)
	assertHasCap(t, launcher, "NET_ADMIN")
	assertHasCap(t, launcher, "SYS_ADMIN")
	// Non-GPU launcher should NOT have GPU caps.
	assertNoCap(t, launcher, "DAC_OVERRIDE")
	assertNoCap(t, launcher, "SYS_RESOURCE")
}

func TestGPUPod_AllContainersHardened(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi")

	// gpu-init
	gpuInit := pod.Spec.InitContainers[0]
	if gpuInit.Name != "gpu-init" {
		t.Fatalf("expected gpu-init, got %s", gpuInit.Name)
	}
	assertNotPrivileged(t, gpuInit)
	assertDropsAll(t, gpuInit)
	assertHasCap(t, gpuInit, "SYS_ADMIN")
	assertNoCap(t, gpuInit, "NET_ADMIN")

	// network-init
	networkInit := pod.Spec.InitContainers[1]
	if networkInit.Name != "network-init" {
		t.Fatalf("expected network-init, got %s", networkInit.Name)
	}
	assertNotPrivileged(t, networkInit)
	assertDropsAll(t, networkInit)
	assertHasCap(t, networkInit, "NET_ADMIN")
	assertHasCap(t, networkInit, "NET_RAW")

	// launcher (GPU path)
	launcher := pod.Spec.Containers[0]
	assertNotPrivileged(t, launcher)
	assertDropsAll(t, launcher)
	assertHasCap(t, launcher, "NET_ADMIN")
	assertHasCap(t, launcher, "SYS_ADMIN")
	assertHasCap(t, launcher, "SYS_RESOURCE")
	assertHasCap(t, launcher, "DAC_OVERRIDE")
}

func TestGPUPod_SysfsPCIVolume(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi")

	found := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "sysfs-pci" {
			found = true
			if v.VolumeSource.HostPath == nil || v.VolumeSource.HostPath.Path != "/sys/bus/pci" {
				t.Errorf("sysfs-pci volume path = %v, want /sys/bus/pci", v.VolumeSource.HostPath)
			}
		}
	}
	if !found {
		t.Error("GPU pod should have sysfs-pci volume")
	}

	// Verify gpu-init mounts it.
	gpuInit := pod.Spec.InitContainers[0]
	mountFound := false
	for _, m := range gpuInit.VolumeMounts {
		if m.Name == "sysfs-pci" && m.MountPath == "/sys/bus/pci" {
			mountFound = true
		}
	}
	if !mountFound {
		t.Error("gpu-init should mount sysfs-pci at /sys/bus/pci")
	}
}

func TestIsFMPartitionOwnedBy(t *testing.T) {
	fm := &gpuv1alpha1.FabricManagerStatus{
		Installed: true,
		Running:   true,
		Partitions: []gpuv1alpha1.FMPartitionStatus{
			{ID: 0, GPUIndices: []int{0, 1}, AllocatedTo: "default/guest-a"},
			{ID: 1, GPUIndices: []int{2, 3}, AllocatedTo: ""},
		},
	}

	if !isFMPartitionOwnedBy(fm, 0, "default/guest-a") {
		t.Error("partition 0 should be owned by default/guest-a")
	}
	if isFMPartitionOwnedBy(fm, 0, "default/guest-b") {
		t.Error("partition 0 should NOT be owned by default/guest-b")
	}
	if isFMPartitionOwnedBy(fm, 1, "default/guest-a") {
		t.Error("partition 1 is not allocated to guest-a")
	}
	if isFMPartitionOwnedBy(fm, 99, "default/guest-a") {
		t.Error("partition 99 does not exist")
	}
	if isFMPartitionOwnedBy(nil, 0, "default/guest-a") {
		t.Error("nil FM should return false")
	}
}
