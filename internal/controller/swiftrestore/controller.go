// Package swiftrestore reconciles SwiftRestore resources.
//
// Phase 1 supports the csi-volume-snapshot backend only. The state machine
// is Pending -> Restoring -> Resuming -> Ready (or Failed). The controller
// pre-creates the target SwiftGuest's per-guest root-disk PVC sourced from
// the SwiftSnapshot's VolumeSnapshot, then creates the SwiftGuest with a
// copy of the source guest's spec. The SwiftGuest controller's
// EnsureRootDiskClone path treats the restore-seeded PVC as authoritative
// (no Copy Job, no expand-and-wait).
//
// Resuming waits for the target SwiftGuest's GuestRunning=True condition
// when ResumeAfterRestore=true. When ResumeAfterRestore=false the target
// runPolicy is forced to Stopped and SwiftRestore goes straight to Ready.
package swiftrestore

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// SwiftRestoreReconciler reconciles SwiftRestore resources.
type SwiftRestoreReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// CurrentHypervisorVersion is the cluster's current CH version
	// string (e.g. "v51.1"), set at controller startup. Used by the
	// Tier B restore path's version check (architect risk #3).
	// Empty string disables the check — controller surfaces a Warning
	// rather than blocking.
	CurrentHypervisorVersion string

	// SnapshotS3Image is the snapshot-s3 image used by the s3 (Tier C)
	// backend's download Job. Wired from KUBESWIFT_SNAPSHOT_S3_IMAGE.
	SnapshotS3Image string
	// SnapshotORASImage is the snapshot-oras image used by the oci backend's
	// download Job. Wired from KUBESWIFT_SNAPSHOT_ORAS_IMAGE.
	SnapshotORASImage string
}

// Reconcile drives the SwiftRestore state machine.
func (r *SwiftRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var restore snapshotv1alpha1.SwiftRestore
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if restore.Status.Phase == snapshotv1alpha1.SwiftRestorePhaseReady ||
		restore.Status.Phase == snapshotv1alpha1.SwiftRestorePhaseFailed {
		return ctrl.Result{}, nil
	}

	phase := restore.Status.Phase
	if phase == "" {
		phase = snapshotv1alpha1.SwiftRestorePhasePending
	}
	status := restore.Status.DeepCopy()

	if status.StartedAt == nil {
		now := metav1.Now()
		status.StartedAt = &now
	}

	switch phase {
	case snapshotv1alpha1.SwiftRestorePhasePending:
		advanced, requeue, err := r.handlePending(ctx, &restore, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !advanced {
			if updateErr := r.persist(ctx, &restore, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}

	case snapshotv1alpha1.SwiftRestorePhaseDownloading:
		// s3 / oci backends: watch the download Job pull artifacts into the
		// node-local cache, then hand off to the shared Tier B restore tail
		// (materializeRestoreTarget) and advance to Restoring.
		handleDl := r.handleDownloading
		if r.downloadBackendIsOCI(ctx, &restore) {
			handleDl = r.handleDownloadingOCI
		}
		advanced, requeue, errMsg, err := handleDl(ctx, &restore, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if errMsg != "" {
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed, errMsg)
		} else if !advanced {
			if updateErr := r.persist(ctx, &restore, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}

	case snapshotv1alpha1.SwiftRestorePhaseRestoring:
		// Resolve the snapshot to dispatch by backend.
		isTierB, dispatchErr := r.snapshotIsTierB(ctx, restore.Namespace, restore.Spec.SnapshotRef.Name)
		if dispatchErr != nil {
			return ctrl.Result{}, dispatchErr
		}
		if isTierB {
			advanced, requeue, errMsg, err := r.handleRestoringLocal(ctx, &restore, status)
			if err != nil {
				return ctrl.Result{}, err
			}
			if errMsg != "" {
				setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
				setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed, errMsg)
			} else if !advanced {
				if updateErr := r.persist(ctx, &restore, status); updateErr != nil {
					return ctrl.Result{}, updateErr
				}
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
			break
		}
		advanced, requeue, errMsg, err := r.handleRestoring(ctx, &restore, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if errMsg != "" {
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed, errMsg)
		} else if !advanced {
			if updateErr := r.persist(ctx, &restore, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}

	case snapshotv1alpha1.SwiftRestorePhaseResuming:
		isTierB, dispatchErr := r.snapshotIsTierB(ctx, restore.Namespace, restore.Spec.SnapshotRef.Name)
		if dispatchErr != nil {
			return ctrl.Result{}, dispatchErr
		}
		if isTierB {
			advanced, requeue, err := r.handleResumingLocal(ctx, &restore, status)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !advanced {
				if updateErr := r.persist(ctx, &restore, status); updateErr != nil {
					return ctrl.Result{}, updateErr
				}
				return ctrl.Result{RequeueAfter: requeue}, nil
			}
			break
		}
		advanced, requeue, err := r.handleResuming(ctx, &restore, status)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !advanced {
			if updateErr := r.persist(ctx, &restore, status); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: requeue}, nil
		}

	default:
		logger.Info("unknown phase, restarting at Pending", "phase", phase)
		setPhase(status, snapshotv1alpha1.SwiftRestorePhasePending)
	}

	return ctrl.Result{}, r.persist(ctx, &restore, status)
}

// handlePending validates the snapshot and the target name, then advances.
func (r *SwiftRestoreReconciler) handlePending(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	// Resolve SwiftSnapshot in same namespace.
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.SnapshotRef.Name, Namespace: restore.Namespace}, &snap); err != nil {
		if !isNotFound(err) {
			return false, 0, err
		}
		setPhase(status, snapshotv1alpha1.SwiftRestorePhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonSnapshotNotFound,
			"SwiftSnapshot "+restore.Spec.SnapshotRef.Name+" not found in namespace "+restore.Namespace)
		return false, 10 * time.Second, nil
	}

	// Snapshot must be Ready before we can restore from it.
	if snap.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhasePending)
		setReadyCondition(status, metav1.ConditionFalse, ReasonSnapshotNotReady,
			"SwiftSnapshot "+snap.Name+" is in phase "+string(snap.Status.Phase)+"; restore requires Ready")
		return false, 10 * time.Second, nil
	}

	// Backend dispatch:
	//   csi-volume-snapshot (Phase 1): pre-create restore PVC + target SwiftGuest.
	//   local              (Phase 2): version check + Tier B handler. The
	//     actual restore-launcher-pod creation is wired in commit 12 along
	//     with config.json patching for identity regen — splitting them
	//     would require two passes over the snapshot's config.json.
	//   s3                 (Phase 3): download from object storage to a
	//     node-local cache (handlePendingS3 -> Downloading), then hand off to
	//     the same Tier B restore path once the cache is populated.
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot:
		// Continue to existing CSI flow below.
	case snapshotv1alpha1.SnapshotBackendLocal:
		return r.handlePendingLocal(ctx, restore, &snap, status)
	case snapshotv1alpha1.SnapshotBackendS3:
		return r.handlePendingS3(ctx, restore, &snap, status)
	case snapshotv1alpha1.SnapshotBackendOCI:
		return r.handlePendingOCI(ctx, restore, &snap, status)
	default:
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"backend "+string(snap.Spec.Backend.Type)+" is not implemented")
		return true, 0, nil
	}

	// Target SwiftGuest conflict check.
	var existingTarget swiftv1alpha1.SwiftGuest
	getErr := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &existingTarget)
	if getErr == nil && !restore.Spec.TargetGuest.OverwriteExisting {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonTargetConflict,
			"SwiftGuest "+restore.Spec.TargetGuest.Name+" already exists; set targetGuest.overwriteExisting=true to replace")
		return true, 0, nil
	}
	if getErr != nil && !isNotFound(getErr) {
		return false, 0, getErr
	}

	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseRestoring)
	setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring, "creating restore PVC and target SwiftGuest")
	return true, 0, nil
}

