package main

import (
	"sort"
	"testing"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
)

// --- TestParseGPUFromLspci ---

func TestParseGPUFromLspci_NVIDIA(t *testing.T) {
	input := `0000:17:00.0 3D controller [0302]: NVIDIA Corporation H200 SXM [10de:2336] (rev a1)
0000:3d:00.0 3D controller [0302]: NVIDIA Corporation H200 SXM [10de:2336] (rev a1)
0000:60:00.0 3D controller [0302]: NVIDIA Corporation H200 SXM [10de:2336] (rev a1)
0000:70:00.0 3D controller [0302]: NVIDIA Corporation H200 SXM [10de:2336] (rev a1)
0000:0a:00.0 Bridge [0680]: NVIDIA Corporation NVSwitch [10de:22a4] (rev 01)
`

	gpus, err := parseGPUsFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}

	if len(gpus) != 4 {
		t.Fatalf("expected 4 GPUs, got %d", len(gpus))
	}

	// Verify order is by PCI address.
	expectedAddrs := []string{"0000:17:00.0", "0000:3d:00.0", "0000:60:00.0", "0000:70:00.0"}
	for i, addr := range expectedAddrs {
		if gpus[i].PCIAddress != addr {
			t.Errorf("gpu[%d] PCIAddress = %q, want %q", i, gpus[i].PCIAddress, addr)
		}
		if gpus[i].Index != i {
			t.Errorf("gpu[%d] Index = %d, want %d", i, gpus[i].Index, i)
		}
	}

	// Check first GPU fields.
	g := gpus[0]
	if g.Vendor != "NVIDIA" {
		t.Errorf("Vendor = %q, want NVIDIA", g.Vendor)
	}
	if g.Model != "NVIDIA Corporation H200 SXM" {
		t.Errorf("Model = %q, want %q", g.Model, "NVIDIA Corporation H200 SXM")
	}
	if g.DeviceID != "10de:2336" {
		t.Errorf("DeviceID = %q, want %q", g.DeviceID, "10de:2336")
	}
}

func TestParseGPUFromLspci_AMD(t *testing.T) {
	input := `0000:03:00.0 VGA compatible controller [0300]: Advanced Micro Devices, Inc. [AMD/ATI] Navi 31 [Radeon RX 7900 XTX/XT] [1002:73bf] (rev c8)
`
	gpus, err := parseGPUsFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(gpus))
	}
	g := gpus[0]
	if g.Vendor != "AMD" {
		t.Errorf("Vendor = %q, want AMD", g.Vendor)
	}
	if g.DeviceID != "1002:73bf" {
		t.Errorf("DeviceID = %q, want 1002:73bf", g.DeviceID)
	}
	if g.PCIAddress != "0000:03:00.0" {
		t.Errorf("PCIAddress = %q", g.PCIAddress)
	}
}

func TestParseGPUFromLspci_Intel(t *testing.T) {
	input := `0000:56:00.0 Display controller [0380]: Intel Corporation Data Center GPU Flex 170 [8086:56c0] (rev 05)
`
	gpus, err := parseGPUsFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(gpus))
	}
	g := gpus[0]
	if g.Vendor != "Intel" {
		t.Errorf("Vendor = %q, want Intel", g.Vendor)
	}
	if g.DeviceID != "8086:56c0" {
		t.Errorf("DeviceID = %q, want 8086:56c0", g.DeviceID)
	}
}

func TestParseGPUFromLspci_Unknown(t *testing.T) {
	input := `0000:10:00.0 3D controller [0302]: Foo Corporation Bar Accelerator [abcd:1234]
`
	gpus, err := parseGPUsFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(gpus))
	}
	if gpus[0].Vendor != "Unknown (abcd)" {
		t.Errorf("Vendor = %q, want Unknown (abcd)", gpus[0].Vendor)
	}
	if gpus[0].DeviceID != "abcd:1234" {
		t.Errorf("DeviceID = %q", gpus[0].DeviceID)
	}
}

