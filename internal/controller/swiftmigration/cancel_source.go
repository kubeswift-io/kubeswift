package swiftmigration

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// Cancelling a live migration on its source (Cloud Hypervisor >= v54).
//
// The destination-kill cancel (transitionCancelLive) stops a transfer by
// SIGKILLing the receiving Cloud Hypervisor, and refuses once that VM runs.
// Between the source finishing its send and the destination reporting its VM
// running there is a window -- the destination acknowledges the last of the
// state, the source exits, and only then does the destination restore and
// resume the guest -- in which the refusal cannot see that the destination is
// about to be the only copy. A kill there leaves no guest.
//
// Cloud Hypervisor v54's vm.cancel-migration closes that window: the source
// itself decides, atomically, whether the migration is cancelled (the source
// guest runs on, resumed if it was paused) or had already gone past its last
// check (it finishes, and the destination takes the guest). The source's send
// then reports which, and the controller acts only on that report:
//
//	cancel-send written on the source (action, persisted with its issue time)
//	   |
//	   +-- source send reports complete  -> committed: CancelIgnored, the
//	   |                                    migration completes (honorCancel)
//	   +-- source send reports failed    -> the source guest is running and is
//	   |                                    the guest: delete the destination,
//	   |                                    Cancelled
//	   +-- no send in progress on the source (cancel-result not-active, no
//	   |   `sending` for this migration's send) -> as failed
//	   +-- the cancel could not be delivered (cancel-result failed), or no
//	       answer within sourceCancelTimeout -> the destination-kill cancel,
//	       as for a launcher without the feature
//
// Until one of those, nothing is deleted: the source stays the guest. Every
// input is a pod annotation, so a controller restart resumes where it was.
//
// Only a source launcher that advertises migration-cancel is cancelled this
// way. Older launchers (Cloud Hypervisor v53, or swiftletd before this) keep
// the destination-kill cancel, and a destination always runs the source's
// launcher image, so both ends of a migration agree.

const (
	// AnnotationLauncherFeatures is what a launcher supports, space-separated
	// (swiftletd report::launcher_features).
	AnnotationLauncherFeatures = "kubeswift.io/launcher-features"
	// LauncherFeatureMigrationCancel: the launcher accepts cancel-send and
	// its Cloud Hypervisor has vm.cancel-migration.
	LauncherFeatureMigrationCancel = "migration-cancel"

	// MigrationActionCancelSend cancels the source's send in Cloud
	// Hypervisor.
	MigrationActionCancelSend = "cancel-send"

	// The cancel-send's own result (swiftletd action::write_cancel_result):
	// requested | not-active | failed. Separate from migration-status, which
	// stays the send's.
	AnnotationMigrationCancelResult       = "kubeswift.io/migration-cancel-result"
	AnnotationMigrationCancelResultID     = "kubeswift.io/migration-cancel-result-id"
	AnnotationMigrationCancelResultDetail = "kubeswift.io/migration-cancel-result-detail"

	cancelResultNotActive = "not-active"
	cancelResultFailed    = "failed"
)

// sourceCancelTimeout bounds the wait for the source's send to report after
// a cancel-send. Cloud Hypervisor checks the cancel between memory
// iterations and resumes the source at once, and swiftletd reports from the
// migration event, so this is seconds in practice; the bound is for a
// launcher that stopped answering.
const sourceCancelTimeout = 60 * time.Second

// Cancel paths and outcomes for kubeswift_migration_cancel_total.
const (
	cancelPathSource      = "source"
	cancelPathDestination = "destination"

	cancelOutcomeCancelled = "cancelled"
	cancelOutcomeIgnored   = "ignored"
	cancelOutcomeFallback  = "fallback"
)

// cancelSendID is the source cancel's action id, one per migration.
func cancelSendID(mig *migrationv1alpha1.SwiftMigration) string {
	return fmt.Sprintf("%s:cancel-send:0", mig.Name)
}

