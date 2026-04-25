package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
)

func gpuGuest(nodeName string, devices []string, partitionID int) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-test", Namespace: "default"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:       &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef:  corev1.LocalObjectReference{Name: "class"},
			SeedProfileRef: &corev1.LocalObjectReference{Name: "seed"},
			GPUProfileRef:  &corev1.LocalObjectReference{Name: "gpu-profile"},
		},
		Status: swiftv1alpha1.SwiftGuestStatus{
			GPU: &swiftv1alpha1.GPUStatus{
				Devices:     devices,
				PartitionID: partitionID,
				NUMANodes:   []int{0},
				Hypervisor:  "qemu",
				NodeName:    nodeName,
			},
		},
	}
}

func gpuResolvedGuest() *resolved.ResolvedGuest {
	return &resolved.ResolvedGuest{
		Resources:     resolved.Resources{CPU: 16, Memory: 32768},
		PreparedImage: resolved.PreparedImage{PVCName: "pvc-root"},
		Seed:          &resolved.Seed{Datasource: "NoCloud", UserData: "x", MetaData: "y"},
		Network:       true,
	}
}

func TestBuildGPUDiskBootPod_NodeSelector(t *testing.T) {
	guest := gpuGuest("gpu-node-42", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	hostname, ok := pod.Spec.NodeSelector["kubernetes.io/hostname"]
	if !ok {
		t.Fatal("missing nodeSelector kubernetes.io/hostname")
	}
	if hostname != "gpu-node-42" {
		t.Errorf("hostname = %q, want gpu-node-42", hostname)
	}
}

func TestBuildGPUDiskBootPod_InitContainers(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0", "0000:3d:00.0"}, 2)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	if len(pod.Spec.InitContainers) < 2 {
		t.Fatalf("initContainers = %d, want at least 2 (gpu-init + network-init)", len(pod.Spec.InitContainers))
	}

	// gpu-init must run first.
	if pod.Spec.InitContainers[0].Name != "gpu-init" {
		t.Errorf("first init container = %q, want gpu-init", pod.Spec.InitContainers[0].Name)
	}
	if pod.Spec.InitContainers[1].Name != "network-init" {
		t.Errorf("second init container = %q, want network-init", pod.Spec.InitContainers[1].Name)
	}

	// Verify gpu-init env vars.
	gpuInit := pod.Spec.InitContainers[0]
	envMap := map[string]string{}
	for _, e := range gpuInit.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["GPU_PCI_ADDRESSES"] != "0000:17:00.0,0000:3d:00.0" {
		t.Errorf("GPU_PCI_ADDRESSES = %q, want comma-separated addresses", envMap["GPU_PCI_ADDRESSES"])
	}
	if envMap["GPU_PARTITION_ID"] != "2" {
		t.Errorf("GPU_PARTITION_ID = %q, want 2", envMap["GPU_PARTITION_ID"])
	}
}

func TestBuildGPUDiskBootPod_Volumes(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	volNames := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		volNames[v.Name] = true
	}

	if !volNames["dev-vfio"] {
		t.Error("missing dev-vfio volume")
	}
	if !volNames["hugepages"] {
		t.Error("missing hugepages volume")
	}
	if !volNames["dev-kvm"] {
		t.Error("missing dev-kvm volume")
	}

	// Verify hugepage volume medium.
	for _, v := range pod.Spec.Volumes {
		if v.Name == "hugepages" && v.VolumeSource.EmptyDir != nil {
			if v.VolumeSource.EmptyDir.Medium != "HugePages-1Gi" {
				t.Errorf("hugepages medium = %q, want HugePages-1Gi", v.VolumeSource.EmptyDir.Medium)
			}
		}
	}

	// Verify launcher mounts include /dev/vfio and /dev/hugepages.
	launcher := pod.Spec.Containers[0]
	mountNames := map[string]bool{}
	for _, m := range launcher.VolumeMounts {
		mountNames[m.Name] = true
	}
	if !mountNames["dev-vfio"] {
		t.Error("launcher missing dev-vfio mount")
	}
	if !mountNames["hugepages"] {
		t.Error("launcher missing hugepages mount")
	}
}

func TestBuildGPUDiskBootPod_NoHugepages(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "", nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == "hugepages" {
			t.Error("hugepages volume should not be present when hugepages is empty")
		}
	}
	launcher := pod.Spec.Containers[0]
	for _, m := range launcher.VolumeMounts {
		if m.Name == "hugepages" {
			t.Error("hugepages mount should not be present when hugepages is empty")
		}
	}
}

func TestBuildGPUDiskBootPod_RestartPolicy(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %v, want Never", pod.Spec.RestartPolicy)
	}
}

