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

		// Select GPUs, preferring same NUMA node for locality.
		gpus, numa := selectGPUs(n.Status.GPUs, profile.Spec.Count, profile.Spec.Model)
		if gpus == nil {
			continue
		}

		// For shared partition mode, find an available Fabric Manager partition.
		partID := -1
		if profile.Spec.PartitionMode == "shared" {
			partID, err = findFMPartition(n.Status.FabricManager, profile.Spec.Count)
			if err != nil {
				// Try the next node.
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

	return nil, nil, nil, -1, errNoCapacity
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

// findFMPartition returns the ID of an unallocated Fabric Manager partition
// whose GPU count matches gpuCount.
func findFMPartition(fm *gpuv1alpha1.FabricManagerStatus, gpuCount int) (int, error) {
	if fm == nil || !fm.Running {
		return -1, fmt.Errorf("Fabric Manager is not running")
	}
	for _, p := range fm.Partitions {
		if len(p.GPUIndices) == gpuCount && p.AllocatedTo == "" {
			return p.ID, nil
		}
	}
	return -1, fmt.Errorf("no unallocated Fabric Manager partition for %d GPUs", gpuCount)
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
