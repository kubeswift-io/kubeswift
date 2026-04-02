package swiftgpu

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
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

	// Only handle guests with a GPU profile reference.
	if guest.Spec.GPUProfileRef == nil {
		return ctrl.Result{}, nil
	}

	// Handle deletion: release GPU allocation and remove finalizer.
	if !guest.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&guest, GPUFinalizerName) {
			if err := r.deallocateGPUs(ctx, &guest); err != nil {
				logger.Error(err, "GPU deallocation failed")
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&guest, GPUFinalizerName)
			if err := r.Update(ctx, &guest); err != nil {
				return ctrl.Result{}, err
			}
			logger.Info("GPU deallocation complete", "guest", req.NamespacedName)
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

	// Resolve the SwiftGPUProfile.
	var profile gpuv1alpha1.SwiftGPUProfile
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: guest.Namespace,
		Name:      guest.Spec.GPUProfileRef.Name,
	}, &profile); err != nil {
		status := guest.Status.DeepCopy()
		setGPUAllocatedCondition(status, false, "ProfileNotFound",
			fmt.Sprintf("SwiftGPUProfile %q not found", guest.Spec.GPUProfileRef.Name))
		if patchErr := r.patchStatus(ctx, &guest, status); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Find a suitable SwiftGPUNode and allocate GPUs.
	gpuNode, selectedGPUs, numaNodes, partitionID, err := r.findAndAllocate(ctx, &guest, &profile)
	if err != nil {
		if err == errNoCapacity {
			logger.Info("no GPU capacity available, will retry", "guest", req.NamespacedName,
				"count", profile.Spec.Count, "model", profile.Spec.Model)
			status := guest.Status.DeepCopy()
			setGPUAllocatedCondition(status, false, "NoCapacity",
				"no SwiftGPUNode has sufficient free GPUs matching the profile")
			if patchErr := r.patchStatus(ctx, &guest, status); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// Determine hypervisor from the GPU tier.
	hypervisor := "cloud-hypervisor"
	if profile.Spec.Tier == "hgx-shared" || profile.Spec.Tier == "hgx-full" {
		hypervisor = "qemu"
	}

	// Build the PCI address list from the selected GPU devices.
	devices := make([]string, len(selectedGPUs))
	for i, g := range selectedGPUs {
		devices[i] = g.PCIAddress
	}

	// Record allocation in SwiftGuest status and set GPUAllocated=True.
	status := guest.Status.DeepCopy()
	status.GPU = &swiftv1alpha1.GPUStatus{
		Devices:     devices,
		PartitionID: partitionID,
		NUMANodes:   numaNodes,
		Hypervisor:  hypervisor,
		NodeName:    gpuNode.Name,
	}
	setGPUAllocatedCondition(status, true, "Allocated",
		fmt.Sprintf("allocated %d GPU(s) on node %s", len(devices), gpuNode.Name))
	if err := r.patchStatus(ctx, &guest, status); err != nil {
		// GPUs are already marked on the node. The finalizer ensures deallocation
		// will be attempted. On the next reconcile, findAndAllocate will detect the
		// existing allocation (AllocatedTo == namespace/name) and return it.
		return ctrl.Result{}, err
	}

	logger.Info("GPU allocation complete",
		"guest", req.NamespacedName,
		"node", gpuNode.Name,
		"devices", devices,
		"hypervisor", hypervisor,
		"partitionID", partitionID)

	return ctrl.Result{}, nil
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
func (r *SwiftGPUReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&swiftv1alpha1.SwiftGuest{}).
		Watches(
			&gpuv1alpha1.SwiftGPUNode{},
			handler.EnqueueRequestsFromMapFunc(r.mapGPUNodeToSwiftGuests),
		).
		Complete(r)
}
