package swiftgpu

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// hgxH100Node is a hardware-faithful 8-GPU HGX H100/H200 SwiftGPUNode fixture,
// taken verbatim from the NVIDIA HGX Shared NVSwitch GPU Passthrough
// Virtualization Integration Guide (WP-12736-002):
//
//   - GPU BDFs (lspci): 0f, 10, 41, 44, 86, 87, b8, bb :00.0 — device 10de:2330.
//     Device Index follows lspci order (what gpu-discovery produces).
//   - NVSwitch BDFs: 03, 04, 05, 06 :00.0 — device 10de:22a3.
//   - The Fabric Manager partition table is the guide's real fmpm -l output:
//     partition 0 = all 8 GPUs, 1–2 = 4-GPU halves, 3–6 = pairs, 7–14 = singles.
//   - CRITICALLY, FM partitions reference GPU *physical IDs* (nvidia-smi -q
//     "Module ID"), which do NOT follow lspci order. The guide's mapping:
//     0f→5, 10→7, 41→6, 44→8, 86→1, 87→3, b8→2, bb→4. GPUIndices below carry
//     that mapping translated into device-Index space — which is exactly what
//     makes an uncoupled "pick GPUs by NUMA, pick a partition by count"
//     allocation hand a guest GPUs the activated partition doesn't cover.
//
// NUMA: the first four GPUs by lspci order (0f,10,41,44) sit on socket 0, the
// rest on socket 1 (the standard dual-socket HGX split).
func hgxH100Node(name string) *gpuv1alpha1.SwiftGPUNode {
	gpu := func(idx int, bdf string, numa int) gpuv1alpha1.GPUDevice {
		return gpuv1alpha1.GPUDevice{
			Index:      idx,
			PCIAddress: bdf,
			Model:      "H100-SXM",
			DeviceID:   "10de:2330",
			NUMANode:   numa,
			IOMMUGroup: 10 + idx,
			Driver:     "vfio-pci",
			BARSizes:   []gpuv1alpha1.BARSize{{Region: 1, SizeMi: 131072}},
		}
	}
	// physicalId (Module ID) → device Index, per the guide's nvidia-smi -q map.
	phys := map[int]int{1: 4, 2: 6, 3: 5, 4: 7, 5: 0, 6: 2, 7: 1, 8: 3}
	part := func(id int, physIDs ...int) gpuv1alpha1.FMPartitionStatus {
		idx := make([]int, 0, len(physIDs))
		for _, p := range physIDs {
			idx = append(idx, phys[p])
		}
		return gpuv1alpha1.FMPartitionStatus{ID: id, GPUIndices: idx}
	}
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			Phase:     "Ready",
			GPUCount:  8,
			FreeGPUs:  8,
			GPUModel:  "H100-SXM",
			VfioReady: true,
			Host: gpuv1alpha1.HostTopology{
				CPUTopology: gpuv1alpha1.CPUTopologyInfo{
					Sockets: 2, CoresPerSocket: 56, ThreadsPerCore: 2, TotalCPUs: 224,
				},
				NUMANodes: []gpuv1alpha1.NUMANodeInfo{
					{ID: 0, CPUs: "0-55,112-167", MemoryMi: 1048576},
					{ID: 1, CPUs: "56-111,168-223", MemoryMi: 1048576},
				},
				IOMMUEnabled: true,
				Hugepages1Gi: gpuv1alpha1.HugepageInfo{Total: 1800, Free: 1800},
			},
			GPUs: []gpuv1alpha1.GPUDevice{
				gpu(0, "0000:0f:00.0", 0),
				gpu(1, "0000:10:00.0", 0),
				gpu(2, "0000:41:00.0", 0),
				gpu(3, "0000:44:00.0", 0),
				gpu(4, "0000:86:00.0", 1),
				gpu(5, "0000:87:00.0", 1),
				gpu(6, "0000:b8:00.0", 1),
				gpu(7, "0000:bb:00.0", 1),
			},
			NVSwitches: []gpuv1alpha1.NVSwitchDevice{
				{PCIAddress: "0000:03:00.0", DeviceID: "10de:22a3", NUMANode: 0},
				{PCIAddress: "0000:04:00.0", DeviceID: "10de:22a3", NUMANode: 0},
				{PCIAddress: "0000:05:00.0", DeviceID: "10de:22a3", NUMANode: 0},
				{PCIAddress: "0000:06:00.0", DeviceID: "10de:22a3", NUMANode: 0},
			},
			FabricManager: &gpuv1alpha1.FabricManagerStatus{
				Installed: true,
				Version:   "550.163.01",
				Running:   true,
				Partitions: []gpuv1alpha1.FMPartitionStatus{
					part(0, 1, 2, 3, 4, 5, 6, 7, 8),
					part(1, 1, 2, 3, 4),
					part(2, 5, 6, 7, 8),
					part(3, 1, 3),
					part(4, 2, 4),
					part(5, 5, 7),
					part(6, 6, 8),
					part(7, 1), part(8, 2), part(9, 3), part(10, 4),
					part(11, 5), part(12, 6), part(13, 7), part(14, 8),
				},
			},
		},
	}
}

