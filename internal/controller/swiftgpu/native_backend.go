package swiftgpu

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/gpualloc"
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

// UnsupportedTierError is returned by nativeBackend.Prepare when the guest's
// SwiftGPUProfile requests tier: hgx-full (Tier 3). The controller maps it to a
// GPUAllocated=False / UnsupportedTier condition + a 30s requeue.
//
// Tier 3 (full baseboard passthrough) needs the host's NVSwitches passed INTO the
// guest so an in-guest Fabric Manager can drive the fabric. gpu-discovery finds
// those NVSwitches and records them on SwiftGPUNode.status.nvSwitches, but the
// controller does not yet thread them into GPUIntent.NVSwitches (buildGPUIntent
// omits the field), so a Tier-3 guest would boot with GPUs but NO fabric and an
// in-guest FM with nothing to manage — a silent failure (Design Principle #6).
// Reject it honestly until the guest-side NVSwitch passthrough is wired and can be
// validated on real HGX hardware. Tier 2 (hgx-shared, host Fabric Manager, no
// guest NVSwitches) is unaffected.
type UnsupportedTierError struct{ Tier string }

func (e *UnsupportedTierError) Error() string {
	return fmt.Sprintf("tier %q (Tier 3, full baseboard passthrough) is not yet supported: "+
		"the guest-side NVSwitch passthrough that an in-guest Fabric Manager needs is not wired "+
		"(NVSwitches are discovered on the SwiftGPUNode but not passed into the guest). "+
		"Use tier: hgx-shared (Tier 2, host Fabric Manager) or tier: pcie", e.Tier)
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

	// Reject Tier 3 BEFORE allocating — it can't be delivered (see
	// UnsupportedTierError), so allocating GPUs for it would strand them on a guest
	// that boots without a fabric. Tier 2 (hgx-shared) is allowed: it uses the host
	// Fabric Manager and does not pass NVSwitches into the guest.
	if profile.Spec.Tier == "hgx-full" {
		return nil, &UnsupportedTierError{Tier: profile.Spec.Tier}
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
