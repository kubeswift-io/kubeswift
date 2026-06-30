package swiftgpu

import (
	"testing"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
)

// makeGPU creates a GPUDevice with the given index, PCI address, model, NUMA node, and allocation state.
func makeGPU(index int, pciAddr, model string, numaNode int, allocated bool, allocatedTo string) gpuv1alpha1.GPUDevice {
	return gpuv1alpha1.GPUDevice{
		Index:       index,
		PCIAddress:  pciAddr,
		Model:       model,
		DeviceID:    "10de:2336",
		NUMANode:    numaNode,
		IOMMUGroup:  15 + index,
		Driver:      "vfio-pci",
		Allocated:   allocated,
		AllocatedTo: allocatedTo,
	}
}

func TestSelectGPUs_SingleGPU(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(2, "0000:60:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(3, "0000:70:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(4, "0000:80:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(5, "0000:90:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(6, "0000:a0:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(7, "0000:b0:00.0", "NVIDIA H200 SXM", 1, false, ""),
	}

	selected, numaNodes := selectGPUs(gpus, 1, "")
	if len(selected) != 1 {
		t.Fatalf("selected %d GPUs, want 1", len(selected))
	}
	if len(numaNodes) != 1 {
		t.Fatalf("numaNodes = %v, want exactly 1 NUMA node", numaNodes)
	}
}

func TestSelectGPUs_SameNUMA(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(2, "0000:60:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(3, "0000:70:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(4, "0000:80:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(5, "0000:90:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(6, "0000:a0:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(7, "0000:b0:00.0", "NVIDIA H200 SXM", 1, false, ""),
	}

	// 4 GPUs requested; NUMA 0 has 4 free, NUMA 1 has 4 free.
	// Should prefer single NUMA node.
	selected, numaNodes := selectGPUs(gpus, 4, "")
	if len(selected) != 4 {
		t.Fatalf("selected %d GPUs, want 4", len(selected))
	}
	if len(numaNodes) != 1 {
		t.Fatalf("numaNodes = %v, want exactly 1 NUMA node (same-NUMA preference)", numaNodes)
	}
	// Verify all selected GPUs are on the same NUMA node.
	for _, g := range selected {
		if g.NUMANode != numaNodes[0] {
			t.Errorf("GPU %s on NUMA %d, expected NUMA %d", g.PCIAddress, g.NUMANode, numaNodes[0])
		}
	}
}

func TestSelectGPUs_CrossNUMA(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(2, "0000:60:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(3, "0000:70:00.0", "NVIDIA H200 SXM", 1, false, ""),
		// GPUs 4-5 on NUMA 0 are allocated.
		makeGPU(4, "0000:80:00.0", "NVIDIA H200 SXM", 0, true, "default/other"),
		makeGPU(5, "0000:90:00.0", "NVIDIA H200 SXM", 0, true, "default/other"),
		// GPUs 6-7 on NUMA 1 are allocated.
		makeGPU(6, "0000:a0:00.0", "NVIDIA H200 SXM", 1, true, "default/other"),
		makeGPU(7, "0000:b0:00.0", "NVIDIA H200 SXM", 1, true, "default/other"),
	}

	// 4 GPUs requested, but only 2 free per NUMA — must span both.
	selected, numaNodes := selectGPUs(gpus, 4, "")
	if len(selected) != 4 {
		t.Fatalf("selected %d GPUs, want 4", len(selected))
	}
	if len(numaNodes) != 2 {
		t.Fatalf("numaNodes = %v, want 2 NUMA nodes (cross-NUMA fallback)", numaNodes)
	}
}

func TestSelectGPUs_InsufficientFree(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(2, "0000:60:00.0", "NVIDIA H200 SXM", 1, true, "default/busy"),
		makeGPU(3, "0000:70:00.0", "NVIDIA H200 SXM", 1, true, "default/busy"),
	}

	selected, _ := selectGPUs(gpus, 4, "")
	if selected != nil {
		t.Errorf("expected nil selection when insufficient free GPUs, got %d", len(selected))
	}
}

func TestSelectGPUs_ModelFilter(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA H200 SXM", 0, false, ""),
		makeGPU(2, "0000:60:00.0", "NVIDIA H200 SXM", 1, false, ""),
		makeGPU(3, "0000:70:00.0", "NVIDIA H200 SXM", 1, false, ""),
	}

	// Model filter "A100-PCIe" should not match "NVIDIA H200 SXM".
	selected, _ := selectGPUs(gpus, 1, "A100-PCIe")
	if selected != nil {
		t.Errorf("expected nil selection when model filter doesn't match, got %d", len(selected))
	}
}

func TestSelectGPUs_ModelFilter_Match(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA A100-PCIe", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "NVIDIA H200 SXM", 0, false, ""),
	}

	// "A100" is a substring of "NVIDIA A100-PCIe".
	selected, _ := selectGPUs(gpus, 1, "A100")
	if len(selected) != 1 {
		t.Fatalf("selected %d GPUs, want 1", len(selected))
	}
	if selected[0].Model != "NVIDIA A100-PCIe" {
		t.Errorf("selected model = %q, want NVIDIA A100-PCIe", selected[0].Model)
	}
}

func TestFindFMPartition_Match(t *testing.T) {
	fm := &gpuv1alpha1.FabricManagerStatus{
		Installed: true,
		Version:   "580.95.05",
		Running:   true,
		Partitions: []gpuv1alpha1.FMPartitionStatus{
			{ID: 0, GPUIndices: []int{0, 1}, AllocatedTo: ""},
			{ID: 1, GPUIndices: []int{2, 3}, AllocatedTo: ""},
			{ID: 2, GPUIndices: []int{0, 1, 2, 3}, AllocatedTo: ""},
		},
	}

	id, err := findFMPartition(fm, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 2 {
		t.Errorf("partition ID = %d, want 2", id)
	}
}

func TestFindFMPartition_NoMatch(t *testing.T) {
	fm := &gpuv1alpha1.FabricManagerStatus{
		Installed: true,
		Version:   "580.95.05",
		Running:   true,
		Partitions: []gpuv1alpha1.FMPartitionStatus{
			{ID: 0, GPUIndices: []int{0, 1}, AllocatedTo: ""},
			{ID: 1, GPUIndices: []int{2, 3}, AllocatedTo: ""},
		},
	}

	_, err := findFMPartition(fm, 8)
	if err == nil {
		t.Error("expected error when no partition matches GPU count")
	}
}

func TestFindFMPartition_AlreadyAllocated(t *testing.T) {
	fm := &gpuv1alpha1.FabricManagerStatus{
		Installed: true,
		Version:   "580.95.05",
		Running:   true,
		Partitions: []gpuv1alpha1.FMPartitionStatus{
			{ID: 0, GPUIndices: []int{0, 1, 2, 3}, AllocatedTo: "default/other-guest"},
		},
	}

	_, err := findFMPartition(fm, 4)
	if err == nil {
		t.Error("expected error when matching partition is already allocated")
	}
}

func TestFindFMPartition_FMNotRunning(t *testing.T) {
	fm := &gpuv1alpha1.FabricManagerStatus{
		Installed: true,
		Version:   "580.95.05",
		Running:   false,
		Partitions: []gpuv1alpha1.FMPartitionStatus{
			{ID: 0, GPUIndices: []int{0, 1, 2, 3}, AllocatedTo: ""},
		},
	}

	_, err := findFMPartition(fm, 4)
	if err == nil {
		t.Error("expected error when Fabric Manager is not running")
	}
}

func TestCountFreeGPUs(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "H200", 0, false, ""),
		makeGPU(1, "0000:3d:00.0", "H200", 0, true, "default/busy"),
		makeGPU(2, "0000:60:00.0", "H200", 1, false, ""),
		makeGPU(3, "0000:70:00.0", "H200", 1, true, "default/busy"),
		makeGPU(4, "0000:80:00.0", "H200", 0, false, ""),
	}

	got := countFreeGPUs(gpus)
	if got != 3 {
		t.Errorf("countFreeGPUs = %d, want 3", got)
	}
}

func TestNumaSetToSlice(t *testing.T) {
	m := map[int]bool{2: true, 0: true, 1: true}
	s := numaSetToSlice(m)
	if len(s) != 3 {
		t.Fatalf("len = %d, want 3", len(s))
	}
	// Verify deterministic sorted output.
	for i, want := range []int{0, 1, 2} {
		if s[i] != want {
			t.Errorf("s[%d] = %d, want %d", i, s[i], want)
		}
	}
}

func TestNumaSetToSlice_Empty(t *testing.T) {
	m := map[int]bool{}
	s := numaSetToSlice(m)
	if len(s) != 0 {
		t.Errorf("len = %d, want 0", len(s))
	}
}
