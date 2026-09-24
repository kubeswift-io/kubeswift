package swiftsandbox

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// sandboxGPUReleaseWait paces the wait for a launcher to let go of its GPU.
const sandboxGPUReleaseWait = 5 * time.Second

// launcherPodName is the sandbox's launcher pod: the claimed slot's pod for a
// pooled checkout, else the cold pod named after the sandbox.
func launcherPodName(sb *sandboxv1alpha1.SwiftSandbox) string {
	if sb.Status.PodRef != "" {
		return sb.Status.PodRef
	}
	return sb.Name
}

// launcherMayHoldGPU reports whether the sandbox's launcher pod may still hold
// its VFIO group: it exists and its containers have not all exited. A
// Terminating pod still counts -- its Cloud Hypervisor holds the group until
// it exits -- and releasing the GPU then let the next consumer's VFIO bind
// fail "Resource busy". The pod is returned for the caller to act on.
func (r *SwiftSandboxReconciler) launcherMayHoldGPU(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (bool, *corev1.Pod, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: sb.Namespace, Name: launcherPodName(sb)}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false, &pod, nil
	}
	return true, &pod, nil
}

// releaseGPUWhenDone returns a finished sandbox's native GPU once its launcher
// has exited. A Completed/Failed sandbox is kept until its TTL (or forever
// without one) for its status and logs, and it used to keep the GPU reserved
// that whole time.
func (r *SwiftSandboxReconciler) releaseGPUWhenDone(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	if sb.Status.GPU == nil || !controllerutil.ContainsFinalizer(sb, sandboxGPUFinalizer) {
		return ctrl.Result{}, nil
	}
	held, _, err := r.launcherMayHoldGPU(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}
	if held {
		return ctrl.Result{RequeueAfter: sandboxGPUReleaseWait}, nil
	}
	if err := r.releaseNativeGPU(ctx, sb); err != nil {
		return ctrl.Result{}, err
	}
	sb.Status.GPU = nil
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type: sandboxv1alpha1.SwiftSandboxConditionGPUAllocated, Status: metav1.ConditionFalse,
		Reason: "Released", Message: "sandbox finished; GPU returned to the pool", ObservedGeneration: sb.Generation,
	})
	return ctrl.Result{}, r.Status().Update(ctx, sb)
}

// handleDeletion releases a native GPU allocation (if the finalizer is present)
// and removes the finalizer so GC can proceed. A no-op for sandboxes that never
// held a native GPU.
//
// The launcher is deleted first and the release waits until it has exited.
// It is owned by the sandbox, but background GC removes it only after the
// sandbox is gone -- i.e. after this finalizer -- so releasing straight away
// freed a GPU whose VFIO group the still-running launcher held.
func (r *SwiftSandboxReconciler) handleDeletion(ctx context.Context, sb *sandboxv1alpha1.SwiftSandbox) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(sb, sandboxGPUFinalizer) {
		held, pod, err := r.launcherMayHoldGPU(ctx, sb)
		if err != nil {
			return ctrl.Result{}, err
		}
		if held {
			if pod.DeletionTimestamp == nil && metav1.IsControlledBy(pod, sb) {
				if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
			}
			return ctrl.Result{RequeueAfter: sandboxGPUReleaseWait}, nil
		}
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