// launcherHasFeature reports whether a launcher pod advertises a feature.
func launcherHasFeature(pod *corev1.Pod, feature string) bool {
	for _, f := range strings.Fields(pod.Annotations[AnnotationLauncherFeatures]) {
		if f == feature {
			return true
		}
	}
	return false
}

// sourceCancelIssued reports whether this migration's cancel-send is on the
// source pod.
func sourceCancelIssued(mig *migrationv1alpha1.SwiftMigration, src *corev1.Pod) bool {
	return src != nil &&
		src.Annotations[AnnotationMigrationAction] == MigrationActionCancelSend &&
		src.Annotations[AnnotationMigrationActionID] == cancelSendID(mig)
}

// cancelOnSource drives a pre-commit cancel through the source when its
// launcher can (see the file comment). handled=false hands the cancel to the
// destination-kill path: the source cannot cancel, or its cancel failed or
// went unanswered.
func (r *SwiftMigrationReconciler) cancelOnSource(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	guest *swiftv1alpha1.SwiftGuest,
) (handled bool, result ctrl.Result, err error) {
	logger := log.FromContext(ctx).WithValues("migration", mig.Name)
	var src corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, guest), Namespace: mig.Namespace}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, fmt.Errorf("get source pod: %w", err)
	}
	dstName, err := dstPodName(mig, guest.Name)
	if err != nil {
		return false, ctrl.Result{}, nil
	}
	var dst corev1.Pod
	dstErr := r.Get(ctx, client.ObjectKey{Name: dstName, Namespace: mig.Namespace}, &dst)
	if dstErr != nil && !apierrors.IsNotFound(dstErr) {
		return true, ctrl.Result{}, fmt.Errorf("get destination pod: %w", dstErr)
	}
	dstExists := dstErr == nil

	// Once the destination-kill cancel has started, it finishes the cancel
	// (it was the fallback, or this migration predates the source cancel).
	if dstExists && dst.Annotations[AnnotationMigrationAction] == MigrationActionCancel &&
		dst.Annotations[AnnotationMigrationActionID] == cancelID(mig) {
		return false, ctrl.Result{}, nil
	}
	cid := cancelSendID(mig)
	if !sourceCancelIssued(mig, &src) {
		// A cancel-send already answered and taken off the source (the
		// fallback clears the source's actions): do not issue it again.
		if src.Annotations[AnnotationMigrationCancelResultID] == cid {
			return false, ctrl.Result{}, nil
		}
		if !launcherHasFeature(&src, LauncherFeatureMigrationCancel) || src.DeletionTimestamp != nil {
			return false, ctrl.Result{}, nil
		}
		patch := client.MergeFrom(src.DeepCopy())
		if src.Annotations == nil {
			src.Annotations = map[string]string{}
		}
		// Replaces this migration's send action, if swiftletd has not taken
		// it up yet: it then never starts (the job clearSourceSend does on
		// the destination-kill path).
		src.Annotations[AnnotationMigrationAction] = MigrationActionCancelSend
		src.Annotations[AnnotationMigrationActionID] = cid
		delete(src.Annotations, AnnotationMigrationActionArgs)
		src.Annotations[AnnotationMigrationCancelIssuedAt] = time.Now().UTC().Format(time.RFC3339)
		if perr := r.Patch(ctx, &src, patch); perr != nil {
			return true, ctrl.Result{}, fmt.Errorf("write cancel-send on source pod %q: %w", src.Name, perr)
		}
		logger.Info("migration cancel issued", "path", cancelPathSource, "pod", src.Name, "via", "source-cancel")
		_ = r.persistPhaseDetail(ctx, mig, phaseDetailCancelSourceIssuing)
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "CancelIssued",
				"asked the source launcher %q to cancel the transfer (id=%s); the source guest keeps running",
				src.Name, cid)
		}
		return true, ctrl.Result{RequeueAfter: cancelPollInterval}, nil
	}

	sid := sendActionID(mig)
	status := src.Annotations[AnnotationMigrationStatus]
	statusID := src.Annotations[AnnotationMigrationStatusID]
	resultID := src.Annotations[AnnotationMigrationCancelResultID]
	cancelResult := src.Annotations[AnnotationMigrationCancelResult]

	sending := status == migrationStatusSending && statusID == sid
	sendEnded := statusID == sid && (status == MigrationStatusFailed || status == MigrationStatusRejected)
	answered := resultID == cid

	switch {
	case srcReportedComplete(mig, &src):
		// The send finished before the cancel took: committed. honorCancel
		// sees it on the next pass and lets the migration complete.
		logger.Info("migration cancel too late", "path", cancelPathSource, "evidence", "source-status-complete")
		return true, ctrl.Result{Requeue: true}, nil
	case sendEnded:
		return r.finishSourceCancel(ctx, mig, &src, &dst, dstExists,
			fmt.Sprintf("cancelled on the source: %s", normalizeStatusDetail(src.Annotations[AnnotationMigrationStatusDtl])))
	case answered && cancelResult == cancelResultFailed:
		return r.fallBackToDestinationCancel(ctx, mig, &src,
			fmt.Sprintf("the source could not cancel: %s", src.Annotations[AnnotationMigrationCancelResultDetail]))
	case answered && cancelResult == cancelResultNotActive && !sending:
		return r.finishSourceCancel(ctx, mig, &src, &dst, dstExists,
			"cancelled before the source sent anything")
	}

	issuedAt, perr := time.Parse(time.RFC3339, src.Annotations[AnnotationMigrationCancelIssuedAt])
	if perr != nil {
		// Written with the action; missing only if edited away. Restart the
		// budget rather than fall back at once.
		patch := client.MergeFrom(src.DeepCopy())
		src.Annotations[AnnotationMigrationCancelIssuedAt] = time.Now().UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, &src, patch); err != nil {
			return true, ctrl.Result{}, fmt.Errorf("stamp cancel time on source pod %q: %w", src.Name, err)
		}
		return true, ctrl.Result{RequeueAfter: cancelPollInterval}, nil
	}
	if time.Since(issuedAt) > sourceCancelTimeout {
		return r.fallBackToDestinationCancel(ctx, mig, &src,
			fmt.Sprintf("the source did not report within %s", sourceCancelTimeout))
	}
	_ = r.persistPhaseDetail(ctx, mig, phaseDetailCancelSourceWaiting)
	return true, ctrl.Result{RequeueAfter: cancelPollInterval}, nil
}

