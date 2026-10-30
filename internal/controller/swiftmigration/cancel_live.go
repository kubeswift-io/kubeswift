package swiftmigration

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// Migration action / status annotation keys read+written by swiftletd
// (rust/swiftletd/src/action.rs::MIGRATION_*_KEY). These are the
// controller's side of the Phase 2 annotation surface.
const (
	AnnotationMigrationAction    = "kubeswift.io/migration-action"
	AnnotationMigrationActionID  = "kubeswift.io/migration-action-id"
	AnnotationMigrationStatus    = "kubeswift.io/migration-status"
	AnnotationMigrationStatusID  = "kubeswift.io/migration-status-id"
	AnnotationMigrationStatusDtl = "kubeswift.io/migration-status-detail"
	// AnnotationMigrationPauseWindowMs is the swiftletd-on-src-reported
	// vCPU-paused window (CH's actual pause-and-send measurement),
	// written alongside `migration-status=complete` at send-complete
	// (rust/swiftletd/src/action.rs::write_migration_status). Read by
	// stopandcopy_live's substateSendComplete handler and stamped into
	// status.ObservedTransferDuration per W27b.
	AnnotationMigrationPauseWindowMs = "kubeswift.io/migration-pause-window-ms"
	// AnnotationMigrationProgressEstimate is the swiftletd-on-src-emitted
	// pre-copy progress estimate (an integer percentage 0-100, a bandwidth
	// heuristic per Phase 3b design §5.4) written on the source pod at ~5s
	// intervals during the live send. The controller reads it during the
	// transferring substate and surfaces it as status.transferProgress
	// (Phase 5). Best-effort and approximate — see the field docstring.
	AnnotationMigrationProgressEstimate = "kubeswift.io/migration-progress-estimate"
	// AnnotationMigrationCancelIssuedAt records, on the destination pod and
	// in the same patch as the cancel action, when the controller issued
	// the cancel (RFC 3339). The cancel-ack budget runs from it.
	AnnotationMigrationCancelIssuedAt = "kubeswift.io/migration-cancel-issued-at"
	MigrationActionCancel             = "cancel"
	MigrationStatusFailed             = "failed"
	MigrationStatusFailedCancelDt     = "cancelled" // expected substring in status-detail

	// MigrationStatusRejected is swiftletd's status for an action it
	// refused to execute (Phase 2 PR-B's decide() rejection path —
	// e.g., missing phase2-ack annotation, namespace mismatch, action-id
	// mismatch). Distinct from "failed" (action ran and errored) per
	// rust/swiftletd/src/action.rs's StatusKind::Rejected vs
	// StatusKind::Failed.
	//
	// Phase 3a's controller treats rejected with matching action-id as
	// terminal: surface as a fast Failed transition with the rejection
	// detail preserved in failureMessage. Without this recognition the
	// migration stalls at substateSendPending/substateRecvPending until
	// spec.timeout (W14, surfaced during PR 1 cluster walkthrough).
	MigrationStatusRejected = "rejected"
)

// cancelAckTimeout is the upper bound for waiting on D1's cancel-ack
// (swiftletd-on-dst writes migration-status=failed after SIGKILL of
// receiver CH; rust/swiftletd/src/action.rs::dispatch_migration_cancel).
// D1's SIGKILL is synchronous and dst-side, and swiftletd dispatches it
// while the receive is still running, so the ack arrives within a poll
// interval or two. (Until swiftletd ran dispatches off its action loop, the
// cancel was seen only after the receive returned, and this timeout was
// the only way a mid-transfer cancel ever took effect.) 30s is the upper
// bound for "swiftletd unreachable or stuck."
//
// swiftletd refuses the cancel once the destination's VM is Running: the
// receive has completed and that VM is the only copy. It writes no status
// then, so this waits out the budget -- but by then the source has
// reported complete, the reconcile sees the commit point, and the cancel
// is ignored before the fallback below can delete the destination.
//
// **W12 inheritance note**: if D1's ack is not observed within 30s
// (e.g., swiftletd unreachable, network partition between controller
// and dst pod, kubelet stuck), the controller force-deletes dst pod
// (grace=0). This force-delete inherits the W12 slow-failure pattern:
// the source CH's vm.send-migration call is synchronous in
// swift-ch-client (Phase 2 plumbing); when dst CH dies, the source
// side's send loop unwinds via TCP-close. Under network partition,
// the source CH may take up to ~127s additional time to unwind via
// the kernel TCP retransmit timeout. Phase 3b's swift-ch-client
// async refactor resolves both W12 and this cancel-fallback
// slow-failure path together.
const cancelAckTimeout = 30 * time.Second

// cancelPollInterval is the requeue cadence while waiting for D1's
// ack. The labeled informer wakes the controller faster on real
// annotation events; the periodic requeue is the safety net.
const cancelPollInterval = 2 * time.Second

