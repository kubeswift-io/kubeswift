package swiftsandbox

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftgpu"
)

// sandboxGPUFinalizer guards native SwiftGPU allocations so the devices reserved
// on the SwiftGPUNode are released before the SwiftSandbox is GC'd. Only added to
// native-GPU sandboxes: DRA claims (ownerRef-GC'd with the pod) and non-GPU
// sandboxes need no finalizer.
const sandboxGPUFinalizer = "kubeswift.io/sandbox-gpu-allocation"

// sandboxGPUAllocatedTo is the object-agnostic allocation identity for a sandbox
// — kind-qualified so it never collides with a SwiftGuest's "<ns>/<name>".
func sandboxGPUAllocatedTo(sb *sandboxv1alpha1.SwiftSandbox) string {
	return "sandbox:" + sb.Namespace + "/" + sb.Name
}

// reconcileNativeGPU runs the controller-time native SwiftGPU allocation for a
// sandbox with spec.gpuProfileRef, mirroring the SwiftGuest SwiftGPU controller:
// ensure the release finalizer, allocate GPUs on a SwiftGPUNode via the shared
// object-agnostic core, and stamp status.gpu. It reuses swiftgpu.FindAndAllocateFor
// (the same allocation the SwiftGuest native path uses), keyed on the sandbox's
// identity.
//
// ready=true means status.gpu is populated (the caller then builds the GPU pod
// pinned to status.gpu.nodeName); ready=false means "requeued / not yet
// allocated — stop this reconcile". No-op (ready=true) for non-native sandboxes
// (no GPU, or the DRA backend, which allocates at pod-schedule time).
func (r *SwiftSandboxReconciler) reconcileNativeGPU(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ready bool, res ctrl.Result, err error) {
	if sb.GPUBackend() != swiftv1alpha1.GPUBackendNative {
		return true, ctrl.Result{}, nil
	}

	// Ensure the release finalizer before any allocation work, so a delete
	// between allocation and the next reconcile still frees the GPUs.
	if !controllerutil.ContainsFinalizer(sb, sandboxGPUFinalizer) {
		controllerutil.AddFinalizer(sb, sandboxGPUFinalizer)
		if err := r.Update(ctx, sb); err != nil {
			return false, ctrl.Result{}, err
		}
	}

	if sb.Status.GPU != nil {
		return true, ctrl.Result{}, nil // already allocated
	}

	var profile gpuv1alpha1.SwiftGPUProfile
	if err := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: sb.Spec.GPUProfileRef.Name}, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ctrl.Result{RequeueAfter: 30 * time.Second}, r.setGPUUnallocated(ctx, sb, "ProfileNotFound",
				fmt.Sprintf("SwiftGPUProfile %q not found", sb.Spec.GPUProfileRef.Name))
		}
		return false, ctrl.Result{}, err
	}

	// Sandboxes boot mode-3 (Cloud Hypervisor direct-kernel). HGX tiers require
	// the QEMU disk-boot topology (pcie-root-port per device, OVMF), which the
	// firmware-less sandbox runtime does not have — reject them honestly rather
	// than allocate GPUs the sandbox can't pass through.
	if profile.Spec.Tier == "hgx-shared" || profile.Spec.Tier == "hgx-full" {
		return false, ctrl.Result{RequeueAfter: 30 * time.Second}, r.setGPUUnallocated(ctx, sb, "UnsupportedTier",
			fmt.Sprintf("GPU sandboxes support only tier: pcie (Cloud Hypervisor mode-3); profile %q is tier %q (use a SwiftGuest for the QEMU HGX path)",
				sb.Spec.GPUProfileRef.Name, profile.Spec.Tier))
	}

	node, gpus, numa, partID, allocErr := swiftgpu.FindAndAllocateFor(ctx, r.Client, sandboxGPUAllocatedTo(sb), "", &profile)
	if allocErr != nil {
		// No capacity (or an FM-version / vfio-ready gate). Surface the reason and
		// requeue — a freed GPU or a fixed node makes the next attempt succeed.
		return false, ctrl.Result{RequeueAfter: 30 * time.Second}, r.setGPUUnallocated(ctx, sb, "NoCapacity", allocErr.Error())
	}

	hypervisor := "cloud-hypervisor"
	if profile.Spec.Tier == "hgx-shared" || profile.Spec.Tier == "hgx-full" {
		hypervisor = "qemu"
	}
	devices := make([]string, len(gpus))
	for i, g := range gpus {
		devices[i] = g.PCIAddress
	}
	sb.Status.GPU = &swiftv1alpha1.GPUStatus{
		Devices:     devices,
		PartitionID: partID,
		NUMANodes:   numa,
		Hypervisor:  hypervisor,
		NodeName:    node.Name,
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGPUAllocated, Status: metav1.ConditionTrue,
		Reason: "Allocated", Message: fmt.Sprintf("allocated %d GPU(s) on node %s", len(devices), node.Name),
		ObservedGeneration: sb.Generation,
	})
	if err := r.Status().Update(ctx, sb); err != nil {
		// The GPUs are already marked on the SwiftGPUNode; the finalizer ensures
		// release, and the next reconcile re-detects the existing allocation.
		return false, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}

// setGPUUnallocated stamps a GPUAllocated=False condition (ProfileNotFound /
// NoCapacity) and persists it.
func (r *SwiftSandboxReconciler) setGPUUnallocated(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox, reason, msg string) error {
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGPUAllocated, Status: metav1.ConditionFalse,
		Reason: reason, Message: msg, ObservedGeneration: sb.Generation,
	})
	return r.Status().Update(ctx, sb)
}

// releaseNativeGPU frees the sandbox's native GPU allocation on every
// SwiftGPUNode (idempotent). Called on the deletion path.
func (r *SwiftSandboxReconciler) releaseNativeGPU(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) error {
	return swiftgpu.DeallocateForWorkload(ctx, r.Client, sandboxGPUAllocatedTo(sb))
}

// handleDeletion releases a native GPU allocation (if the finalizer is present)
// and removes the finalizer so GC can proceed. A no-op for sandboxes that never
// held a native GPU.
func (r *SwiftSandboxReconciler) handleDeletion(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(sb, sandboxGPUFinalizer) {
		if err := r.releaseNativeGPU(ctx, sb); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(sb, sandboxGPUFinalizer)
		if err := r.Update(ctx, sb); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}
