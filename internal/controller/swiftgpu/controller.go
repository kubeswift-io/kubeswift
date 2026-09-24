package swiftgpu

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/gpualloc"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

const (
	// GPUFinalizerName is added to SwiftGuests when GPU devices are allocated,
	// ensuring deallocation happens before the object is removed from the API server.
	GPUFinalizerName = "kubeswift.io/gpu-allocation"
)

// SwiftGPUReconciler allocates GPU devices for SwiftGuests that have gpuProfileRef set.
//
// Ownership boundaries:
//   - This controller is the SOLE writer of SwiftGPUNode.status fields:
//     gpus[].allocated, gpus[].allocatedTo, fabricManager.partitions[].allocatedTo, freeGPUs
//   - The GPU discovery DaemonSet (future) owns: phase, gpus[] device info (model, pciAddress,
//     driver, barSizes, numaNode, iommuGroup), host topology, nvSwitches, partitions[].active
//   - Never overwrite discovery-owned fields during allocation or deallocation.
type SwiftGPUReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile implements the reconcile loop.
func (r *SwiftGPUReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.reconcile(ctx, req)
	if apierrors.IsConflict(err) {
		// The guest changed under this pass; retry from the newer object.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return res, err
}

func (r *SwiftGPUReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, req.NamespacedName, &guest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Select the GPU allocation backend (native: gpuProfileRef; dra:
	// gpuResourceClaim). gpuProfileRef and gpuResourceClaim are mutable, so
	// what the spec asks for NOW is not necessarily what was allocated: release
	// below follows the allocation (the SwiftGPUNodes' AllocatedTo), not the
	// spec. It used to return here when the spec asked for nothing, so a guest
	// whose ref was removed after allocation never had its finalizer removed
	// (stuck Terminating) and its GPUs were never freed; switching native to
	// DRA ran DRA's no-op Release and leaked the native GPUs the same way.
	backendName := guest.GPUBackend()

	// Handle deletion: release the allocation and remove the finalizer -- but
	// not before the launcher pod is gone.
	//
	// A terminating launcher's Cloud Hypervisor still holds the VFIO group, so
	// releasing on DeletionTimestamp alone publishes "free" on the SwiftGPUNode
	// while the device is not. The next consumer then allocates it and fails to
	// boot with `failed to open /dev/vfio/<group> group: Resource busy`, and
	// (for a pool slot) stays in Error holding the allocation. The sandbox pool
	// already waits this way in reconcileSlotGPUGC; this is the same rule for
	// the SwiftGuest path.
	if !guest.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&guest, GPUFinalizerName) {
			held, podName, err := r.launcherStillPresent(ctx, &guest)
			if err != nil {
				return ctrl.Result{}, err
			}
			if held {
				logger.Info("GPU release deferred: launcher pod still present, it may still hold the VFIO group",
					"guest", req.NamespacedName, "pod", podName)
				return ctrl.Result{RequeueAfter: gpuReleaseWaitInterval}, nil
			}
			if err := r.releaseAll(ctx, &guest, backendName); err != nil {
				logger.Error(err, "GPU release failed", "backend", backendName)
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&guest, GPUFinalizerName)
			if err := r.Update(ctx, &guest); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("GPU release complete", "guest", req.NamespacedName, "backend", backendName)
		}
		return ctrl.Result{}, nil
	}

	// A guest that carries the finalizer was set up for GPUs at some point. If
	// it no longer asks for NATIVE GPUs but still holds some (ref removed, or
	// switched to DRA), return them once the launcher lets go of the VFIO group.
	if backendName != swiftv1alpha1.GPUBackendNative && controllerutil.ContainsFinalizer(&guest, GPUFinalizerName) {
		if res, waiting, err := r.releaseStaleNative(ctx, &guest, logger); err != nil || waiting {
			return res, err
		}
		if backendName == "" {
			// Nothing requested and nothing held: the finalizer has no job left.
			controllerutil.RemoveFinalizer(&guest, GPUFinalizerName)
			if err := r.Update(ctx, &guest); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}
	if backendName == "" {
		return ctrl.Result{}, nil
	}
	backend := r.backend(backendName)

	// Ensure the finalizer is present before any allocation work.
	if !controllerutil.ContainsFinalizer(&guest, GPUFinalizerName) {
		controllerutil.AddFinalizer(&guest, GPUFinalizerName)
		if err := r.Update(ctx, &guest); err != nil {
			return ctrl.Result{}, err
		}
	}

	// If GPUAllocated is already True, nothing to do.
	if isGPUAllocated(&guest) {
		return ctrl.Result{}, nil
	}

	// Phase 1: Prepare. native resolves here (decides node+devices); dra defers
	// to the scheduler and returns Resolved=false.
	pr, err := backend.Prepare(ctx, &guest)
	if err != nil {
		return r.handlePrepareError(ctx, &guest, err)
	}
	if pr.Resolved {
		return r.commitAllocation(ctx, &guest, pr.Status, logger)
	}

	// Phase 2 (dra): the launcher pod carries a ResourceClaim; mark
	// GPUClaimPending so the SwiftGuest controller builds it, then Resolve once
	// the scheduler/DRA driver has allocated a device.
	return r.reconcileDeferred(ctx, &guest, backend, logger)
}

// staleNativeReleaseRecheck paces the re-check while a guest that no longer
// asks for native GPUs still runs a launcher holding them. The guest's own
// status change when its launcher goes away re-triggers the reconcile; this is
// only the fallback.
const staleNativeReleaseRecheck = time.Minute

// releaseAll frees everything the guest may hold: native allocations by
// identity on every SwiftGPUNode (idempotent — a no-op when none), plus the
// current DRA backend's release when the spec uses DRA.
func (r *SwiftGPUReconciler) releaseAll(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, backendName string) error {
	if err := DeallocateForWorkload(ctx, r.Client, guest.Namespace+"/"+guest.Name); err != nil {
		return err
	}
	if backendName == swiftv1alpha1.GPUBackendDRA {
		return r.backend(backendName).Release(ctx, guest)
	}
	return nil
}

// releaseStaleNative returns native GPUs a guest still holds but no longer
// requests. waiting is true while its launcher is still present — it may still
// hold the VFIO group, and publishing the GPU as free then would let the next
// consumer allocate a busy device. After release, the stale native allocation
// is cleared from status so nothing is built from it.
func (r *SwiftGPUReconciler) releaseStaleNative(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, logger logr.Logger) (ctrl.Result, bool, error) {
	key := guest.Namespace + "/" + guest.Name
	held, err := nativeAllocationHeld(ctx, r.Client, key)
	if err != nil || !held {
		return ctrl.Result{}, false, err
	}
	present, podName, err := r.launcherStillPresent(ctx, guest)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if present {
		logger.Info("native GPU release deferred: no longer requested, but the launcher may still hold the VFIO group",
			"guest", key, "pod", podName)
		return ctrl.Result{RequeueAfter: staleNativeReleaseRecheck}, true, nil
	}
	if err := DeallocateForWorkload(ctx, r.Client, key); err != nil {
		return ctrl.Result{}, false, err
	}
	status := guest.Status.DeepCopy()
	status.GPU = nil
	apimeta.RemoveStatusCondition(&status.Conditions, swiftv1alpha1.ConditionGPUAllocated)
	if err := r.patchStatus(ctx, guest, status); err != nil {
		return ctrl.Result{}, false, err
	}
	logger.Info("released native GPUs no longer requested", "guest", key)
	return ctrl.Result{}, false, nil
}

// backend returns the gpualloc.Backend for the given backend name.
func (r *SwiftGPUReconciler) backend(name string) gpualloc.Backend {
	if name == swiftv1alpha1.GPUBackendDRA {
		return gpualloc.NewDRABackend(r.Client)
	}
	return &nativeBackend{r: r}
}

// handlePrepareError maps the typed allocation errors to the same conditions /
// requeue / metrics the native path produced before the refactor.
func (r *SwiftGPUReconciler) handlePrepareError(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, err error) (ctrl.Result, error) {
	var pnf *ProfileNotFoundError
	var ute *UnsupportedTierError
	switch {
	case errors.As(err, &pnf):
		status := guest.Status.DeepCopy()
		setGPUAllocatedCondition(status, false, "ProfileNotFound", pnf.Error())
		if patchErr := r.patchStatus(ctx, guest, status); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	case errors.As(err, &ute):
		// Tier 3 requested but not deliverable — surface it as a condition (no silent
		// failure) rather than booting a fabric-less guest. No GPUs were allocated
		// (the tier check precedes findAndAllocate), so nothing to release.
		status := guest.Status.DeepCopy()
		setGPUAllocatedCondition(status, false, "UnsupportedTier", ute.Error())
		if patchErr := r.patchStatus(ctx, guest, status); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	case errors.Is(err, ErrNoCapacity):
		// Transition-gated: count entering the NoCapacity state once, not every
		// 30s retry tick while capacity stays exhausted.
		if !hasGPUAllocatedReason(guest, "NoCapacity") {
			metrics.GPUAllocationsTotal.WithLabelValues("no_capacity").Inc()
		}
		status := guest.Status.DeepCopy()
		// Surface the error detail (e.g. a Fabric Manager version mismatch that
		// was the sole blocker) rather than a bare "no capacity" — no silent
		// failures (Design Principle #6). findAndAllocate wraps ErrNoCapacity
		// with the specific reason when there is one.
		msg := "no SwiftGPUNode has sufficient free GPUs matching the profile"
		if detail := err.Error(); detail != ErrNoCapacity.Error() {
			msg = detail
		}
		setGPUAllocatedCondition(status, false, "NoCapacity", msg)
		if patchErr := r.patchStatus(ctx, guest, status); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	default:
		return ctrl.Result{}, err
	}
}

// commitAllocation stamps the resolved GPUStatus and sets GPUAllocated=True.
func (r *SwiftGPUReconciler) commitAllocation(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, gpuStatus *swiftv1alpha1.GPUStatus, logger logr.Logger) (ctrl.Result, error) {
	status := guest.Status.DeepCopy()
	status.GPU = gpuStatus
	setGPUAllocatedCondition(status, true, "Allocated",
		fmt.Sprintf("allocated %d GPU(s) on node %s", len(gpuStatus.Devices), gpuStatus.NodeName))
	// Clear any GPUClaimPending left over from the DRA deferred path.
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: swiftv1alpha1.ConditionGPUClaimPending, Status: metav1.ConditionFalse,
		Reason: "Allocated", Message: "device allocated",
	})
	// At most once per successful allocation: the isGPUAllocated early return
	// means this path only runs while the condition is not yet True (a
	// status-patch-failure retry may rarely re-count — acceptable).
	metrics.GPUAllocationsTotal.WithLabelValues("allocated").Inc()
	if err := r.patchStatus(ctx, guest, status); err != nil {
		// GPUs are already marked (native) / the claim is allocated (dra). The
		// finalizer ensures release will be attempted; the next reconcile
		// re-detects the existing allocation.
		return ctrl.Result{}, err
	}
	logger.Info("GPU allocation complete",
		"guest", client.ObjectKeyFromObject(guest),
		"node", gpuStatus.NodeName,
		"devices", gpuStatus.Devices,
		"hypervisor", gpuStatus.Hypervisor)
	return ctrl.Result{}, nil
}