const (
	phaseDetailCancelSourceIssuing = "issuing cancel on source"
	phaseDetailCancelSourceWaiting = "waiting for the source to report the cancelled transfer"
)

// finishSourceCancel ends a cancel the source has settled: its guest runs on
// it. The destination is deleted -- unless it reports the guest running, in
// which case the migration committed and honorCancel completes it.
func (r *SwiftMigrationReconciler) finishSourceCancel(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	src, dst *corev1.Pod,
	dstExists bool,
	detail string,
) (bool, ctrl.Result, error) {
	if dstExists && dstReportedRunning(mig, dst) {
		log.FromContext(ctx).Info("migration cancel too late", "path", cancelPathSource,
			"evidence", "destination-status-running", "migration", mig.Name)
		return true, ctrl.Result{Requeue: true}, nil
	}
	_ = r.persistPhaseDetail(ctx, mig, phaseDetailCancelDeleting)
	if dstExists {
		if err := r.Delete(ctx, dst); err != nil && !apierrors.IsNotFound(err) {
			return true, ctrl.Result{}, fmt.Errorf("delete destination pod %q: %w", dst.Name, err)
		}
	}
	if err := r.clearSourceCancel(ctx, mig, src); err != nil {
		return true, ctrl.Result{}, err
	}
	log.FromContext(ctx).Info("migration cancelled", "path", cancelPathSource,
		"evidence", "source-status", "migration", mig.Name)
	res, err := r.finalizeCancelled(ctx, mig, detail+"; destination pod deleted")
	if err == nil && !res.Requeue {
		metrics.MigrationCancelTotal.WithLabelValues(cancelPathSource, cancelOutcomeCancelled).Inc()
	}
	return true, res, err
}