func TestBuildPod_NonGPU_Unchanged(t *testing.T) {
	// Regression test: BuildPod for a non-GPU guest should not include GPU volumes.
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

	pod := BuildPod(guest, rg, "test-seed", "test-intent", nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == "dev-vfio" {
			t.Error("non-GPU pod should not have dev-vfio volume")
		}
		if v.Name == "hugepages" {
			t.Error("non-GPU pod should not have hugepages volume")
		}
	}
	// No gpu-init container.
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == "gpu-init" {
			t.Error("non-GPU pod should not have gpu-init init container")
		}
	}
}

func TestBuildGPUDiskBootPod_DataDiskVolume(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()
	rg.DataDisk = &resolved.PreparedImage{PVCName: "pvc-data", Ready: true, Format: "raw"}

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	foundVol := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "data-disk" {
			foundVol = true
			if v.VolumeSource.PersistentVolumeClaim.ClaimName != "pvc-data" {
				t.Errorf("data-disk PVC = %q, want pvc-data", v.VolumeSource.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	if !foundVol {
		t.Error("GPU pod missing data-disk volume")
	}

	launcher := pod.Spec.Containers[0]
	foundMount := false
	for _, m := range launcher.VolumeMounts {
		if m.Name == "data-disk" {
			foundMount = true
			if m.MountPath != DisksDataPath {
				t.Errorf("data-disk mountPath = %q, want %q", m.MountPath, DisksDataPath)
			}
		}
	}
	if !foundMount {
		t.Error("GPU launcher missing data-disk mount")
	}
}

func TestBuildGPUDiskBootPod_NoDataDisk(t *testing.T) {
	guest := gpuGuest("gpu-node-1", []string{"0000:17:00.0"}, -1)
	rg := gpuResolvedGuest()

	pod := BuildGPUDiskBootPod(guest, rg, "test-seed", "test-intent", "1Gi", nil)

	for _, v := range pod.Spec.Volumes {
		if v.Name == "data-disk" {
			t.Error("GPU pod should not have data-disk volume when DataDisk is nil")
		}
	}
}

// --- expandCPURange tests ---

func TestExpandCPURange_Complex(t *testing.T) {
	cpus, err := expandCPURange("0-47,96-143")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cpus) != 96 {
		t.Errorf("len = %d, want 96", len(cpus))
	}
	// Check bounds.
	if cpus[0] != 0 {
		t.Errorf("first CPU = %d, want 0", cpus[0])
	}
	if cpus[47] != 47 {
		t.Errorf("cpus[47] = %d, want 47", cpus[47])
	}
	if cpus[48] != 96 {
		t.Errorf("cpus[48] = %d, want 96 (start of second range)", cpus[48])
	}
	if cpus[95] != 143 {
		t.Errorf("last CPU = %d, want 143", cpus[95])
	}
}

func TestExpandCPURange_Single(t *testing.T) {
	cpus, err := expandCPURange("5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cpus) != 1 || cpus[0] != 5 {
		t.Errorf("cpus = %v, want [5]", cpus)
	}
}

func TestExpandCPURange_Invalid(t *testing.T) {
	_, err := expandCPURange("abc")
	if err == nil {
		t.Error("expected error for invalid CPU range")
	}
}

func TestExpandCPURange_InvalidRange(t *testing.T) {
	_, err := expandCPURange("1-abc")
	if err == nil {
		t.Error("expected error for invalid range bound")
	}
}

// --- buildNUMAAndPinning tests ---

func TestBuildNUMAAndPinning_TwoSockets(t *testing.T) {
	numaSpec := &gpuv1alpha1.NUMATopologySpec{
		Sockets:           2,
		CoresPerSocket:    40,
		ThreadsPerCore:    1,
		MemoryPerSocketMi: 983040,
	}
	allocatedNUMANodes := []int{0, 1}
	hostNUMANodes := []gpuv1alpha1.NUMANodeInfo{
		{ID: 0, CPUs: "0-47,96-143", MemoryMi: 1048576},
		{ID: 1, CPUs: "48-95,144-191", MemoryMi: 1048576},
	}

	numaIntent, pins := buildNUMAAndPinning(numaSpec, allocatedNUMANodes, hostNUMANodes, true)

	if numaIntent == nil {
		t.Fatal("numaIntent is nil")
	}
	if len(numaIntent.Nodes) != 2 {
		t.Fatalf("NUMA nodes = %d, want 2", len(numaIntent.Nodes))
	}

	// Verify virtual NUMA node 0: CPUs 0-39, memoryMi 983040.
	n0 := numaIntent.Nodes[0]
	if n0.ID != 0 || n0.CPUs != "0-39" || n0.MemoryMi != 983040 {
		t.Errorf("node 0 = %+v, want id=0 cpus=0-39 mem=983040", n0)
	}
	n1 := numaIntent.Nodes[1]
	if n1.ID != 1 || n1.CPUs != "40-79" || n1.MemoryMi != 983040 {
		t.Errorf("node 1 = %+v, want id=1 cpus=40-79 mem=983040", n1)
	}

	// Verify pinning exists and maps vCPUs to physical CPUs.
	if len(pins) == 0 {
		t.Fatal("expected vCPU pins, got none")
	}
	// With 2 sockets x 40 cores = 80 vCPUs total, pins should cover both sockets.
	if len(pins) != 80 {
		t.Errorf("pins count = %d, want 80", len(pins))
	}
	// First vCPU (socket 0) should map to physical NUMA 0 CPU.
	if pins[0].VCPU != 0 {
		t.Errorf("first pin vCPU = %d, want 0", pins[0].VCPU)
	}
	if pins[0].HostCPU != 0 {
		t.Errorf("first pin hostCPU = %d, want 0 (first CPU of NUMA 0)", pins[0].HostCPU)
	}
}

func TestBuildNUMAAndPinning_NoPinning(t *testing.T) {
	numaSpec := &gpuv1alpha1.NUMATopologySpec{
		Sockets:           2,
		CoresPerSocket:    40,
		ThreadsPerCore:    1,
		MemoryPerSocketMi: 983040,
	}
	allocatedNUMANodes := []int{0, 1}
	hostNUMANodes := []gpuv1alpha1.NUMANodeInfo{
		{ID: 0, CPUs: "0-47", MemoryMi: 1048576},
		{ID: 1, CPUs: "48-95", MemoryMi: 1048576},
	}

	numaIntent, pins := buildNUMAAndPinning(numaSpec, allocatedNUMANodes, hostNUMANodes, false)

	if numaIntent == nil {
		t.Fatal("numaIntent should still be set when pinning is off")
	}
	if pins != nil {
		t.Errorf("pins should be nil when vcpuPinning=false, got %d pins", len(pins))
	}
}

func TestBuildNUMAAndPinning_SingleCore(t *testing.T) {
	numaSpec := &gpuv1alpha1.NUMATopologySpec{
		Sockets:           1,
		CoresPerSocket:    1,
		ThreadsPerCore:    1,
		MemoryPerSocketMi: 4096,
	}
	allocatedNUMANodes := []int{0}
	hostNUMANodes := []gpuv1alpha1.NUMANodeInfo{
		{ID: 0, CPUs: "0-7", MemoryMi: 65536},
	}

	numaIntent, pins := buildNUMAAndPinning(numaSpec, allocatedNUMANodes, hostNUMANodes, true)

	if len(numaIntent.Nodes) != 1 {
		t.Fatalf("NUMA nodes = %d, want 1", len(numaIntent.Nodes))
	}
	// Single core should produce cpuRange "0" (not "0-0").
	if numaIntent.Nodes[0].CPUs != "0" {
		t.Errorf("cpus = %q, want '0' for single core", numaIntent.Nodes[0].CPUs)
	}
	if len(pins) != 1 {
		t.Fatalf("pins = %d, want 1", len(pins))
	}
	if pins[0].VCPU != 0 || pins[0].HostCPU != 0 {
		t.Errorf("pin = %+v, want vcpu=0 hostCPU=0", pins[0])
	}
}

// --- isGPUAllocated tests ---

func TestIsGPUAllocated_True(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Status: swiftv1alpha1.SwiftGuestStatus{
			Conditions: []metav1.Condition{
				{Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionTrue},
			},
		},
	}
	if !isGPUAllocated(guest) {
		t.Error("expected true when GPUAllocated=True")
	}
}

func TestIsGPUAllocated_False(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		Status: swiftv1alpha1.SwiftGuestStatus{
			Conditions: []metav1.Condition{
				{Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionFalse},
			},
		},
	}
	if isGPUAllocated(guest) {
		t.Error("expected false when GPUAllocated=False")
	}
}

func TestIsGPUAllocated_NoCondition(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{}
	if isGPUAllocated(guest) {
		t.Error("expected false when no conditions")
	}
}

// --- GPUIntent construction helper tests ---

func TestGPUIntentDeviceConstruction(t *testing.T) {
	// Verify that VFIODeviceIntent fields round-trip correctly.
	dev := runtimeintent.VFIODeviceIntent{
		HostPath:        "/sys/bus/pci/devices/0000:17:00.0/",
		PCIAddress:      "0000:17:00.0",
		PCIeRootPort:    true,
		GPUDirectClique: 0,
		NoMmap:          true,
		NUMANode:        0,
	}
	if dev.HostPath != "/sys/bus/pci/devices/0000:17:00.0/" {
		t.Errorf("hostPath = %q", dev.HostPath)
	}
	if !dev.PCIeRootPort {
		t.Error("pcieRootPort should be true")
	}
	if !dev.NoMmap {
		t.Error("noMmap should be true")
	}
}