func TestParseGPUFromLspci_MixedVendors(t *testing.T) {
	input := `0000:01:00.0 VGA compatible controller [0300]: NVIDIA Corporation GP104 [GeForce GTX 1080] [10de:1b80] (rev a1)
0000:03:00.0 VGA compatible controller [0300]: Advanced Micro Devices, Inc. [AMD/ATI] Navi 31 [1002:73bf] (rev c8)
0000:56:00.0 Display controller [0380]: Intel Corporation Data Center GPU Flex 170 [8086:56c0]
`
	gpus, err := parseGPUsFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 3 {
		t.Fatalf("expected 3 GPUs, got %d", len(gpus))
	}
	vendors := map[string]bool{}
	for _, g := range gpus {
		vendors[g.Vendor] = true
	}
	if !vendors["NVIDIA"] || !vendors["AMD"] || !vendors["Intel"] {
		t.Errorf("expected NVIDIA, AMD, Intel; got %v", vendors)
	}
}

func TestParseGPUFromLspciVGA(t *testing.T) {
	input := `0000:41:00.0 VGA compatible controller [0300]: NVIDIA Corporation GA102GL [RTX A6000] [10de:2230] (rev a1)
`
	gpus, err := parseGPUsFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(gpus))
	}
	if gpus[0].Vendor != "NVIDIA" {
		t.Errorf("Vendor = %q", gpus[0].Vendor)
	}
}

func TestParseGPUNotGPUClass(t *testing.T) {
	// Network controller (class 0280) should NOT be detected as a GPU.
	input := `0000:01:00.0 Network controller [0280]: Intel Corporation Wi-Fi 6 [8086:2723]
0000:0a:00.0 Bridge [0680]: NVIDIA Corporation NVSwitch [10de:22a4] (rev 01)
`
	gpus, err := parseGPUsFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 0 {
		t.Errorf("expected 0 GPUs for non-GPU classes, got %d", len(gpus))
	}
}

func TestHasNVIDIAGPUs(t *testing.T) {
	nvidia := []gpuv1alpha1.GPUDevice{{Vendor: "NVIDIA"}}
	amd := []gpuv1alpha1.GPUDevice{{Vendor: "AMD"}}
	mixed := []gpuv1alpha1.GPUDevice{{Vendor: "AMD"}, {Vendor: "NVIDIA"}}

	if !HasNVIDIAGPUs(nvidia) {
		t.Error("HasNVIDIAGPUs should be true for NVIDIA")
	}
	if HasNVIDIAGPUs(amd) {
		t.Error("HasNVIDIAGPUs should be false for AMD-only")
	}
	if !HasNVIDIAGPUs(mixed) {
		t.Error("HasNVIDIAGPUs should be true for mixed")
	}
	if HasNVIDIAGPUs(nil) {
		t.Error("HasNVIDIAGPUs should be false for nil")
	}
}