// reconcileDeferred drives the DRA (scheduler-time) allocation: mark
// GPUClaimPending (so the SwiftGuest controller builds the claim-bearing pod),
// then poll Resolve until the scheduler/DRA driver has allocated a device.
func (r *SwiftGPUReconciler) reconcileDeferred(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, backend gpualloc.Backend, logger logr.Logger) (ctrl.Result, error) {
	if !apimeta.IsStatusConditionTrue(guest.Status.Conditions, swiftv1alpha1.ConditionGPUClaimPending) {
		status := guest.Status.DeepCopy()
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: swiftv1alpha1.ConditionGPUClaimPending, Status: metav1.ConditionTrue,
			Reason:  "AwaitingScheduler",
			Message: "launcher pod created with a ResourceClaim; awaiting scheduler/DRA device allocation",
		})
		if err := r.patchStatus(ctx, guest, status); err != nil {
			return ctrl.Result{}, err
		}
	}
	res, err := backend.Resolve(ctx, guest)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !res.Ready {
		// Pod not scheduled / claim not allocated yet — poll.
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return r.commitAllocation(ctx, guest, res.Status, logger)
}

func (r *SwiftGPUReconciler) patchStatus(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, status *swiftv1alpha1.SwiftGuestStatus) error {
	if equality.Semantic.DeepEqual(guest.Status, *status) {
		return nil
	}
	// Optimistic lock: status.conditions is an atomic list, and setting
	// GPUAllocated resends all of it. From a stale read that would put back
	// an old GuestRunning written by swiftletd. A conflict retries with a
	// fresh read (see Reconcile).
	patch := client.MergeFromWithOptions(guest.DeepCopy(), client.MergeFromWithOptimisticLock{})
	guest.Status = *status
	return r.Status().Patch(ctx, guest, patch)
}

