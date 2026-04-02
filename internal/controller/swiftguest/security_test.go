package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

func assertPrivileged(t *testing.T, sc *corev1.SecurityContext, name string) {
	t.Helper()
	if sc == nil {
		t.Fatalf("%s: SecurityContext is nil", name)
	}
	if sc.Privileged == nil || !*sc.Privileged {
		t.Errorf("%s: expected privileged=true", name)
	}
}

func TestNetworkInitSecurityContext(t *testing.T) {
	c := networkInitContainer()
	assertPrivileged(t, c.SecurityContext, c.Name)
}

func TestGPUInitSecurityContext(t *testing.T) {
	sc := gpuInitSecurityContext()
	assertPrivileged(t, sc, "gpu-init")
}

func TestLauncherSecurityContext_NoGPU(t *testing.T) {
	sc := launcherSecurityContext(false)
	assertPrivileged(t, sc, "launcher")
}

func TestLauncherSecurityContext_WithGPU(t *testing.T) {
	sc := launcherSecurityContext(true)
	assertPrivileged(t, sc, "launcher-gpu")
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

func TestDiskBootPod_ContainersPrivileged(t *testing.T) {
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
	assertPrivileged(t, pod.Spec.InitContainers[0].SecurityContext, "network-init")
	assertPrivileged(t, pod.Spec.Containers[0].SecurityContext, "launcher")
}

func TestGPUPod_AllContainersPrivileged(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi")

	// gpu-init
	if pod.Spec.InitContainers[0].Name != "gpu-init" {
		t.Fatalf("expected gpu-init, got %s", pod.Spec.InitContainers[0].Name)
	}
	assertPrivileged(t, pod.Spec.InitContainers[0].SecurityContext, "gpu-init")

	// network-init
	if pod.Spec.InitContainers[1].Name != "network-init" {
		t.Fatalf("expected network-init, got %s", pod.Spec.InitContainers[1].Name)
	}
	assertPrivileged(t, pod.Spec.InitContainers[1].SecurityContext, "network-init")

	// launcher
	assertPrivileged(t, pod.Spec.Containers[0].SecurityContext, "launcher")
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
