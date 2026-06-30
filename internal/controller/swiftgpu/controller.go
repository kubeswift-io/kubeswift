package swiftgpu

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
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
	logger := log.FromContext(ctx)

	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, req.NamespacedName, &guest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Select the GPU allocation backend (native: gpuProfileRef; dra:
	// gpuResourceClaim). No GPU request -> nothing to do.
	backendName := guest.GPUBackend()
	if backendName == "" {
		return ctrl.Result{}, nil
	}
	backend := r.backend(backendName)

	// Handle deletion: release the allocation and remove the finalizer.
	if !guest.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&guest, GPUFinalizerName) {
			if err := backend.Release(ctx, &guest); err != nil {
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
	switch {
	case errors.As(err, &pnf):
		status := guest.Status.DeepCopy()
		setGPUAllocatedCondition(status, false, "ProfileNotFound", pnf.Error())
		if patchErr := r.patchStatus(ctx, guest, status); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	case errors.Is(err, errNoCapacity):
		// Transition-gated: count entering the NoCapacity state once, not every
		// 30s retry tick while capacity stays exhausted.
		if !hasGPUAllocatedReason(guest, "NoCapacity") {
			metrics.GPUAllocationsTotal.WithLabelValues("no_capacity").Inc()
		}
		status := guest.Status.DeepCopy()
		setGPUAllocatedCondition(status, false, "NoCapacity",
			"no SwiftGPUNode has sufficient free GPUs matching the profile")
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
	patch := client.MergeFrom(guest.DeepCopy())
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
