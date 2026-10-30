package swiftmigration

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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

// Cancel paths for kubeswift_migration_cancel_total (plus cancelPathNone).
const (
	cancelPathSource      = "source"
	cancelPathDestination = "destination"

	// cancelOutcomeFallback counts a source cancel handed to the destination
	// cancel; the other outcomes are abortOutcome's.
	cancelOutcomeFallback = "fallback"
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

// resolveOnSource is resolveSourceOwnership's source step (see the file
// comment). fallback=true hands the abort to the destination cancel: the
// source could not cancel, or did not report within sourceCancelTimeout.
func (r *SwiftMigrationReconciler) resolveOnSource(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	src, dst *corev1.Pod,
	trigger abortTrigger,
) (res abortResolution, fallback bool, err error) {
	logger := log.FromContext(ctx).WithValues("migration", mig.Name, "trigger", trigger)
	cid := cancelSendID(mig)
	pending := abortResolution{outcome: abortPending, path: cancelPathSource,
		requeue: ctrl.Result{RequeueAfter: cancelPollInterval}}

	// An answered cancel-send stays this path's even once its action is
	// cleared: a pass from a stale copy of the migration finishes it again,
	// idempotently, instead of starting the destination cancel.
	if !sourceCancelIssued(mig, src) && src.Annotations[AnnotationMigrationCancelResultID] != cid {
		patch := client.MergeFrom(src.DeepCopy())
		if src.Annotations == nil {
			src.Annotations = map[string]string{}
		}
		// Replaces this migration's send action, if swiftletd has not taken
		// it up yet: it then never starts (the job clearSourceSend does on
		// the destination path).
		src.Annotations[AnnotationMigrationAction] = MigrationActionCancelSend
		src.Annotations[AnnotationMigrationActionID] = cid
		delete(src.Annotations, AnnotationMigrationActionArgs)
		src.Annotations[AnnotationMigrationCancelIssuedAt] = time.Now().UTC().Format(time.RFC3339)
		if perr := r.Patch(ctx, src, patch); perr != nil {
			return abortResolution{}, false, fmt.Errorf("write cancel-send on source pod %q: %w", src.Name, perr)
		}
		logger.Info("migration cancel issued", "path", cancelPathSource, "pod", src.Name)
		_ = r.persistPhaseDetail(ctx, mig, phaseDetailCancelSourceIssuing)
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "CancelIssued",
				"%s: asked the source launcher %q to cancel the transfer (id=%s); the source guest keeps running",
				triggerText(trigger), src.Name, cid)
		}
		return pending, false, nil
	}

	sid := sendActionID(mig)
	status := src.Annotations[AnnotationMigrationStatus]
	statusID := src.Annotations[AnnotationMigrationStatusID]
	answered := src.Annotations[AnnotationMigrationCancelResultID] == cid
	cancelResult := src.Annotations[AnnotationMigrationCancelResult]
	sending := status == migrationStatusSending && statusID == sid
	sendEnded := statusID == sid && (status == MigrationStatusFailed || status == MigrationStatusRejected)

	switch {
	case srcReportedComplete(mig, src) || dstReportedRunning(mig, dst):
		// The transfer finished before the cancel took.
		logger.Info("migration cancel too late", "path", cancelPathSource)
		return abortResolution{outcome: abortCommitted, path: cancelPathSource,
			detail: "the transfer completed before the cancel took effect"}, false, nil
	case sendEnded:
		return abortResolution{outcome: abortSourceRetained, path: cancelPathSource,
			detail: strings.TrimPrefix(normalizeStatusDetail(src.Annotations[AnnotationMigrationStatusDtl]), "cancelled: ")}, false, nil
	case answered && cancelResult == cancelResultFailed:
		return r.fallBackToDestinationCancel(ctx, mig, src, trigger,
			fmt.Sprintf("the source could not cancel: %s", src.Annotations[AnnotationMigrationCancelResultDetail]))
	case answered && cancelResult == cancelResultNotActive && !sending:
		return abortResolution{outcome: abortSourceRetained, path: cancelPathSource,
			detail: "cancelled before the source sent anything"}, false, nil
	}

	issuedAt, perr := time.Parse(time.RFC3339, src.Annotations[AnnotationMigrationCancelIssuedAt])
	if perr != nil {
		// Written with the action; missing only if edited away. Restart the
		// budget rather than fall back at once.
		patch := client.MergeFrom(src.DeepCopy())
		src.Annotations[AnnotationMigrationCancelIssuedAt] = time.Now().UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, src, patch); err != nil {
			return abortResolution{}, false, fmt.Errorf("stamp cancel time on source pod %q: %w", src.Name, err)
		}
		return pending, false, nil
	}
	if time.Since(issuedAt) > sourceCancelTimeout {
		return r.fallBackToDestinationCancel(ctx, mig, src, trigger,
			fmt.Sprintf("the source did not report within %s", sourceCancelTimeout))
	}
	_ = r.persistPhaseDetail(ctx, mig, phaseDetailCancelSourceWaiting)
	return pending, false, nil
}

const (
	phaseDetailCancelSourceIssuing = "issuing cancel on source"
	phaseDetailCancelSourceWaiting = "waiting for the source to report the cancelled transfer"
)

// fallBackToDestinationCancel hands the abort to the destination cancel. The
// cancel-send is recorded as failed and taken off the source in one patch,
// so it is neither issued again nor counted twice.
func (r *SwiftMigrationReconciler) fallBackToDestinationCancel(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	src *corev1.Pod,
	trigger abortTrigger,
	reason string,
) (abortResolution, bool, error) {
	log.FromContext(ctx).Info("migration cancel falling back", "path", cancelPathDestination,
		"reason", reason, "migration", mig.Name, "trigger", trigger)
	patch := client.MergeFrom(src.DeepCopy())
	cid := cancelSendID(mig)
	if src.Annotations[AnnotationMigrationCancelResultID] != cid {
		src.Annotations[AnnotationMigrationCancelResultID] = cid
		src.Annotations[AnnotationMigrationCancelResult] = cancelResultFailed
		src.Annotations[AnnotationMigrationCancelResultDetail] = reason
	}
	if sourceCancelIssued(mig, src) {
		delete(src.Annotations, AnnotationMigrationAction)
		delete(src.Annotations, AnnotationMigrationActionID)
		delete(src.Annotations, AnnotationMigrationActionArgs)
	}
	if err := r.Patch(ctx, src, patch); client.IgnoreNotFound(err) != nil {
		return abortResolution{}, false, fmt.Errorf("record cancel fallback on source pod %q: %w", src.Name, err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(mig, corev1.EventTypeWarning, "CancelFallback",
			"%s; stopping the transfer at the destination instead", reason)
	}
	metrics.MigrationCancelTotal.WithLabelValues(string(trigger), cancelPathSource, cancelOutcomeFallback).Inc()
	return abortResolution{}, true, nil
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