// Phase 3a phaseDetail vocabulary additions for cancel handling.
const (
	phaseDetailCancelIssuing  = "issuing cancel on destination"
	phaseDetailCancelWaiting  = "waiting for cancel acknowledgment"
	phaseDetailCancelDeleting = "deleting destination pod"
)

// cancelID returns the deterministic $CANCEL_ID for the given
// SwiftMigration. Per design §2.3 cancel discipline step 3:
//
//	$CANCEL_ID = "<swiftmigration.Name>:cancel:0"
//
// One-shot per SwiftMigration; cancel doesn't follow the action-id-
// pairing idempotency rule for send/recv. The "0" suffix is for
// audit symmetry with $RECV_ID/$SEND_ID, not an attempt counter.
func cancelID(mig *migrationv1alpha1.SwiftMigration) string {
	return fmt.Sprintf("%s:cancel:0", mig.Name)
}

// honorCancel implements the controller-side cancel handler per
// design §5.3. Called from Reconcile between ensureFinalizer and
// the per-phase dispatch.
//
// Returns (true, result, err) when the cancel handler took over the
// reconcile and the caller should return result/err immediately.
// Returns (false, _, _) when the handler did NOT terminate the
// reconcile and the caller should fall through to normal phase
// dispatch (e.g., post-cutover cancel that only sets the
// CancelIgnored condition).
//
// **Live-mode-only gate**: cancel handling fires only when
// status.Mode=="live". Phase 1 offline mode has no cancel-via-spec
// pathway in Phase 3a's scope; offline SwiftMigrations with
// spec.cancelRequested=true are silently ignored (the operator can
// `kubectl delete` to trigger the existing handleCancellation path,
// which works for offline). A future small follow-up may extend
// Phase 3a's cancel handler to offline mode if operator demand
// surfaces.
//
// **Pre-cutover vs post-cutover**:
//   - Pre-cutover (isPostCutover==false): drive to Cancelled via
//     transitionCancelLive (writes cancel annotation on dst pod,
//     waits for D1's ack with 30s budget, deletes dst pod, sets
//     phase=Cancelled with FailureReason=Cancelled).
//   - Post-cutover (isPostCutover==true): set CancelIgnored
//     condition with reason=PastCutover; do NOT change phase or
//     failureReason; return (false, _, _) so phase dispatch
//     continues normally.
//
// **Idempotency**: re-reconcile observes the existing cancel
// annotation on dst pod and skips the write step. Leader-handover
// survives by observing cluster state (annotation existence + ack
// status).
func (r *SwiftMigrationReconciler) honorCancel(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) (handled bool, result ctrl.Result, err error) {
	if !mig.Spec.CancelRequested {
		return false, ctrl.Result{}, nil
	}

	// Live-mode gate. Offline-mode cancel via spec.cancelRequested
	// is not in scope for Phase 3a; operators use kubectl delete +
	// the existing handleCancellation path for offline.
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		return false, ctrl.Result{}, nil
	}

	// Committed = post-cutover OR the source already reported complete. In
	// both, the destination holds the only running copy, so a cancel must NOT
	// tear it down — it is ignored and the migration completes. Only the
	// pre-commit case (source still running) drives to Cancelled. A transient
	// error reading the source pod requeues rather than risking a destroy.
	committed, err := r.liveCommitted(ctx, mig)
	if err != nil {
		return true, ctrl.Result{}, fmt.Errorf("cancel: determine commit point: %w", err)
	}
	if committed {
		// Set CancelIgnored condition, return false-handled so phase
		// dispatch proceeds normally. Migration completes to Completed;
		// the condition is informational audit-trail.
		return r.markCancelIgnored(ctx, mig)
	}

	// Pre-commit: the source is still running — drive to Cancelled.
	res, terr := r.transitionCancelLive(ctx, mig)
	return true, res, terr
}

// markCancelIgnored sets the CancelIgnored condition with
// reason=PastCutover. Idempotent: setting a condition with
// status=True and the same reason is a no-op write (controller-
// runtime's setCondition + DeepEqual short-circuit). Returns
// false-handled so the caller proceeds to phase dispatch.
func (r *SwiftMigrationReconciler) markCancelIgnored(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) (handled bool, result ctrl.Result, err error) {
	// Already set with matching reason — no-op.
	for _, c := range mig.Status.Conditions {
		if c.Type == migrationv1alpha1.SwiftMigrationConditionCancelIgnored &&
			c.Status == metav1.ConditionTrue &&
			c.Reason == migrationv1alpha1.ReasonPastCutover {
			return false, ctrl.Result{}, nil
		}
	}

	// Persist the condition via status patch. Avoid full persist()
	// because we don't want to clobber phase-dispatch-in-progress
	// state — the caller is about to run phase dispatch which will
	// persist its own changes.
	patch := client.MergeFrom(mig.DeepCopy())
	setCondition(&mig.Status, migrationv1alpha1.SwiftMigrationConditionCancelIgnored,
		metav1.ConditionTrue, migrationv1alpha1.ReasonPastCutover,
		"spec.cancelRequested=true received post-cutover; migration cannot be reversed; continuing normal phase dispatch")
	if perr := r.Status().Patch(ctx, mig, patch); perr != nil {
		return false, ctrl.Result{}, fmt.Errorf("patch CancelIgnored condition: %w", perr)
	}
	recordAbort(abortExplicit, abortResolution{outcome: abortCommitted, path: r.cancelPath(ctx, mig)})
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "CancelIgnored",
			"spec.cancelRequested=true received post-cutover; migration cannot be reversed")
	}
	return false, ctrl.Result{}, nil
}