// mapGPUNodeToSwiftGuests enqueues SwiftGuests waiting for GPU allocation when
// a SwiftGPUNode changes (capacity freed or node becomes ready).
func (r *SwiftGPUReconciler) mapGPUNodeToSwiftGuests(ctx context.Context, obj client.Object) []reconcile.Request {
	var list swiftv1alpha1.SwiftGuestList
	if err := r.List(ctx, &list, client.InNamespace(metav1.NamespaceAll)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		g := &list.Items[i]
		if g.Spec.GPUProfileRef != nil && !isGPUAllocated(g) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(g)})
		}
	}
	return reqs
}

// SetupWithManager registers the reconciler with the manager.
// The controller is named explicitly to avoid collision with the SwiftGuest controller,
// since both watch SwiftGuest resources.
func (r *SwiftGPUReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("swiftgpu").
		For(&swiftv1alpha1.SwiftGuest{}).
		Watches(
			&gpuv1alpha1.SwiftGPUNode{},
			handler.EnqueueRequestsFromMapFunc(r.mapGPUNodeToSwiftGuests),
		).
		Complete(r)
}

// guestPodLabelKey selects a guest's launcher pod. Matches podLabels() in the
// swiftguest controller.
const guestPodLabelKey = "swift.kubeswift.io/guest"

// gpuReleaseWaitInterval is how often deletion re-checks whether the launcher
// pod has gone. Short, because the wait is normally a few seconds of pod
// termination and the GPU is unusable by anyone else until it ends.
const gpuReleaseWaitInterval = 5 * time.Second

// launcherStillPresent reports whether the guest's launcher pod still exists,
// and its name for logging. A pod that is Terminating still counts: its Cloud
// Hypervisor can hold the VFIO group right up to the moment the pod object
// goes, which is precisely the window this guards.
func (r *SwiftGPUReconciler) launcherStillPresent(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (bool, string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(guest.Namespace),
		client.MatchingLabels{guestPodLabelKey: guest.Name},
	); err != nil {
		return false, "", err
	}
	if len(pods.Items) == 0 {
		return false, "", nil
	}
	return true, pods.Items[0].Name, nil
}