// handleRestoring creates the per-guest PVC and the target SwiftGuest, then
// advances. Idempotent: a re-run is a no-op once the resources exist.
func (r *SwiftRestoreReconciler) handleRestoring(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, string, error) {
	// Re-resolve snapshot (caller already verified it's Ready).
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.SnapshotRef.Name, Namespace: restore.Namespace}, &snap); err != nil {
		if isNotFound(err) {
			return false, 0, fmt.Sprintf("SwiftSnapshot %s disappeared mid-restore", restore.Spec.SnapshotRef.Name), nil
		}
		return false, 0, "", err
	}

	rootDisk := findRootDisk(&snap.Status)
	if rootDisk == nil {
		return false, 0, "SwiftSnapshot status has no root disk; nothing to restore", nil
	}
	vsNS, vsName, ok := SnapshotHandle(rootDisk.Handle)
	if !ok {
		return false, 0, "SwiftSnapshot disk handle is malformed: " + rootDisk.Handle, nil
	}
	if vsNS != restore.Namespace {
		// Defensive — same-namespace constraint should be enforced by the
		// validation webhook.
		return false, 0, "SwiftSnapshot VolumeSnapshot is in a different namespace: " + vsNS, nil
	}

	// Resolve source SwiftGuest for spec copy + StorageClassName.
	var source swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: restore.Namespace}, &source); err != nil {
		if isNotFound(err) {
			setReadyCondition(status, metav1.ConditionFalse, ReasonSourceGuestGone,
				"source SwiftGuest "+snap.Spec.GuestRef.Name+" no longer exists; cannot copy spec")
			return false, 0, "source SwiftGuest " + snap.Spec.GuestRef.Name + " gone — restore needs source spec", nil
		}
		return false, 0, "", err
	}

	storageClass, err := r.sourceStorageClass(ctx, restore.Namespace, source.Name)
	if err != nil {
		return false, 0, "", err
	}

	// Create the per-guest restore PVC.
	pvcName := rootPVCName(restore.Spec.TargetGuest.Name)
	if err := r.ensureRestorePVC(ctx, restore, pvcName, vsName, storageClass, rootDisk.SizeBytes); err != nil {
		return false, 0, "", err
	}

	// Wait for Bound before creating the target SwiftGuest. Otherwise the
	// guest controller's EnsureRootDiskClone races on an unbound PVC.
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: restore.Namespace}, &pvc); err != nil {
		return false, 0, "", err
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoring,
			"restore PVC "+pvcName+" not yet Bound (phase="+string(pvc.Status.Phase)+")")
		return false, 5 * time.Second, "", nil
	}

	target, err := r.ensureTargetGuest(ctx, restore, &source)
	if err != nil {
		return false, 0, "", err
	}
	status.GuestRef = &snapshotv1alpha1.SwiftRestoreGuestRef{Name: target.Name}

	if !restore.Spec.ResumeAfterRestore {
		// Caller asked not to resume — go straight to Ready.
		now := metav1.Now()
		status.CompletedAt = &now
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseReady)
		setReadyCondition(status, metav1.ConditionTrue, ReasonRestoreReady,
			"restore complete; target SwiftGuest is Stopped per resumeAfterRestore=false")
		return true, 0, "", nil
	}

	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseResuming)
	setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
		"target SwiftGuest "+target.Name+" created; waiting for GuestRunning=True")
	return true, 0, "", nil
}