// partitionMembers returns the PCI addresses of a partition's member devices.
func partitionMembers(n *gpuv1alpha1.SwiftGPUNode, partID int) map[string]bool {
	byIndex := map[int]string{}
	for _, g := range n.Status.GPUs {
		byIndex[g.Index] = g.PCIAddress
	}
	out := map[string]bool{}
	for _, p := range n.Status.FabricManager.Partitions {
		if p.ID == partID {
			for _, idx := range p.GPUIndices {
				out[byIndex[idx]] = true
			}
		}
	}
	return out
}

// The load-bearing shared-NVSwitch invariant (NVIDIA WP-12736-002: the fabric
// allows NVLink among ONLY the GPUs within the activated partition, and the
// admin passes through the GPUs WITHIN that partition): the devices an
// allocation returns must be exactly the returned partition's members. With
// the guide's real Module-ID mapping, an uncoupled NUMA-first GPU pick +
// count-first partition pick hands the guest partition 2's GPUs while
// activating partition 1 — no NVLink for this tenant, and a fabric
// cross-wired against the next one.
func TestFindAndAllocate_HGXShared_PartitionMembershipCoupled(t *testing.T) {
	node := hgxH100Node("hgx-0")
	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	profile := testGPUProfile("hgx4", "default", "hgx-shared", "", 4, "shared")

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, guest, profile).
		WithStatusSubresource(node, guest).
		Build()
	r := &SwiftGPUReconciler{Client: c, Scheme: scheme.Scheme}

	_, gpus, numa, partID, err := r.findAndAllocate(context.Background(), guest, profile)
	if err != nil {
		t.Fatalf("findAndAllocate: %v", err)
	}
	if partID < 0 {
		t.Fatalf("shared mode must select an FM partition, got %d", partID)
	}
	members := partitionMembers(node, partID)
	if len(gpus) != 4 {
		t.Fatalf("got %d GPUs, want 4", len(gpus))
	}
	for _, g := range gpus {
		if !members[g.PCIAddress] {
			t.Errorf("allocated GPU %s is NOT a member of activated partition %d %v — the guest would have no NVLink to it",
				g.PCIAddress, partID, members)
		}
	}
	// The guide's 4-GPU partitions are single-NUMA halves; the derived NUMA
	// set must reflect the members, not an independent NUMA-first pick.
	if len(numa) != 1 {
		t.Errorf("partition members are one NUMA node, got numa=%v", numa)
	}
}

// Two 4-GPU guests must land on the two disjoint 4-GPU partitions, each
// receiving exactly its partition's members; a third finds no capacity.
func TestFindAndAllocate_HGXShared_TwoTenantsDisjoint(t *testing.T) {
	node := hgxH100Node("hgx-0")
	g1 := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	g2 := testSwiftGuest("g2", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	g3 := testSwiftGuest("g3", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	profile := testGPUProfile("hgx4", "default", "hgx-shared", "", 4, "shared")

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, g1, g2, g3, profile).
		WithStatusSubresource(node, g1, g2, g3).
		Build()
	r := &SwiftGPUReconciler{Client: c, Scheme: scheme.Scheme}
	ctx := context.Background()

	_, gpus1, _, part1, err := r.findAndAllocate(ctx, g1, profile)
	if err != nil {
		t.Fatalf("g1: %v", err)
	}
	_, gpus2, _, part2, err := r.findAndAllocate(ctx, g2, profile)
	if err != nil {
		t.Fatalf("g2: %v", err)
	}
	if part1 == part2 {
		t.Fatalf("both guests got partition %d", part1)
	}
	seen := map[string]bool{}
	for _, g := range gpus1 {
		seen[g.PCIAddress] = true
	}
	for _, g := range gpus2 {
		if seen[g.PCIAddress] {
			t.Errorf("GPU %s allocated to both guests", g.PCIAddress)
		}
	}
	// Each set must equal its own partition's membership.
	var updated gpuv1alpha1.SwiftGPUNode
	if err := c.Get(ctx, client.ObjectKey{Name: "hgx-0"}, &updated); err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		gpus []gpuv1alpha1.GPUDevice
		part int
	}{{gpus1, part1}, {gpus2, part2}} {
		members := partitionMembers(&updated, pair.part)
		for _, g := range pair.gpus {
			if !members[g.PCIAddress] {
				t.Errorf("GPU %s outside its partition %d", g.PCIAddress, pair.part)
			}
		}
	}
	if updated.Status.FreeGPUs != 0 {
		t.Errorf("freeGPUs = %d, want 0", updated.Status.FreeGPUs)
	}

	if _, _, _, _, err := r.findAndAllocate(ctx, g3, profile); err == nil {
		t.Error("third 4-GPU tenant must fail: no free partition with free members")
	}
}

