package swiftgpu

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/gpualloc"
)

// nativeBackend is the gpualloc.Backend for the native SwiftGPU model. It is a
// thin wrapper that MOVES NO ALLOCATION LOGIC — findAndAllocate, selectGPUs,
// findFMPartition and deallocateGPUs stay exactly as-is and are called here.
// The native model decides node+devices entirely in Prepare (controller-time),
// so Resolve is a no-op.
type nativeBackend struct {
	r *SwiftGPUReconciler
}

// ProfileNotFoundError is returned by nativeBackend.Prepare when the guest's
// SwiftGPUProfile does not exist; the controller maps it to the ProfileNotFound
// condition + a 30s requeue (preserving the prior behavior).
type ProfileNotFoundError struct{ Name string }

func (e *ProfileNotFoundError) Error() string {
	return fmt.Sprintf("SwiftGPUProfile %q not found", e.Name)
}

// Name implements gpualloc.Backend.
func (n *nativeBackend) Name() string { return swiftv1alpha1.GPUBackendNative }

// Prepare implements gpualloc.Backend: load the profile, findAndAllocate, and
// build the GPUStatus — i.e. the native model resolves fully here. Returns a
// ProfileNotFoundError (profile missing) or errNoCapacity (no node fits) for the
// controller to map to conditions/requeue/metrics.
func (n *nativeBackend) Prepare(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*gpualloc.PrepareResult, error) {
	var profile gpuv1alpha1.SwiftGPUProfile
	if err := n.r.Get(ctx, client.ObjectKey{
		Namespace: guest.Namespace,
		Name:      guest.Spec.GPUProfileRef.Name,
	}, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &ProfileNotFoundError{Name: guest.Spec.GPUProfileRef.Name}
		}
		return nil, err
	}

	gpuNode, selectedGPUs, numaNodes, partitionID, err := n.r.findAndAllocate(ctx, guest, &profile)
	if err != nil {
		return nil, err // errNoCapacity (or a transient error) — mapped by the controller.
	}

	hypervisor := "cloud-hypervisor"
	if profile.Spec.Tier == "hgx-shared" || profile.Spec.Tier == "hgx-full" {
		hypervisor = "qemu"
	}
	devices := make([]string, len(selectedGPUs))
	for i, g := range selectedGPUs {
		devices[i] = g.PCIAddress
	}

	return &gpualloc.PrepareResult{
		Resolved: true,
		Status: &swiftv1alpha1.GPUStatus{
			Devices:     devices,
			PartitionID: partitionID,
			NUMANodes:   numaNodes,
			Hypervisor:  hypervisor,
			NodeName:    gpuNode.Name,
		},
	}, nil
}

// Resolve implements gpualloc.Backend: the native model already produced the
// Status in Prepare, so this is a no-op that echoes the stamped status.
func (n *nativeBackend) Resolve(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*gpualloc.Resolution, error) {
	return &gpualloc.Resolution{Ready: true, Status: guest.Status.GPU}, nil
}

// Release implements gpualloc.Backend: free the guest's GPUs on every SwiftGPUNode.
func (n *nativeBackend) Release(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) error {
	return n.r.deallocateGPUs(ctx, guest)
}