// handleResuming polls the target SwiftGuest for GuestRunning=True.
func (r *SwiftRestoreReconciler) handleResuming(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	var target swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &target); err != nil {
		if isNotFound(err) {
			// Target deleted out from under us — fail the restore.
			setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
			setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
				"target SwiftGuest "+restore.Spec.TargetGuest.Name+" was deleted during Resuming")
			return true, 0, nil
		}
		return false, 0, err
	}

	if isGuestRunning(&target) {
		now := metav1.Now()
		status.CompletedAt = &now
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseReady)
		setReadyCondition(status, metav1.ConditionTrue, ReasonRestoreReady,
			"restore complete; target SwiftGuest "+target.Name+" is Running")
		return true, 0, nil
	}

	return false, 5 * time.Second, nil
}

// isGuestRunning returns true when the target SwiftGuest has GuestRunning=True.
// Mirrors the literal used by the SwiftGuest controller and swiftletd.
const conditionGuestRunning = "GuestRunning"

func isGuestRunning(guest *swiftv1alpha1.SwiftGuest) bool {
	for _, c := range guest.Status.Conditions {
		if c.Type == conditionGuestRunning && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *SwiftRestoreReconciler) persist(ctx context.Context, restore *snapshotv1alpha1.SwiftRestore, status *snapshotv1alpha1.SwiftRestoreStatus) error {
	// Fire the Phase 5 metric once, on the non-terminal -> terminal transition.
	freshTerminal := isRestoreTerminal(status.Phase) && !isRestoreTerminal(restore.Status.Phase)
	restore.Status = *status
	if err := r.Status().Update(ctx, restore); err != nil {
		return err
	}
	if freshTerminal {
		recordRestoreTerminal(restore)
	}
	return nil
}

// isRestoreTerminal reports whether a SwiftRestore phase is terminal.
func isRestoreTerminal(p snapshotv1alpha1.SwiftRestorePhase) bool {
	return p == snapshotv1alpha1.SwiftRestorePhaseReady || p == snapshotv1alpha1.SwiftRestorePhaseFailed
}

// recordRestoreTerminal emits the Phase 5 restore metrics on the
// non-terminal -> terminal transition: a per-result counter plus the
// start->complete latency for successful restores.
func recordRestoreTerminal(restore *snapshotv1alpha1.SwiftRestore) {
	st := &restore.Status
	var result string
	switch st.Phase {
	case snapshotv1alpha1.SwiftRestorePhaseReady:
		result = "ready"
	case snapshotv1alpha1.SwiftRestorePhaseFailed:
		result = "failed"
	default:
		return
	}
	metrics.RestoreTotal.WithLabelValues(result).Inc()
	if result == "ready" && st.StartedAt != nil && st.CompletedAt != nil {
		if d := st.CompletedAt.Sub(st.StartedAt.Time); d > 0 {
			metrics.RestoreSeconds.Observe(d.Seconds())
		}
	}
}

func isNotFound(err error) bool {
	return client.IgnoreNotFound(err) == nil
}

// snapshotIsTierB resolves the SwiftSnapshot referenced by a
// SwiftRestore and returns true when its backend is local. Used by
// the Restoring/Resuming phase dispatch to route Tier B restores
// through their dedicated handlers.
//
// A NotFound on the snapshot is reported as (false, nil) — the
// caller treats this as "fall through to CSI handler" which then
// fails fast with a meaningful error. We don't surface the error
// here because the snapshot must already have been Ready when the
// restore was advanced past Pending; a mid-flight delete is best
// described by the per-handler error path.
func (r *SwiftRestoreReconciler) snapshotIsTierB(ctx context.Context, namespace, name string) (bool, error) {
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &snap); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return IsTierBRestore(&snap), nil
}

// SetupWithManager registers the reconciler. Owns the target SwiftGuest so
// state changes (GuestRunning condition transitions) trigger immediate
// reconciliation in the Resuming phase.
func (r *SwiftRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.SwiftRestore{}).
		Owns(&swiftv1alpha1.SwiftGuest{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