// A COUNT-matching free partition whose members are (partially) held must be
// skipped in favor of one whose members are all free — the case a count-only
// partition pick gets wrong (it would activate a partition overlapping another
// tenant's devices).
func TestFindAndAllocate_HGXShared_SkipsPartitionWithHeldMembers(t *testing.T) {
	node := hgxH100Node("hgx-0")
	// Hold ONE member of partition 1 (physicalId 1 → index 4 → 0000:86:00.0)
	// for another guest, without touching partition records.
	for j := range node.Status.GPUs {
		if node.Status.GPUs[j].Index == 4 {
			node.Status.GPUs[j].Allocated = true
			node.Status.GPUs[j].AllocatedTo = "default/other"
		}
	}
	node.Status.FreeGPUs = 7

	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	profile := testGPUProfile("hgx4", "default", "hgx-shared", "", 4, "shared")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, guest, profile).
		WithStatusSubresource(node, guest).
		Build()
	r := &SwiftGPUReconciler{Client: c, Scheme: scheme.Scheme}

	_, gpus, _, partID, err := r.findAndAllocate(context.Background(), guest, profile)
	if err != nil {
		t.Fatalf("findAndAllocate: %v", err)
	}
	if partID != 2 {
		t.Fatalf("must skip partition 1 (member held) and select partition 2, got %d", partID)
	}
	members := partitionMembers(node, 2)
	for _, g := range gpus {
		if !members[g.PCIAddress] {
			t.Errorf("GPU %s not in partition 2 %v", g.PCIAddress, members)
		}
	}
}

// The whole-baseboard (Tier-3-shaped) 8-GPU partition allocates all devices.
func TestFindAndAllocate_HGXShared_FullBaseboard(t *testing.T) {
	node := hgxH100Node("hgx-0")
	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx8"})
	profile := testGPUProfile("hgx8", "default", "hgx-shared", "", 8, "shared")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, guest, profile).
		WithStatusSubresource(node, guest).
		Build()
	r := &SwiftGPUReconciler{Client: c, Scheme: scheme.Scheme}

	_, gpus, numa, partID, err := r.findAndAllocate(context.Background(), guest, profile)
	if err != nil {
		t.Fatalf("findAndAllocate: %v", err)
	}
	if partID != 0 || len(gpus) != 8 {
		t.Fatalf("want partition 0 with 8 GPUs, got partition %d with %d", partID, len(gpus))
	}
	if len(numa) != 2 {
		t.Errorf("8 GPUs span both NUMA nodes, got %v", numa)
	}
}

// ReserveOnNode (the migration reserve-before-stop primitive) must apply the
// same partition-membership coupling as findAndAllocate.
func TestReserveOnNode_HGXShared_PartitionMembershipCoupled(t *testing.T) {
	node := hgxH100Node("hgx-target")
	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	profile := testGPUProfile("hgx4", "default", "hgx-shared", "", 4, "shared")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, guest, profile).
		WithStatusSubresource(node, guest).
		Build()

	gpus, _, partID, err := ReserveOnNode(context.Background(), c, guest, profile, "hgx-target")
	if err != nil {
		t.Fatalf("ReserveOnNode: %v", err)
	}
	members := partitionMembers(node, partID)
	for _, g := range gpus {
		if !members[g.PCIAddress] {
			t.Errorf("reserved GPU %s outside activated partition %d", g.PCIAddress, partID)
		}
	}
}

// GPUNodeHasCapacity must agree with the selection: enough FREE GPUs by count
// is NOT capacity when every count-matching partition has a held member.
func TestGPUNodeHasCapacity_HGXShared_HeldMembersFail(t *testing.T) {
	node := hgxH100Node("hgx-0")
	// Hold one member of EACH 4-GPU partition (indices 4 and 0) for others:
	// 6 GPUs remain free (>= 4), but no viable 4-GPU partition exists.
	for j := range node.Status.GPUs {
		if node.Status.GPUs[j].Index == 4 || node.Status.GPUs[j].Index == 0 {
			node.Status.GPUs[j].Allocated = true
			node.Status.GPUs[j].AllocatedTo = "default/other"
		}
	}
	node.Status.FreeGPUs = 6

	profile := testGPUProfile("hgx4", "default", "hgx-shared", "", 4, "shared")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node).
		WithStatusSubresource(node).
		Build()

	if err := GPUNodeHasCapacity(context.Background(), c, "hgx-0", profile); err == nil {
		t.Error("capacity check must fail: 6 free GPUs but no 4-GPU partition with all members free")
	}
	// A pair is still viable: partition 4 ({2,4}→indices 6,7) has free members.
	pair := testGPUProfile("hgx2", "default", "hgx-shared", "", 2, "shared")
	if err := GPUNodeHasCapacity(context.Background(), c, "hgx-0", pair); err != nil {
		t.Errorf("2-GPU capacity should pass (partition with free members exists): %v", err)
	}
}