func TestVendorName(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"10de", "NVIDIA"},
		{"1002", "AMD"},
		{"8086", "Intel"},
		{"abcd", "Unknown (abcd)"},
	}
	for _, tt := range tests {
		got := VendorName(tt.id)
		if got != tt.want {
			t.Errorf("VendorName(%q) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// --- TestParseBARSizes ---

func TestParseBARSizes(t *testing.T) {
	input := `0000:17:00.0 3D controller: NVIDIA Corporation H200 SXM
	Region 0: Memory at d0000000 (32-bit, non-prefetchable) [size=64M]
	Region 1: Memory at 3bfe0000000 (64-bit, prefetchable) [size=256G]
	Region 3: Memory at 3c7e0000000 (64-bit, prefetchable) [size=32M]
	Capabilities: [blah]
`
	bars := parseBARSizes(input)

	if len(bars) != 3 {
		t.Fatalf("expected 3 BARs, got %d", len(bars))
	}

	expected := []struct {
		region int
		sizeMi int64
	}{
		{0, 64},
		{1, 256 * 1024},
		{3, 32},
	}
	for i, e := range expected {
		if bars[i].Region != e.region || bars[i].SizeMi != e.sizeMi {
			t.Errorf("bar[%d] = {%d, %d}, want {%d, %d}",
				i, bars[i].Region, bars[i].SizeMi, e.region, e.sizeMi)
		}
	}
}

func TestParseBARSizesSmall(t *testing.T) {
	// BARs smaller than 1 MiB should be skipped.
	input := `	Region 0: Memory at 0xfoo [size=128K]
`
	bars := parseBARSizes(input)
	if len(bars) != 0 {
		t.Errorf("expected 0 BARs (sub-MiB), got %d", len(bars))
	}
}

// --- TestParseNUMATopology ---

func TestParseNUMATopology(t *testing.T) {
	lscpuOutput := `Architecture:          x86_64
CPU(s):                192
Thread(s) per core:    2
Core(s) per socket:    48
Socket(s):             2
`
	info := parseLscpuOutput(lscpuOutput)
	if info.Sockets != 2 {
		t.Errorf("Sockets = %d, want 2", info.Sockets)
	}
	if info.CoresPerSocket != 48 {
		t.Errorf("CoresPerSocket = %d, want 48", info.CoresPerSocket)
	}
	if info.ThreadsPerCore != 2 {
		t.Errorf("ThreadsPerCore = %d, want 2", info.ThreadsPerCore)
	}
	if info.TotalCPUs != 192 {
		t.Errorf("TotalCPUs = %d, want 192", info.TotalCPUs)
	}
}

func TestParseNUMAMemoryContent(t *testing.T) {
	content := `Node 0 MemTotal:       1073741824 kB
Node 0 MemFree:         500000000 kB
Node 0 MemUsed:         573741824 kB
`
	mem := parseNUMAMemoryContent(content, 0)
	expectedMi := int64(1073741824 / 1024)
	if mem != expectedMi {
		t.Errorf("MemoryMi = %d, want %d", mem, expectedMi)
	}
}

func TestParseNUMAMemoryContentWrongNode(t *testing.T) {
	content := `Node 1 MemTotal:       500000 kB
`
	mem := parseNUMAMemoryContent(content, 0)
	if mem != 0 {
		t.Errorf("expected 0 for wrong node, got %d", mem)
	}
}

// --- TestMergePreservesAllocation ---

func TestMergePreservesAllocation(t *testing.T) {
	existing := &gpuv1alpha1.SwiftGPUNodeStatus{
		Phase:    "Ready",
		GPUCount: 8,
		FreeGPUs: 6,
		GPUModel: "NVIDIA Corporation H200 SXM",
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:17:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: true, AllocatedTo: "default/gpu-vm-1"},
			{Index: 1, PCIAddress: "0000:3d:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: true, AllocatedTo: "default/gpu-vm-1"},
			{Index: 2, PCIAddress: "0000:60:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: false},
			{Index: 3, PCIAddress: "0000:70:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: false},
			{Index: 4, PCIAddress: "0000:80:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: false},
			{Index: 5, PCIAddress: "0000:90:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: false},
			{Index: 6, PCIAddress: "0000:a0:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: false},
			{Index: 7, PCIAddress: "0000:b0:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", Driver: "nvidia", Allocated: false},
		},
		FabricManager: &gpuv1alpha1.FabricManagerStatus{
			Installed: true,
			Version:   "580.95.05",
			Running:   true,
			Partitions: []gpuv1alpha1.FMPartitionStatus{
				{ID: 0, GPUIndices: []int{0, 1}, Active: true, AllocatedTo: "default/gpu-vm-1"},
				{ID: 1, GPUIndices: []int{2, 3}, Active: false, AllocatedTo: ""},
			},
		},
	}

	discovered := &SwiftGPUNodeStatus{
		Host: gpuv1alpha1.HostTopology{
			CPUTopology: gpuv1alpha1.CPUTopologyInfo{Sockets: 2, CoresPerSocket: 48, ThreadsPerCore: 2, TotalCPUs: 192},
		},
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:17:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "vfio-pci"},
			{Index: 1, PCIAddress: "0000:3d:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "vfio-pci"},
			{Index: 2, PCIAddress: "0000:60:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "nvidia"},
			{Index: 3, PCIAddress: "0000:70:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "nvidia"},
			{Index: 4, PCIAddress: "0000:80:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "nvidia"},
			{Index: 5, PCIAddress: "0000:90:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "nvidia"},
			{Index: 6, PCIAddress: "0000:a0:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "nvidia"},
			{Index: 7, PCIAddress: "0000:b0:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336", Driver: "nvidia"},
		},
		FabricManager: &gpuv1alpha1.FabricManagerStatus{
			Installed: true,
			Version:   "580.95.05",
			Running:   true,
			Partitions: []gpuv1alpha1.FMPartitionStatus{
				{ID: 0, GPUIndices: []int{0, 1}, Active: true},
				{ID: 1, GPUIndices: []int{2, 3}, Active: false},
			},
		},
	}

	merged := mergeStatus(discovered, existing)

	if !merged.GPUs[0].Allocated || merged.GPUs[0].AllocatedTo != "default/gpu-vm-1" {
		t.Errorf("gpu[0] allocation not preserved: allocated=%v, allocatedTo=%q",
			merged.GPUs[0].Allocated, merged.GPUs[0].AllocatedTo)
	}
	if !merged.GPUs[1].Allocated || merged.GPUs[1].AllocatedTo != "default/gpu-vm-1" {
		t.Errorf("gpu[1] allocation not preserved")
	}
	for i := 2; i < 8; i++ {
		if merged.GPUs[i].Allocated {
			t.Errorf("gpu[%d] should not be allocated", i)
		}
	}
	if merged.GPUs[0].Driver != "vfio-pci" {
		t.Errorf("gpu[0] Driver = %q, want vfio-pci", merged.GPUs[0].Driver)
	}
	if merged.GPUs[0].Vendor != "NVIDIA" {
		t.Errorf("gpu[0] Vendor = %q, want NVIDIA", merged.GPUs[0].Vendor)
	}
	if merged.FabricManager.Partitions[0].AllocatedTo != "default/gpu-vm-1" {
		t.Errorf("partition[0] AllocatedTo not preserved: %q", merged.FabricManager.Partitions[0].AllocatedTo)
	}
	if merged.FabricManager.Partitions[1].AllocatedTo != "" {
		t.Errorf("partition[1] AllocatedTo should be empty")
	}
	if merged.GPUCount != 8 {
		t.Errorf("GPUCount = %d, want 8", merged.GPUCount)
	}
	if merged.FreeGPUs != 6 {
		t.Errorf("FreeGPUs = %d, want 6", merged.FreeGPUs)
	}
	if merged.GPUVendor != "NVIDIA" {
		t.Errorf("GPUVendor = %q, want NVIDIA", merged.GPUVendor)
	}
}

func TestMergeNewGPU(t *testing.T) {
	existing := &gpuv1alpha1.SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:17:00.0", Allocated: false},
		},
	}
	discovered := &SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:17:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM"},
			{Index: 1, PCIAddress: "0000:3d:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM"},
		},
	}
	merged := mergeStatus(discovered, existing)
	if len(merged.GPUs) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(merged.GPUs))
	}
	if merged.GPUs[1].Allocated {
		t.Error("new GPU should not be allocated")
	}
	if merged.GPUCount != 2 {
		t.Errorf("GPUCount = %d, want 2", merged.GPUCount)
	}
}

func TestMergeRemovedGPU(t *testing.T) {
	existing := &gpuv1alpha1.SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:17:00.0", Allocated: true, AllocatedTo: "default/vm1"},
			{Index: 1, PCIAddress: "0000:3d:00.0", Allocated: false},
		},
	}
	discovered := &SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:3d:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM"},
		},
	}
	merged := mergeStatus(discovered, existing)
	if merged.Phase != "Error" {
		t.Errorf("Phase = %q, want Error (allocated GPU removed)", merged.Phase)
	}
	if len(merged.GPUs) != 1 {
		t.Fatalf("expected 1 GPU, got %d", len(merged.GPUs))
	}
}

func TestMergeRemovedGPUUnallocated(t *testing.T) {
	existing := &gpuv1alpha1.SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:17:00.0", Allocated: false},
			{Index: 1, PCIAddress: "0000:3d:00.0", Allocated: false},
		},
	}
	discovered := &SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:3d:00.0"},
		},
	}
	merged := mergeStatus(discovered, existing)
	if merged.Phase != "Ready" {
		t.Errorf("Phase = %q, want Ready", merged.Phase)
	}
}

