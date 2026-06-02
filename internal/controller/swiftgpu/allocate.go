package swiftgpu

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
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

	// First pass: check whether a previous allocation already exists for this
	// guest (idempotency guard — handles partial status patch failure).
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
		if len(existing) > 0 {
			nodes := numaSetToSlice(numaSet)
			return n, existing, nodes, existingPartID, nil
		}
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
	if guest.Status.GPU == nil || guest.Status.GPU.NodeName == "" {
		return nil
	}
	// Delegate to the exported ReleaseFromNode primitive (the same one the
	// migration release-and-reallocate path uses), keyed on the guest's
	// currently-allocated node.
	return ReleaseFromNode(ctx, r.Client, guest, guest.Status.GPU.NodeName)
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