// fallBackToDestinationCancel hands the cancel to the destination-kill path.
// The cancel-send stays answered on the source, so it is not issued again.
func (r *SwiftMigrationReconciler) fallBackToDestinationCancel(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	src *corev1.Pod,
	reason string,
) (bool, ctrl.Result, error) {
	log.FromContext(ctx).Info("migration cancel falling back", "path", cancelPathDestination,
		"reason", reason, "migration", mig.Name)
	if r.Recorder != nil {
		r.Recorder.Eventf(mig, corev1.EventTypeWarning, "CancelFallback",
			"%s; stopping the transfer at the destination instead", reason)
	}
	// Answer it for the record if swiftletd never did, so a later pass does
	// not re-issue it after the action is cleared.
	if src.Annotations[AnnotationMigrationCancelResultID] != cancelSendID(mig) {
		patch := client.MergeFrom(src.DeepCopy())
		src.Annotations[AnnotationMigrationCancelResultID] = cancelSendID(mig)
		src.Annotations[AnnotationMigrationCancelResult] = cancelResultFailed
		src.Annotations[AnnotationMigrationCancelResultDetail] = reason
		if err := r.Patch(ctx, src, patch); client.IgnoreNotFound(err) != nil {
			return true, ctrl.Result{}, fmt.Errorf("record cancel fallback on source pod %q: %w", src.Name, err)
		}
	}
	metrics.MigrationCancelTotal.WithLabelValues(cancelPathSource, cancelOutcomeFallback).Inc()
	return false, ctrl.Result{}, nil
}

// clearSourceCancel takes this migration's cancel-send off a launcher, so the
// launcher does not count its migration namespace busy (#823) once the
// cancel is over. A no-op for any other action.
func (r *SwiftMigrationReconciler) clearSourceCancel(ctx context.Context, mig *migrationv1alpha1.SwiftMigration, pod *corev1.Pod) error {
	if pod == nil || !sourceCancelIssued(mig, pod) {
		return nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	delete(pod.Annotations, AnnotationMigrationAction)
	delete(pod.Annotations, AnnotationMigrationActionID)
	delete(pod.Annotations, AnnotationMigrationActionArgs)
	return client.IgnoreNotFound(r.Patch(ctx, pod, patch))
}

// clearLauncherCancel is clearSourceCancel for a terminal migration, on the
// guest's launcher: run on every pass over a terminal migration, so a cancel
// finished by an earlier pass that stopped short of it is still cleared.
func (r *SwiftMigrationReconciler) clearLauncherCancel(ctx context.Context, mig *migrationv1alpha1.SwiftMigration) error {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		return client.IgnoreNotFound(err)
	}
	var src corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, &guest), Namespace: mig.Namespace}, &src); err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.clearSourceCancel(ctx, mig, &src)
}

// cancelPath is the path a cancel took, for the metric: source when this
// migration's cancel-send was issued, else destination.
func (r *SwiftMigrationReconciler) cancelPath(ctx context.Context, mig *migrationv1alpha1.SwiftMigration) string {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		return cancelPathDestination
	}
	var src corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, &guest), Namespace: mig.Namespace}, &src); err != nil {
		return cancelPathDestination
	}
	if sourceCancelIssued(mig, &src) || src.Annotations[AnnotationMigrationCancelResultID] == cancelSendID(mig) {
		return cancelPathSource
	}
	return cancelPathDestination
}
