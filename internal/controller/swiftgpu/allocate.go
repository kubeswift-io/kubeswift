package swiftgpu

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

var errNoCapacity = errors.New("no GPU node has sufficient capacity")

// findAndAllocate finds a suitable SwiftGPUNode, marks the GPUs as allocated
// in its status, and returns the selected GPUs and associated metadata.
//
// Idempotent: if GPUs are already marked as allocated to this guest (e.g. a
// previous reconcile updated the node but failed to patch the guest status),
// they are returned without re-allocating.
func (r *SwiftGPUReconciler) findAndAllocate(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	profile *gpuv1alpha1.SwiftGPUProfile,
) (node *gpuv1alpha1.SwiftGPUNode, selectedGPUs []gpuv1alpha1.GPUDevice, numaNodes []int, partitionID int, err error) {

	var nodeList gpuv1alpha1.SwiftGPUNodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return nil, nil, nil, -1, err
	}

	allocatedTo := guest.Namespace + "/" + guest.Name

	// The node status.GPU already references, if any. The first pass prefers it.
	preferredNode := ""
	if guest.Status.GPU != nil {
		preferredNode = guest.Status.GPU.NodeName
	}

	// First pass: return an existing allocation for this guest (idempotency
	// guard — handles partial status patch failure). PREFER the node
	// status.GPU already points at over the first node found.
	//
	// W-GPU-1: during a VFIO offline migration's reserve-before-stop window the
	// guest is briefly allocated on BOTH the source and the target node (the
	// reservation reuses AllocatedTo). Returning the FIRST allocated node would
	// make this controller re-stamp status.GPU to that node — racing the
	// migration controller, which owns status.GPU during the migration (it sets
	// status.GPU=target at cutover). Preferring the node status.GPU already
	// references makes the re-stamp a no-op that never fights the migration:
	// status.GPU=source during reserve → returns source; status.GPU=target
	// after cutover → returns target.
	var fbNode *gpuv1alpha1.SwiftGPUNode
	var fbGPUs []gpuv1alpha1.GPUDevice
	var fbNUMA []int
	fbPart := -1
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		var existing []gpuv1alpha1.GPUDevice
		numaSet := map[int]bool{}
		existingPartID := -1

		for _, g := range n.Status.GPUs {
			if g.AllocatedTo == allocatedTo {
				existing = append(existing, g)
				numaSet[g.NUMANode] = true
			}
		}
		if n.Status.FabricManager != nil {
			for _, p := range n.Status.FabricManager.Partitions {
				if p.AllocatedTo == allocatedTo {
					existingPartID = p.ID
				}
			}
		}
		if len(existing) == 0 {
			continue
		}
		if preferredNode != "" && n.Name == preferredNode {
			return n, existing, numaSetToSlice(numaSet), existingPartID, nil
		}
		if fbNode == nil {
			fbNode, fbGPUs, fbNUMA, fbPart = n, existing, numaSetToSlice(numaSet), existingPartID
		}
	}
	if fbNode != nil {
		return fbNode, fbGPUs, fbNUMA, fbPart, nil
	}

	// Second pass: find a node with capacity and allocate.
	fmVersionMismatch := false
	for i := range nodeList.Items {
		n := &nodeList.Items[i]

		// Require at least profile.Count free GPUs.
		if n.Status.FreeGPUs < profile.Spec.Count {
			continue
		}

		// Optional model filter: profile.Model must be a substring of the node's GPUModel.
		if profile.Spec.Model != "" && !strings.Contains(n.Status.GPUModel, profile.Spec.Model) {
			continue
		}

		// Fabric Manager version gate (shared NVSwitch mode). Per NVIDIA
		// WP-12736-002, the host Fabric Manager version MUST exactly match the
		// guest nvidia-open driver version or partition activation/attach fails
		// — a broken fabric, not a boot failure, so it must be caught before
		// allocation (Design Principle #6: no silent failures). Skip the node;
		// if version mismatch turns out to be the sole blocker the final error
		// names it (rather than a bare "no capacity").
		if !fmVersionCompatible(n, profile) {
			fmVersionMismatch = true
			continue
		}

		// Select GPUs. In shared partition mode the Fabric Manager partition is
		// the unit of allocation: the NVSwitch fabric is programmed to allow
		// NVLink among ONLY the GPUs within the activated partition (NVIDIA HGX
		// Shared NVSwitch integration guide, WP-12736-002), so the guest MUST
		// receive exactly that partition's member GPUs. Selecting GPUs by NUMA
		// locality and a partition by count independently hands the guest GPUs
		// the activated partition doesn't cover — no NVLink for this tenant,
		// and a fabric cross-wired against another tenant's devices.
		var gpus []gpuv1alpha1.GPUDevice
		var numa []int
		partID := -1
		if profile.Spec.PartitionMode == "shared" {
			gpus, numa, partID = selectPartitionGPUs(n, profile.Spec.Count, profile.Spec.Model)
			if gpus == nil {
				// No free partition whose member GPUs are all free — try the
				// next node.
				continue
			}
		} else {
			// No fabric constraint: prefer same-NUMA GPUs for locality.
			gpus, numa = selectGPUs(n.Status.GPUs, profile.Spec.Count, profile.Spec.Model)
			if gpus == nil {
				continue
			}
		}

		// Mark GPUs as allocated (controller-owned fields only).
		for _, g := range gpus {
			for j := range n.Status.GPUs {
				if n.Status.GPUs[j].PCIAddress == g.PCIAddress {
					n.Status.GPUs[j].Allocated = true
					n.Status.GPUs[j].AllocatedTo = allocatedTo
				}
			}
		}

		// Mark FM partition as allocated.
		if partID >= 0 && n.Status.FabricManager != nil {
			for j := range n.Status.FabricManager.Partitions {
				if n.Status.FabricManager.Partitions[j].ID == partID {
					n.Status.FabricManager.Partitions[j].AllocatedTo = allocatedTo
				}
			}
		}

		// Recompute freeGPUs from allocation state.
		n.Status.FreeGPUs = countFreeGPUs(n.Status.GPUs)

		if err := r.Status().Update(ctx, n); err != nil {
			return nil, nil, nil, -1, fmt.Errorf("update SwiftGPUNode %s status: %w", n.Name, err)
		}

		return n, gpus, numa, partID, nil
	}

	if fmVersionMismatch {
		return nil, nil, nil, -1, fmt.Errorf("%w: candidate GPU node(s) run a Fabric Manager version that does not match profile.spec.fabricManager.requiredVersion=%q (for shared NVSwitch mode the host Fabric Manager version must exactly match the guest driver version)",
			errNoCapacity, profile.Spec.FabricManager.RequiredVersion)
	}
	return nil, nil, nil, -1, errNoCapacity
}

