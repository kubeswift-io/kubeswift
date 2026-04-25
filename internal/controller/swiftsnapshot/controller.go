// Package swiftsnapshot reconciles SwiftSnapshot resources.
//
// Phase 1 supports the csi-volume-snapshot backend only. The state machine
// is Pending -> Capturing -> Ready (or Failed). The controller creates a
// snapshot.storage.k8s.io VolumeSnapshot of the SwiftGuest's per-guest
// root-disk clone PVC, then waits for readyToUse=true before flipping the
// SwiftSnapshot to Ready.
//
// Local and S3 backends are reserved for later phases; this controller
// rejects them up-front with a Failed condition. The validation webhook
// also rejects them, but defense in depth.
package swiftsnapshot

import (
	"context"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// SwiftSnapshotReconciler reconciles SwiftSnapshot resources.
type SwiftSnapshotReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile drives the SwiftSnapshot state machine.
func (r *SwiftSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Terminal states: nothing to do.
	if snap.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseReady ||
		snap.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		return ctrl.Result{}, nil
	}

	phase := snap.Status.Phase
	if phase == "" {
		phase = snapshotv1alpha1.SwiftSnapshotPhasePending
	}
	status := snap.Status.DeepCopy()

	switch phase {
	case snapshotv1alpha1.SwiftSnapshotPhasePending:
		result, requeue, err := r.handlePending(ctx, &snap, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !result {
			// Not yet ready to advance — persist any progress + requeue.
			if updateErr := r.persist(ctx, &snap, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}

	case snapshotv1alpha1.SwiftSnapshotPhaseCapturing:
		ready, errMsg, err := r.handleCapturing(ctx, &snap, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if errMsg != "" {
			setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonSnapshotFailed, errMsg)
		} else if !ready {
			if updateErr := r.persist(ctx, &snap, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

	default:
		// Unknown phase — treat as Pending. Future-phase additions
		// (Uploading) live in their own cases.
		logger.Info("unknown phase, restarting at Pending", "phase", phase)
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
	}

	return ctrl.Result{}, r.persist(ctx, &snap, status)
}

// handlePending validates inputs, captures CapturedGuestSpec, kicks off the
// VolumeSnapshot, and transitions to Capturing.
//
// Returns (advanced, requeueAfter, err):
//   - advanced=true means the status now reflects Capturing.
//   - advanced=false means we're still in Pending and the caller should
//     requeue after the returned duration.
func (r *SwiftSnapshotReconciler) handlePending(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftSnapshotStatus,
) (bool, time.Duration, error) {
	// Phase 1: only the csi-volume-snapshot backend is supported.
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonUnsupportedBackend,
			"backend "+string(snap.Spec.Backend.Type)+" is not implemented in Phase 1; use csi-volume-snapshot")
		return true, 0, nil
	}

	// Resolve source SwiftGuest in the same namespace.
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: snap.Namespace}, &guest); err != nil {
		if !isNotFound(err) {
			return false, 0, err
		}
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonGuestNotFound,
			"SwiftGuest "+snap.Spec.GuestRef.Name+" not found in namespace "+snap.Namespace)
		// Source guest may appear later — requeue rather than fail.
		return false, 10 * time.Second, nil
	}

	// Locate the per-guest root-disk clone PVC. The shared SwiftImage PVC
	// is read-only across guests; snapshotting it would be incorrect.
	pvc, err := r.guestRootPVC(ctx, snap.Namespace, guest.Name)
	if err != nil {
		return false, 0, err
	}
	if pvc == nil {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRootPVCNotFound,
			"per-guest root-disk PVC not yet created; SwiftGuest may still be provisioning")
		return false, 5 * time.Second, nil
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		setPhase(status, snapshotv1alpha1.SwiftSnapshotPhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRootPVCNotFound,
			"per-guest root-disk PVC "+pvc.Name+" not yet Bound (phase="+string(pvc.Status.Phase)+")")
		return false, 5 * time.Second, nil
	}

	// Capture spec metadata before kicking off the VolumeSnapshot — these
	// are needed by SwiftRestore to validate compatibility.
	status.GuestSpec = capturedGuestSpec(&guest)
	if guest.Status.Runtime != nil {
		status.Hypervisor = guest.Status.Runtime.Hypervisor
	}

	// Phase 1 captures only the root disk. Data disks are out of scope.
	setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseCapturing)
	setReadyCondition(status, metav1.ConditionFalse, ReasonCapturing, "creating VolumeSnapshot of root disk")
	return true, 0, nil
}

// handleCapturing creates the VolumeSnapshot if needed and polls readiness.
// Returns (ready, errMsg, err):
//   - errMsg non-empty -> caller transitions to Failed.
//   - ready=true -> caller transitions to Ready (and this function has
//     populated status.Disks/CapturedAt/TotalSizeBytes).
//   - ready=false, errMsg="" -> caller requeues.
func (r *SwiftSnapshotReconciler) handleCapturing(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftSnapshotStatus,
) (bool, string, error) {
	pvc, err := r.guestRootPVC(ctx, snap.Namespace, snap.Spec.GuestRef.Name)
	if err != nil {
		return false, "", err
	}
	if pvc == nil {
		// PVC vanished mid-capture — fail the snapshot rather than spin.
		return false, "per-guest root-disk PVC " + rootPVCName(snap.Spec.GuestRef.Name) + " disappeared during snapshot", nil
	}

	ready, restoreSize, errMsg, err := r.ensureVolumeSnapshot(ctx, snap, pvc.Name)
	if err != nil {
		return false, "", err
	}
	if errMsg != "" {
		return false, errMsg, nil
	}
	if !ready {
		return false, "", nil
	}

	// VolumeSnapshot is readyToUse — populate status.disks and flip to Ready.
	now := metav1.Now()
	status.CapturedAt = &now
	status.Disks = []snapshotv1alpha1.SnapshotDiskRef{{
		Role:      "root",
		SizeBytes: restoreSize,
		Handle:    snap.Namespace + "/" + VolumeSnapshotName(snap.Name),
	}}
	status.TotalSizeBytes = restoreSize
	setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseReady)
	setReadyCondition(status, metav1.ConditionTrue, ReasonSnapshotReady, "VolumeSnapshot is readyToUse")
	return true, "", nil
}

// capturedGuestSpec freezes the SwiftGuest spec fields SwiftRestore needs.
func capturedGuestSpec(guest *swiftv1alpha1.SwiftGuest) *snapshotv1alpha1.CapturedGuestSpec {
	out := &snapshotv1alpha1.CapturedGuestSpec{}
	if guest.Spec.ImageRef != nil {
		out.ImageName = guest.Spec.ImageRef.Name
	}
	return out
}

// persist writes status changes back to the API server.
func (r *SwiftSnapshotReconciler) persist(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) error {
	snap.Status = *status
	return r.Status().Update(ctx, snap)
}

// isNotFound is a small wrapper so this package doesn't grow extra imports
// for one call site.
func isNotFound(err error) bool {
	return client.IgnoreNotFound(err) == nil
}

// SetupWithManager registers the reconciler.
//
// Owns(VolumeSnapshot) ensures readyToUse transitions trigger an immediate
// reconcile rather than waiting for a periodic resync. The controller does
// not own SwiftGuest (the source is observed read-only).
func (r *SwiftSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.SwiftSnapshot{}).
		Owns(&volumesnapshotv1.VolumeSnapshot{}).
		Complete(r)
}