func TestFreeGPUCount(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{
		{Allocated: true}, {Allocated: false}, {Allocated: true}, {Allocated: false}, {Allocated: false},
	}
	if n := countFreeGPUs(gpus); n != 3 {
		t.Errorf("countFreeGPUs = %d, want 3", n)
	}
}

func TestFreeGPUCountAllFree(t *testing.T) {
	gpus := []gpuv1alpha1.GPUDevice{{Allocated: false}, {Allocated: false}}
	if n := countFreeGPUs(gpus); n != 2 {
		t.Errorf("countFreeGPUs = %d, want 2", n)
	}
}

func TestFreeGPUCountNone(t *testing.T) {
	if n := countFreeGPUs(nil); n != 0 {
		t.Errorf("countFreeGPUs(nil) = %d, want 0", n)
	}
}

// --- TestNVSwitchDiscovery ---

func TestNVSwitchDiscovery(t *testing.T) {
	input := `0000:0a:00.0 Bridge [0680]: NVIDIA Corporation NVSwitch [10de:22a4] (rev 01)
0000:0b:00.0 Bridge [0680]: NVIDIA Corporation NVSwitch [10de:22a4] (rev 01)
0000:17:00.0 3D controller [0302]: NVIDIA Corporation H200 SXM [10de:2336] (rev a1)
`
	switches, err := parseNVSwitchesFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(switches) != 2 {
		t.Fatalf("expected 2 NVSwitches, got %d", len(switches))
	}
	if switches[0].PCIAddress != "0000:0a:00.0" {
		t.Errorf("switch[0] PCIAddress = %q", switches[0].PCIAddress)
	}
	if switches[0].DeviceID != "10de:22a4" {
		t.Errorf("switch[0] DeviceID = %q", switches[0].DeviceID)
	}
}