// fmVersionCompatible reports whether node's Fabric Manager version satisfies
// the profile's RequiredVersion.
//
// Per NVIDIA WP-12736-002, in shared NVSwitch mode the host Fabric Manager
// version MUST exactly match the guest nvidia-open driver version
// (profile.spec.fabricManager.requiredVersion) — the FM partition activate/attach
// handshake with the guest driver fails otherwise. This applies to SHARED mode
// only: in full mode (Tier 3) Fabric Manager runs IN the guest, so the host FM
// version is irrelevant. An empty RequiredVersion means the operator did not pin
// a version -> no check (any FM version accepted).
func fmVersionCompatible(node *gpuv1alpha1.SwiftGPUNode, profile *gpuv1alpha1.SwiftGPUProfile) bool {
	if profile.Spec.PartitionMode != "shared" {
		return true
	}
	if profile.Spec.FabricManager == nil || profile.Spec.FabricManager.RequiredVersion == "" {
		return true
	}
	fm := node.Status.FabricManager
	return fm != nil && fm.Version == profile.Spec.FabricManager.RequiredVersion
}

// fmVersionString returns node's Fabric Manager version for error messages, or a
// placeholder when Fabric Manager is not reported on the node.
func fmVersionString(node *gpuv1alpha1.SwiftGPUNode) string {
	if node.Status.FabricManager == nil {
		return "<no Fabric Manager reported>"
	}
	return node.Status.FabricManager.Version
}

