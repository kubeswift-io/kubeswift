package swiftgpu

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
)

// GPUNodeHasCapacity is the GPU analogue of swiftmigration.NodeHasCapacity: it
// reports whether nodeName's SwiftGPUNode can host a guest using profile — the
// node is vfio-ready AND has at least profile.Count free GPUs matching the
// profile's model (and, for shared partition mode, a free Fabric Manager
// partition).
//
// Read-only (it allocates nothing). Intended for the VFIO release-and-
// reallocate sub-phase: the drain controller's GPU target selection and the
// migration GPU target pre-flight call this so a VFIO guest only ever targets
// a node that can actually host it — turning a would-be gpu-init Init:Error
// (e.g. vfio-pci not loaded) into an early, clear rejection. Returns nil when
// the node fits; a descriptive error otherwise.
func GPUNodeHasCapacity(ctx context.Context, c client.Client, nodeName string, profile *gpuv1alpha1.SwiftGPUProfile) error {
	var node gpuv1alpha1.SwiftGPUNode
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("node %q has no SwiftGPUNode (not a GPU node, or discovery has not run)", nodeName)
		}
		return fmt.Errorf("get SwiftGPUNode %q: %w", nodeName, err)
	}

	if !node.Status.VfioReady {
		return fmt.Errorf("GPU node %q is not vfio-ready (vfio-pci not loaded); load vfio-pci on the host", nodeName)
	}
	if node.Status.FreeGPUs < profile.Spec.Count {
		return fmt.Errorf("GPU node %q has %d free GPU(s), need %d", nodeName, node.Status.FreeGPUs, profile.Spec.Count)
	}
	if profile.Spec.Model != "" && !strings.Contains(node.Status.GPUModel, profile.Spec.Model) {
		return fmt.Errorf("GPU node %q model %q does not match profile model %q", nodeName, node.Status.GPUModel, profile.Spec.Model)
	}
	if profile.Spec.PartitionMode == "shared" {
		if _, err := findFMPartition(node.Status.FabricManager, profile.Spec.Count); err != nil {
			return fmt.Errorf("GPU node %q has no free Fabric Manager partition for %d GPU(s): %w", nodeName, profile.Spec.Count, err)
		}
	}
	return nil
}
