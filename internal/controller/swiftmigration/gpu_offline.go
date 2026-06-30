package swiftmigration

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftgpu"
)

// GPU release-and-reallocate for offline migration of VFIO/GPU guests (the
// Phase 4 drain follow-on, design doc vfio-release-reallocate.md §5). All of
// this is gated on guest.HasVFIODevices() at the call sites, so non-GPU
// offline migration is byte-for-byte unchanged.
//
// Sequence (migration-controller-orchestrated, reserve-before-stop):
//   Validating  -> gpuPreflight: target node is vfio-ready + has free matching GPUs
//   Preparing   -> reserveTargetGPUs(T) BEFORE the source is stopped
//   StopAndCopy -> cutoverGPUs: ReleaseFromNode(S) + stamp status.GPU=T (so the
//                  pod builder's precedence rule holds when the dst pod builds)
//   Failed      -> releaseTargetReservation(T) on pre-cutover failure
//
// VFIO guests migrate OFFLINE only (CH cannot live-migrate a VFIO device); the
// webhook still rejects mode=live for them.

// resolveGPUProfile fetches the SwiftGPUProfile referenced by the guest.
func (r *SwiftMigrationReconciler) resolveGPUProfile(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*gpuv1alpha1.SwiftGPUProfile, error) {
	if guest.Spec.GPUProfileRef == nil {
		return nil, fmt.Errorf("guest %q has no gpuProfileRef", guest.Name)
	}
	var profile gpuv1alpha1.SwiftGPUProfile
	if err := r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: guest.Spec.GPUProfileRef.Name}, &profile); err != nil {
		return nil, fmt.Errorf("get SwiftGPUProfile %q: %w", guest.Spec.GPUProfileRef.Name, err)
	}
	return &profile, nil
}

// gpuPreflight (Validating) verifies the target node can host the guest's GPUs
// before the migration commits to anything — an early, clear rejection rather
// than a Preparing-time reserve failure. Fails the migration on miss.
func (r *SwiftMigrationReconciler) gpuPreflight(ctx context.Context, mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest) *phaseResult {
	profile, err := r.resolveGPUProfile(ctx, guest)
	if err != nil {
		return phaseFailure(fmt.Sprintf("GPU pre-flight: %v", err), "")
	}
	if err := swiftgpu.GPUNodeHasCapacity(ctx, r.Client, mig.Spec.Target.NodeName, profile); err != nil {
		return phaseFailure(fmt.Sprintf("GPU pre-flight: %v", err), "")
	}
	return nil
}

// reserveTargetGPUs (Preparing, BEFORE the source is stopped) reserves the
// target GPUs. Idempotent across re-entries. On failure the source has not yet
// been stopped, so failing here leaves it running and unharmed.
func (r *SwiftMigrationReconciler) reserveTargetGPUs(ctx context.Context, mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, status *migrationv1alpha1.SwiftMigrationStatus) *phaseResult {
	profile, err := r.resolveGPUProfile(ctx, guest)
	if err != nil {
		return phaseFailure(fmt.Sprintf("reserve target GPUs: %v", err), "")
	}
	if _, _, _, rerr := swiftgpu.ReserveOnNode(ctx, r.Client, guest, profile, mig.Spec.Target.NodeName); rerr != nil {
		// Reserve failed before the source was stopped — fail cleanly.
		return phaseFailure(fmt.Sprintf("reserve target GPUs on %q: %v", mig.Spec.Target.NodeName, rerr), "")
	}
	setPhaseDetail(status, fmt.Sprintf("reserved target GPUs on %q", mig.Spec.Target.NodeName))
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "GPUReserved",
			fmt.Sprintf("reserved target GPUs on %q for %q", mig.Spec.Target.NodeName, guest.Name))
	}
	return nil
}

// cutoverGPUs (StopAndCopy, before the runPolicy+nodeName spec patch) frees the
// source GPUs and stamps status.GPU=target so the precedence rule in the pod
// builder holds when the destination pod is created. Idempotent: a no-op once
// status.GPU already points at the target.
func (r *SwiftMigrationReconciler) cutoverGPUs(ctx context.Context, mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, status *migrationv1alpha1.SwiftMigrationStatus) *phaseResult {
	target := mig.Spec.Target.NodeName

	// Already cut over (re-entry): status.GPU points at the target.
	if guest.Status.GPU != nil && guest.Status.GPU.NodeName == target {
		return nil
	}

	profile, err := r.resolveGPUProfile(ctx, guest)
	if err != nil {
		return phaseTransient(fmt.Errorf("cutover GPUs: %w", err))
	}
	// Re-derive the reservation (idempotent) for the device/NUMA/partition set.
	devices, numaNodes, partID, rerr := swiftgpu.ReserveOnNode(ctx, r.Client, guest, profile, target)
	if rerr != nil {
		return phaseTransient(fmt.Errorf("cutover: re-derive target reservation on %q: %w", target, rerr))
	}

	// Free the source GPUs (the guest's pre-cutover node).
	sourceNode := ""
	if guest.Status.GPU != nil {
		sourceNode = guest.Status.GPU.NodeName
	}
	if sourceNode != "" && sourceNode != target {
		if err := swiftgpu.ReleaseFromNode(ctx, r.Client, guest, sourceNode); err != nil {
			return phaseTransient(fmt.Errorf("cutover: release source GPUs on %q: %w", sourceNode, err))
		}
	}

	// Stamp status.GPU = target. Preserve the resolved hypervisor.
	pciAddrs := make([]string, 0, len(devices))
	for _, d := range devices {
		pciAddrs = append(pciAddrs, d.PCIAddress)
	}
	hypervisor := ""
	if guest.Status.GPU != nil {
		hypervisor = guest.Status.GPU.Hypervisor
	}
	patch := client.MergeFrom(guest.DeepCopy())
	guest.Status.GPU = &swiftv1alpha1.GPUStatus{
		Devices:     pciAddrs,
		PartitionID: partID,
		NUMANodes:   numaNodes,
		Hypervisor:  hypervisor,
		NodeName:    target,
	}
	if err := r.Status().Patch(ctx, guest, patch); err != nil {
		return phaseTransient(fmt.Errorf("cutover: stamp status.GPU=%q: %w", target, err))
	}
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "GPUCutover",
			fmt.Sprintf("released source GPUs on %q, committed target GPUs on %q for %q", sourceNode, target, guest.Name))
	}
	return nil
}

// releaseTargetReservation drops the target-node GPU reservation. Called from
// the pre-cutover failure/cleanup path so a failed migration does not strand a
// reservation on the target; the source resumes on its own node with its
// existing GPUs (status.GPU is unchanged pre-cutover). Idempotent.
func (r *SwiftMigrationReconciler) releaseTargetReservation(ctx context.Context, mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest) error {
	// Only release the target if it is NOT the guest's committed node (it is
	// not, pre-cutover: status.GPU still points at the source). Guards against
	// freeing the live allocation if called post-cutover by mistake.
	if guest.Status.GPU != nil && guest.Status.GPU.NodeName == mig.Spec.Target.NodeName {
		return nil
	}
	if err := swiftgpu.ReleaseFromNode(ctx, r.Client, guest, mig.Spec.Target.NodeName); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