// transitionCancelLive drives a pre-commit spec.cancelRequested to
// Cancelled through resolveSourceOwnership (abort_live.go), the ownership
// check every abandonment shares: nothing is deleted until the source's send
// is known to have ended without completing, or the destination cancel has
// stopped the receive (or, failing both, the long-standing fallback). A
// transfer that completes first is left to complete: honorCancel marks it
// CancelIgnored.
func (r *SwiftMigrationReconciler) transitionCancelLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) (ctrl.Result, error) {
	res, err := r.resolveSourceOwnership(ctx, mig, abortExplicit)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cancel: %w", err)
	}
	switch res.outcome {
	case abortPending:
		return res.requeue, nil
	case abortCommitted:
		// honorCancel sees the commit on the next pass.
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.deleteAbandonedDestination(ctx, mig, res); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.clearLauncherCancel(ctx, mig); err != nil {
		return ctrl.Result{}, fmt.Errorf("clear cancel action on the source launcher: %w", err)
	}
	result, ferr := r.finalizeCancelled(ctx, mig, cancelledDetail(res))
	if ferr == nil && !result.Requeue {
		recordAbort(abortExplicit, res)
	}
	return result, ferr
}

// cancelledDetail is the Cancelled message for a settled abort.
func cancelledDetail(res abortResolution) string {
	switch {
	case res.outcome == abortSourceRetained:
		return fmt.Sprintf("cancelled on the source (%s); destination pod deleted", res.detail)
	case res.outcome == abortDestinationDead:
		return "destination pod deleted after swiftletd cancel ack"
	case res.outcome == abortUnresolved:
		return fmt.Sprintf("destination pod force-deleted; swiftletd cancel ack timed out (%s budget)", cancelAckTimeout)
	case res.detail == "the source SwiftGuest is gone":
		return "source SwiftGuest missing at cancel-time; nothing to clean up"
	case res.detail == "the destination pod does not exist":
		return "destination pod does not exist; cancel completes without swiftletd ack"
	default:
		return fmt.Sprintf("%s; destination pod deleted", res.detail)
	}
}

// finalizeCancelled stamps the SwiftMigration to terminal Cancelled
// phase with FailureReason=Cancelled and the supplied detail. The
// finalizer is removed by the next reconcile's terminal-phase
// branch.
func (r *SwiftMigrationReconciler) finalizeCancelled(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	detail string,
) (ctrl.Result, error) {
	// Optimistic: a reconcile working from a stale copy (the cache not yet
	// showing the Cancelled this reconcile's predecessor wrote) must not
	// overwrite it. It used to, replacing "destination pod force-deleted"
	// with "destination pod was never created" (D8).
	patch := client.MergeFromWithOptions(mig.DeepCopy(), client.MergeFromWithOptimisticLock{})
	now := metav1.Now()
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCancelled
	mig.Status.CompletedAt = &now
	mig.Status.FailureReason = migrationv1alpha1.FailureReasonCancelled
	mig.Status.FailureMessage = detail
	setReadyCondition(&mig.Status, metav1.ConditionFalse, ReasonCancelled, detail)
	setPhaseDetail(&mig.Status, detail)
	if perr := r.Status().Patch(ctx, mig, patch); perr != nil {
		if apierrors.IsConflict(perr) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("patch terminal Cancelled status: %w", perr)
	}
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, ReasonCancelled,
			fmt.Sprintf("migration cancelled: %s", detail))
	}
	return ctrl.Result{}, nil
}

// persistPhaseDetail writes a phaseDetail update via status patch.
// Used by the cancel sub-state transitions for operator visibility.
// Non-fatal: callers tolerate failure and log via Recorder if needed.
func (r *SwiftMigrationReconciler) persistPhaseDetail(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	detail string,
) error {
	if mig.Status.PhaseDetail == detail {
		return nil
	}
	patch := client.MergeFrom(mig.DeepCopy())
	setPhaseDetail(&mig.Status, detail)
	return r.Status().Patch(ctx, mig, patch)
}