// deallocateGPUs releases the GPU allocation recorded in guest.status.gpu from
// the SwiftGPUNode. No-op if no allocation is recorded or the node is gone.
func (r *SwiftGPUReconciler) deallocateGPUs(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error {
	if guest.Status.GPU == nil {
		return nil
	}
	// Free the guest's GPUs (and FM partitions) on EVERY SwiftGPUNode, not just
	// status.GPU.NodeName. During a VFIO offline migration's reserve-before-stop
	// window the guest is briefly allocated on BOTH the source
	// (status.GPU.NodeName) and the target node — the reservation reuses
	// GPUDevice.AllocatedTo. If the guest is deleted in that window, releasing
	// only status.GPU.NodeName would strand the target reservation forever
	// (AllocatedTo a now-deleted guest, never freed). Listing all nodes and
	// releasing each covers both the source allocation and any held reservation.
	// ReleaseFromNode is idempotent (a no-op on nodes the guest doesn't hold),
	// so this is safe and the per-node Get cost is trivial (few GPU nodes).
	// (Design doc vfio-release-reallocate.md §10.1.)
	var nodes gpuv1alpha1.SwiftGPUNodeList
	if err := r.List(ctx, &nodes); err != nil {
		return fmt.Errorf("list SwiftGPUNodes for deallocation: %w", err)
	}
	for i := range nodes.Items {
		if err := ReleaseFromNode(ctx, r.Client, guest, nodes.Items[i].Name); err != nil {
			return err
		}
	}
	return nil
}

// selectGPUs picks count free GPUs from gpus, preferring GPUs on the same NUMA
// node for locality. Returns nil if fewer than count free GPUs are available.
func selectGPUs(gpus []gpuv1alpha1.GPUDevice, count int, model string) (selected []gpuv1alpha1.GPUDevice, numaNodes []int) {
	// Collect free GPUs, applying optional model filter.
	var free []gpuv1alpha1.GPUDevice
	for _, g := range gpus {
		if g.Allocated {
			continue
		}
		if model != "" && !strings.Contains(g.Model, model) {
			continue
		}
		free = append(free, g)
	}
	if len(free) < count {
		return nil, nil
	}

	// Group by NUMA node, iterate in deterministic order.
	byNUMA := map[int][]gpuv1alpha1.GPUDevice{}
	for _, g := range free {
		byNUMA[g.NUMANode] = append(byNUMA[g.NUMANode], g)
	}
	numaKeys := make([]int, 0, len(byNUMA))
	for k := range byNUMA {
		numaKeys = append(numaKeys, k)
	}
	sort.Ints(numaKeys)

	// Prefer a single NUMA node that can satisfy the full count.
	for _, numaID := range numaKeys {
		if len(byNUMA[numaID]) >= count {
			return byNUMA[numaID][:count], []int{numaID}
		}
	}

	// Fall back: take from multiple NUMA nodes in NUMA-ID order.
	numaSet := map[int]bool{}
	var picked []gpuv1alpha1.GPUDevice
	for _, numaID := range numaKeys {
		for _, g := range byNUMA[numaID] {
			if len(picked) >= count {
				break
			}
			picked = append(picked, g)
			numaSet[numaID] = true
		}
		if len(picked) >= count {
			break
		}
	}

	return picked, numaSetToSlice(numaSet)
}

// selectPartitionGPUs picks a free Fabric Manager partition with exactly count
// member GPUs whose members are ALL free (and match the optional model filter),
// and returns those member devices — the partition is the unit of allocation
// in shared NVSwitch mode. Partition GPUIndices reference GPUDevice.Index
// (gpu-discovery owns mapping the FM physical/module IDs — which do NOT follow
// lspci order, per nvidia-smi -q "Module ID" — onto the device inventory).
// When several partitions qualify, one whose members share a single NUMA node
// is preferred (locality, matching selectGPUs' preference). Returns nil when
// no viable partition exists.
func selectPartitionGPUs(
	n *gpuv1alpha1.SwiftGPUNode,
	count int,
	model string,
) (selected []gpuv1alpha1.GPUDevice, numaNodes []int, partitionID int) {
	fm := n.Status.FabricManager
	if fm == nil || !fm.Running {
		return nil, nil, -1
	}
	byIndex := map[int]gpuv1alpha1.GPUDevice{}
	for _, g := range n.Status.GPUs {
		byIndex[g.Index] = g
	}

	var fbGPUs []gpuv1alpha1.GPUDevice
	var fbNUMA []int
	fbPart := -1
	for _, p := range fm.Partitions {
		if len(p.GPUIndices) != count || p.AllocatedTo != "" {
			continue
		}
		members := make([]gpuv1alpha1.GPUDevice, 0, count)
		numaSet := map[int]bool{}
		viable := true
		for _, idx := range p.GPUIndices {
			g, ok := byIndex[idx]
			if !ok || g.Allocated || (model != "" && !strings.Contains(g.Model, model)) {
				viable = false
				break
			}
			members = append(members, g)
			numaSet[g.NUMANode] = true
		}
		if !viable {
			continue
		}
		if len(numaSet) == 1 {
			return members, numaSetToSlice(numaSet), p.ID
		}
		if fbPart < 0 {
			fbGPUs, fbNUMA, fbPart = members, numaSetToSlice(numaSet), p.ID
		}
	}
	return fbGPUs, fbNUMA, fbPart
}

// countFreeGPUs recomputes the free GPU count from the allocation state.
func countFreeGPUs(gpus []gpuv1alpha1.GPUDevice) int {
	n := 0
	for _, g := range gpus {
		if !g.Allocated {
			n++
		}
	}
	return n
}

func numaSetToSlice(m map[int]bool) []int {
	s := make([]int, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Ints(s)
	return s
}