func TestNVSwitchDiscoveryNone(t *testing.T) {
	input := `0000:17:00.0 3D controller [0302]: NVIDIA Corporation H200 SXM [10de:2336] (rev a1)
`
	switches, err := parseNVSwitchesFromLspci(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(switches) != 0 {
		t.Errorf("expected 0 NVSwitches, got %d", len(switches))
	}
}

// --- TestFabricManagerParsing ---

// With an empty module map (nvidia-smi unavailable / PCIe-only), the parsed
// physical IDs pass through as-is (identity) — preserves pre-translation
// behavior for nodes without a Fabric.
func TestParseFMPartitions_IdentityFallback(t *testing.T) {
	output := `partition 0: gpus=0,1 active=false
partition 1: gpus=2,3 active=false
partition 2: gpus=0,1,2,3 active=false
partition 3: gpus=0,1,2,3,4,5,6,7 active=true
`
	partitions := parseFMPartitions(output, map[int]int{})
	if len(partitions) != 4 {
		t.Fatalf("expected 4 partitions, got %d", len(partitions))
	}
	if partitions[0].ID != 0 || len(partitions[0].GPUIndices) != 2 {
		t.Errorf("partition[0] = {ID:%d, GPUs:%v}", partitions[0].ID, partitions[0].GPUIndices)
	}
	if !partitions[3].Active || len(partitions[3].GPUIndices) != 8 {
		t.Errorf("partition[3] Active=%v, GPUs=%d", partitions[3].Active, len(partitions[3].GPUIndices))
	}
}

// The load-bearing correctness property (NVIDIA WP-12736-002): Fabric Manager
// lists partition membership in GPU physical/Module IDs, which do NOT follow
// lspci order. parseFMPartitions must translate those into device-Index space.
func TestParseFMPartitions_ModuleIDTranslation(t *testing.T) {
	// Guide mapping: Module Id -> device Index.
	moduleToIndex := map[int]int{5: 0, 7: 1, 6: 2, 8: 3, 1: 4, 3: 5, 2: 6, 4: 7}
	// Partition 1 (the guide's first 4-GPU half) = Module IDs 1,2,3,4.
	partitions := parseFMPartitions("partition 1: gpus=1,2,3,4 active=false\n", moduleToIndex)
	if len(partitions) != 1 {
		t.Fatalf("expected 1 partition, got %d", len(partitions))
	}
	got := append([]int(nil), partitions[0].GPUIndices...)
	sort.Ints(got)
	// Module {1,2,3,4} -> Index {4,6,5,7}; NOT the raw {1,2,3,4}.
	want := []int{4, 5, 6, 7}
	if !equalInts(got, want) {
		t.Errorf("GPUIndices = %v (raw would be [1 2 3 4]); want translated %v",
			partitions[0].GPUIndices, want)
	}
}

// A physical ID with no discovered device is dropped (safe under-count), not
// left as a wrong index.
func TestParseFMPartitions_DropsUnmappedModuleID(t *testing.T) {
	moduleToIndex := map[int]int{5: 0, 7: 1}
	partitions := parseFMPartitions("partition 0: gpus=5,99,7 active=false\n", moduleToIndex)
	got := append([]int(nil), partitions[0].GPUIndices...)
	sort.Ints(got)
	if !equalInts(got, []int{0, 1}) {
		t.Errorf("GPUIndices = %v, want [0 1] (Module Id 99 dropped)", partitions[0].GPUIndices)
	}
}

func TestNormalizeBDF(t *testing.T) {
	cases := map[string]string{
		"00000000:0F:00.0": "0000:0f:00.0", // nvidia-smi form
		"0000:0f:00.0":     "0000:0f:00.0", // lspci form
		"0000:B8:00.0":     "0000:b8:00.0",
		"0f:00.0":          "0000:0f:00.0", // domain-less
	}
	for in, want := range cases {
		if got := normalizeBDF(in); got != want {
			t.Errorf("normalizeBDF(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseModuleIDs(t *testing.T) {
	// Real-shape `nvidia-smi -q` excerpt: 8-hex-digit upper-case domain header,
	// indented fields.
	input := `
==============NVSMI LOG==============

Attached GPUs                             : 2
GPU 00000000:0F:00.0
    Product Name                          : NVIDIA H100 80GB HBM3
    GPU Module Id                         : 5
    Bus Id                                : 00000000:0F:00.0
GPU 00000000:10:00.0
    Product Name                          : NVIDIA H100 80GB HBM3
    GPU Module Id                         : 7
`
	m := parseModuleIDs(input)
	if m["0000:0f:00.0"] != 5 {
		t.Errorf("module id for 0f = %d, want 5 (map=%v)", m["0000:0f:00.0"], m)
	}
	if m["0000:10:00.0"] != 7 {
		t.Errorf("module id for 10 = %d, want 7 (map=%v)", m["0000:10:00.0"], m)
	}
}

func TestBuildModuleToIndex(t *testing.T) {
	bdfToModule := map[string]int{"0000:0f:00.0": 5, "0000:10:00.0": 7}
	gpus := []gpuv1alpha1.GPUDevice{
		{Index: 0, PCIAddress: "0000:0f:00.0"},
		{Index: 1, PCIAddress: "0000:10:00.0"},
	}
	m := buildModuleToIndex(bdfToModule, gpus)
	if m[5] != 0 || m[7] != 1 {
		t.Errorf("moduleToIndex = %v, want {5:0, 7:1}", m)
	}
}

// End-to-end round-trip proving discovery produces exactly the Index-space
// GPUIndices the #405 allocator fixture hard-codes: nvidia-smi Module IDs +
// lspci BDF order -> the guide's physicalId->Index map -> translated partition.
func TestFMPartition_GuideRoundTrip(t *testing.T) {
	smi := `GPU 00000000:0F:00.0
    GPU Module Id : 5
GPU 00000000:10:00.0
    GPU Module Id : 7
GPU 00000000:41:00.0
    GPU Module Id : 6
GPU 00000000:44:00.0
    GPU Module Id : 8
GPU 00000000:86:00.0
    GPU Module Id : 1
GPU 00000000:87:00.0
    GPU Module Id : 3
GPU 00000000:B8:00.0
    GPU Module Id : 2
GPU 00000000:BB:00.0
    GPU Module Id : 4
`
	gpus := []gpuv1alpha1.GPUDevice{
		{Index: 0, PCIAddress: "0000:0f:00.0"}, {Index: 1, PCIAddress: "0000:10:00.0"},
		{Index: 2, PCIAddress: "0000:41:00.0"}, {Index: 3, PCIAddress: "0000:44:00.0"},
		{Index: 4, PCIAddress: "0000:86:00.0"}, {Index: 5, PCIAddress: "0000:87:00.0"},
		{Index: 6, PCIAddress: "0000:b8:00.0"}, {Index: 7, PCIAddress: "0000:bb:00.0"},
	}
	moduleToIndex := buildModuleToIndex(parseModuleIDs(smi), gpus)

	// The guide's partition 1 = the 4-GPU half with Module IDs {1,2,3,4}.
	// The #405 fixture encodes the same partition as device Indices via
	// phys{1:4,2:6,3:5,4:7} => {4,6,5,7}.
	partitions := parseFMPartitions("partition 1: gpus=1,2,3,4 active=false\n", moduleToIndex)
	got := append([]int(nil), partitions[0].GPUIndices...)
	sort.Ints(got)
	if !equalInts(got, []int{4, 5, 6, 7}) {
		t.Errorf("round-trip GPUIndices = %v, want [4 5 6 7] (fixture parity)", partitions[0].GPUIndices)
	}
}

func equalInts(a, b []int) bool {
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

func TestNoFabricManager(t *testing.T) {
	existing := &gpuv1alpha1.SwiftGPUNodeStatus{}
	discovered := &SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:41:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation L40S"},
		},
		FabricManager: nil,
	}
	merged := mergeStatus(discovered, existing)
	if merged.FabricManager != nil {
		t.Error("FabricManager should be nil for PCIe-only nodes")
	}
}

func TestNoGPUs(t *testing.T) {
	existing := &gpuv1alpha1.SwiftGPUNodeStatus{}
	discovered := &SwiftGPUNodeStatus{
		Host: gpuv1alpha1.HostTopology{
			CPUTopology: gpuv1alpha1.CPUTopologyInfo{Sockets: 1, CoresPerSocket: 8, TotalCPUs: 8},
		},
		GPUs: nil,
	}
	merged := mergeStatus(discovered, existing)
	if merged.GPUCount != 0 {
		t.Errorf("GPUCount = %d, want 0", merged.GPUCount)
	}
	if merged.GPUVendor != "" {
		t.Errorf("GPUVendor = %q, want empty", merged.GPUVendor)
	}
}

func TestMergeEmptyExisting(t *testing.T) {
	existing := &gpuv1alpha1.SwiftGPUNodeStatus{}
	discovered := &SwiftGPUNodeStatus{
		GPUs: []gpuv1alpha1.GPUDevice{
			{Index: 0, PCIAddress: "0000:17:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336"},
			{Index: 1, PCIAddress: "0000:3d:00.0", Vendor: "NVIDIA", Model: "NVIDIA Corporation H200 SXM", DeviceID: "10de:2336"},
		},
	}
	merged := mergeStatus(discovered, existing)
	if len(merged.GPUs) != 2 {
		t.Fatalf("expected 2 GPUs, got %d", len(merged.GPUs))
	}
	if merged.GPUs[0].Allocated {
		t.Error("first-run GPUs should not be allocated")
	}
	if merged.GPUVendor != "NVIDIA" {
		t.Errorf("GPUVendor = %q, want NVIDIA", merged.GPUVendor)
	}
}

func TestParseIntList(t *testing.T) {
	result := parseIntList("0,1,2,3")
	if len(result) != 4 {
		t.Fatalf("expected 4 ints, got %d", len(result))
	}
	for i, v := range result {
		if v != i {
			t.Errorf("result[%d] = %d, want %d", i, v, i)
		}
	}
}

func TestMergeStatus_VfioReady(t *testing.T) {
	for _, ready := range []bool{true, false} {
		discovered := &SwiftGPUNodeStatus{VfioReady: ready}
		merged := mergeStatus(discovered, &gpuv1alpha1.SwiftGPUNodeStatus{})
		if merged.VfioReady != ready {
			t.Errorf("VfioReady = %v, want %v (must carry through from discovery)", merged.VfioReady, ready)
		}
	}
}
