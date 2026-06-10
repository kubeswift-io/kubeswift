package swiftgpu

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/metrics"
)

// ReserveOnNode reserves profile.Count GPUs (+ an FM partition for shared
// mode) for the guest on a SPECIFIC node, marking them AllocatedTo the guest
// on that node's SwiftGPUNode WITHOUT touching guest.Status.GPU. Returns the
// selected devices / NUMA nodes / FM partition id for the caller to stamp into
// status.GPU at migration cutover. Idempotent: a re-reserve returns the
// existing reservation on this node.
//
// This is the "reserve target before stopping the source" primitive for VFIO
// release-and-reallocate: the source keeps its own allocation + status.GPU
// while the target GPUs are held here. Because it does NOT write status.GPU,
// the SwiftGPU controller (which only allocates when status.GPU is nil) stays
// idle and the pod builder (which keys on status.GPU) is unaffected — but
// findAndAllocate for OTHER guests sees these GPUs as AllocatedTo this guest,
// so the reservation holds. Mirrors findAndAllocate's per-node selection
// (selectGPUs / findFMPartition / countFreeGPUs) constrained to one node.
//
// Callers should GPUNodeHasCapacity-pre-flight first; this re-checks (vfio-
// ready, free matching GPUs) so a stale pre-flight cannot over-commit.
func ReserveOnNode(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, profile *gpuv1alpha1.SwiftGPUProfile, nodeName string) (devices []gpuv1alpha1.GPUDevice, numaNodes []int, partitionID int, err error) {
	allocatedTo := guest.Namespace + "/" + guest.Name

	var node gpuv1alpha1.SwiftGPUNode
	if gerr := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); gerr != nil {
		return nil, nil, -1, fmt.Errorf("get SwiftGPUNode %q: %w", nodeName, gerr)
	}

	// Idempotency: an existing reservation for this guest on this node.
	var existing []gpuv1alpha1.GPUDevice
	numaSet := map[int]bool{}
	existingPart := -1
	for _, g := range node.Status.GPUs {
		if g.AllocatedTo == allocatedTo {
			existing = append(existing, g)
			numaSet[g.NUMANode] = true
		}
	}
	if node.Status.FabricManager != nil {
		for _, p := range node.Status.FabricManager.Partitions {
			if p.AllocatedTo == allocatedTo {
				existingPart = p.ID
			}
		}
	}
	if len(existing) > 0 {
		return existing, numaSetToSlice(numaSet), existingPart, nil
	}

	if !node.Status.VfioReady {
		return nil, nil, -1, fmt.Errorf("GPU node %q is not vfio-ready (vfio-pci not loaded)", nodeName)
	}
	if node.Status.FreeGPUs < profile.Spec.Count {
		return nil, nil, -1, fmt.Errorf("GPU node %q has %d free GPU(s), need %d", nodeName, node.Status.FreeGPUs, profile.Spec.Count)
	}
	if profile.Spec.Model != "" && !strings.Contains(node.Status.GPUModel, profile.Spec.Model) {
		return nil, nil, -1, fmt.Errorf("GPU node %q model %q does not match profile model %q", nodeName, node.Status.GPUModel, profile.Spec.Model)
	}

	gpus, numa := selectGPUs(node.Status.GPUs, profile.Spec.Count, profile.Spec.Model)
	if gpus == nil {
		return nil, nil, -1, fmt.Errorf("GPU node %q could not select %d matching GPU(s)", nodeName, profile.Spec.Count)
	}

	partID := -1
	if profile.Spec.PartitionMode == "shared" {
		partID, err = findFMPartition(node.Status.FabricManager, profile.Spec.Count)
		if err != nil {
			return nil, nil, -1, fmt.Errorf("GPU node %q: no free FM partition for %d GPU(s): %w", nodeName, profile.Spec.Count, err)
		}
	}

	for _, g := range gpus {
		for j := range node.Status.GPUs {
			if node.Status.GPUs[j].PCIAddress == g.PCIAddress {
				node.Status.GPUs[j].Allocated = true
				node.Status.GPUs[j].AllocatedTo = allocatedTo
			}
		}
	}
	if partID >= 0 && node.Status.FabricManager != nil {
		for j := range node.Status.FabricManager.Partitions {
			if node.Status.FabricManager.Partitions[j].ID == partID {
				node.Status.FabricManager.Partitions[j].AllocatedTo = allocatedTo
			}
		}
	}
	node.Status.FreeGPUs = countFreeGPUs(node.Status.GPUs)

	if uerr := c.Status().Update(ctx, &node); uerr != nil {
		return nil, nil, -1, fmt.Errorf("reserve GPUs on %q: %w", nodeName, uerr)
	}
	return gpus, numa, partID, nil
}

// ReleaseFromNode clears the guest's GPU (+ FM partition) reservation/
// allocation on a SPECIFIC node's SwiftGPUNode. Idempotent; no-op if the node
// is gone or nothing is allocated to the guest there.
//
// deallocateGPUs delegates here (releasing status.GPU.NodeName on guest
// delete); migration release-and-reallocate uses it to drop a pre-cutover
// reservation on the target (failure path) or to free the source at cutover.
func ReleaseFromNode(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, nodeName string) error {
	if nodeName == "" {
		return nil
	}
	var node gpuv1alpha1.SwiftGPUNode
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // node gone; nothing to clean up
		}
		return err
	}

	allocatedTo := guest.Namespace + "/" + guest.Name
	changed := false
	for i := range node.Status.GPUs {
		if node.Status.GPUs[i].AllocatedTo == allocatedTo {
			node.Status.GPUs[i].Allocated = false
			node.Status.GPUs[i].AllocatedTo = ""
			changed = true
		}
	}
	if node.Status.FabricManager != nil {
		for i := range node.Status.FabricManager.Partitions {
			if node.Status.FabricManager.Partitions[i].AllocatedTo == allocatedTo {
				node.Status.FabricManager.Partitions[i].AllocatedTo = ""
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	node.Status.FreeGPUs = countFreeGPUs(node.Status.GPUs)
	if err := c.Status().Update(ctx, &node); err != nil {
		return err
	}
	// Count only releases that actually freed something (the !changed
	// early-return above keeps idempotent no-op releases out).
	metrics.GPUReleasesTotal.Inc()
	return nil
}
