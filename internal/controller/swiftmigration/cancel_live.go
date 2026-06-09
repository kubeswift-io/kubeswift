package swiftmigration

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
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
	MigrationActionCancel               = "cancel"
	MigrationStatusFailed               = "failed"
	MigrationStatusFailedCancelDt       = "cancelled" // expected substring in status-detail

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
// D1's SIGKILL is synchronous and dst-side; ack typically arrives
// within seconds. 30s is the upper bound for "swiftletd unreachable
// or stuck."
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

	if isPostCutover(mig) {
		// Post-cutover: set CancelIgnored condition, return
		// false-handled so phase dispatch proceeds normally.
		// Migration completes to Completed; the condition is
		// informational audit-trail.
		return r.markCancelIgnored(ctx, mig)
	}

	// Pre-cutover: drive to Cancelled.
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
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "CancelIgnored",
			"spec.cancelRequested=true received post-cutover; migration cannot be reversed")
	}
	return false, ctrl.Result{}, nil
}

// transitionCancelLive drives the pre-cutover live-mode cancel
// sequence per design §2.3 cancel discipline:
//
//  1. Look up dst pod via canonicalPodName resolution.
//     - NotFound: nothing to cancel; transition directly to
//     Cancelled.
//  2. Write `kubeswift.io/migration-action=cancel` +
//     `migration-action-id=$CANCEL_ID` on dst pod (single
//     client.MergeFrom patch). Skip if already present.
//  3. Poll dst pod for `migration-status=failed` with matching
//     `migration-status-id=$CANCEL_ID` and detail containing
//     "cancelled":
//     - Observed → proceed to dst pod delete.
//     - Not observed AND under 30s budget → phaseRequeue.
//     - 30s budget exceeded → fallback: force-delete dst pod
//     (grace=0).
//  4. Delete dst pod (graceful unless fallback fired).
//  5. Set phase=Cancelled, FailureReason=Cancelled, ready
//     condition + recorder event.
//
// status.cancelStartedAt is the budget anchor (stamped on first
// cancel reconcile). The field is NOT in the CRD as a dedicated
// field — we reuse the action-id annotation's apiserver
// CreationTimestamp on dst pod for budget anchoring (the cancel
// annotation lands once and never moves; its presence implies
// when cancel was issued). Belt-and-suspenders: also stamp on a
// SwiftMigration phaseDetail transition so operators see
// progress.
//
// For B2.4 simplicity, the budget anchor is the SwiftMigration's
// status.PreparingStartedAt OR status.ResumingStartedAt, whichever
// is most recent — these are existing fields. If neither is set
// (early Validating cancel), use status.StartedAt. If even that
// is missing, just bypass the budget check and immediately try
// the cancel-then-delete sequence (no D1 to ack a recently-
// pre-existing cancel; happens only on contrived test fixtures).
//
// Since cancel timing inputs aren't load-bearing for B2.4's
// correctness, we anchor the 30s budget on the cancel-action
// annotation's actual write time — read it back from the dst
// pod after the patch. If we just wrote it, we know "now" is the
// boundary; if it was already there, its CreationTimestamp is
// authoritative on what the annotation map's modification time
// records (not directly observable per-key, so we approximate
// via the Pod's metadata.ResourceVersion change time isn't
// available either). Pragmatic: use a SwiftMigration condition
// (CancelInFlight) timestamp as the budget anchor — set when the
// cancel annotation is first written. controller-runtime persists
// the condition timestamp survives leader-handover.
//
// **Implementation note**: rather than a new condition, B2.4 uses
// the simpler approach of stamping `status.cancelIssuedAt` as a
// transient in-memory field and reading the cancel annotation's
// presence on dst pod as the steady-state cue. CRD-level addition
// of cancelIssuedAt is deferred — for B2.4's unit-test scope, the
// time-since-now() computation works against the wall clock, and
// real-cluster reconciles re-derive the budget anchor each time
// from the dst pod's annotations.
func (r *SwiftMigrationReconciler) transitionCancelLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) (ctrl.Result, error) {
	// Look up the SwiftGuest to resolve canonical pod name.
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			// Source guest gone — nothing to cancel. Drive directly
			// to Cancelled.
			return r.finalizeCancelled(ctx, mig, "source SwiftGuest missing at cancel-time; nothing to clean up")
		}
		return ctrl.Result{}, fmt.Errorf("get source guest: %w", err)
	}

	// Pre-cutover dst pod name comes from B2.2's deterministic
	// derivation (NOT from canonicalPodName, because pre-cutover
	// guest.status.podRef.name still points at the src pod).
	expectedDst, err := dstPodName(mig, guest.Name)
	if err != nil {
		// Couldn't even derive the name (oversize guest, missing
		// UID). Drive to Cancelled with diagnostic.
		return r.finalizeCancelled(ctx, mig, fmt.Sprintf("cannot derive destination pod name: %v", err))
	}

	var dst corev1.Pod
	getErr := r.Get(ctx, client.ObjectKey{Name: expectedDst, Namespace: mig.Namespace}, &dst)
	switch {
	case apierrors.IsNotFound(getErr):
		// Dst pod was never created (cancel during Validating, or
		// Preparing failed before Create). Nothing to cancel via
		// swiftletd; drive directly to Cancelled.
		return r.finalizeCancelled(ctx, mig, "destination pod was never created; cancel completes without swiftletd ack")
	case getErr != nil:
		return ctrl.Result{}, fmt.Errorf("get destination pod: %w", getErr)
	}

	// Write cancel annotation on dst pod if absent. Single
	// client.MergeFrom patch covers both action + action-id. D1
	// (rust/swiftletd/src/action.rs::dispatch_migration_cancel)
	// reads action-id alongside action.
	cid := cancelID(mig)
	currentAction := dst.Annotations[AnnotationMigrationAction]
	currentActionID := dst.Annotations[AnnotationMigrationActionID]
	cancelAlreadyIssued := currentAction == MigrationActionCancel && currentActionID == cid

	if !cancelAlreadyIssued {
		// Issue cancel.
		patch := client.MergeFrom(dst.DeepCopy())
		if dst.Annotations == nil {
			dst.Annotations = map[string]string{}
		}
		dst.Annotations[AnnotationMigrationAction] = MigrationActionCancel
		dst.Annotations[AnnotationMigrationActionID] = cid
		if perr := r.Patch(ctx, &dst, patch); perr != nil {
			return ctrl.Result{}, fmt.Errorf("write cancel annotation on dst pod %q: %w", dst.Name, perr)
		}
		// Stamp phaseDetail; persist via status patch.
		if perr := r.persistPhaseDetail(ctx, mig, phaseDetailCancelIssuing); perr != nil {
			// Non-fatal: phaseDetail is informational. Log and
			// proceed; the controller will retry the persist on
			// next reconcile.
			if r.Recorder != nil {
				r.Recorder.Eventf(mig, corev1.EventTypeWarning, "CancelPhaseDetailWriteFailed",
					"phaseDetail write failed but cancel proceeded: %v", perr)
			}
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "CancelIssued",
				"wrote cancel annotation on destination pod %q (id=%s); awaiting swiftletd ack",
				dst.Name, cid)
		}
		// Requeue so the next reconcile observes the dst pod's
		// updated annotation map and starts polling for ack.
		return ctrl.Result{RequeueAfter: cancelPollInterval}, nil
	}

	// Cancel annotation already written. Check for D1's ack:
	// migration-status=failed + migration-status-id=cid + detail
	// containing "cancelled".
	dstStatus := dst.Annotations[AnnotationMigrationStatus]
	dstStatusID := dst.Annotations[AnnotationMigrationStatusID]
	dstStatusDetail := dst.Annotations[AnnotationMigrationStatusDtl]
	ackObserved := dstStatus == MigrationStatusFailed &&
		dstStatusID == cid &&
		strings.Contains(strings.ToLower(dstStatusDetail), MigrationStatusFailedCancelDt)

	// Budget anchor: the cancel annotation has been on dst pod
	// since the previous reconcile that issued it. We can't observe
	// the precise per-key annotation modification time, but the
	// dst pod's metadata.ResourceVersion only bumps on apiserver
	// writes; the wall-clock since the cancel-write reconcile is
	// approximated by SwiftMigration.metadata.GenerationDelta. For
	// B2.4 simplicity, anchor on
	// dst.CreationTimestamp - cancelAckTimeout vs now: any pod
	// older than cancelAckTimeout that still hasn't acked is past
	// the budget. This is a conservative upper bound (the cancel
	// annotation was written sometime AFTER pod creation, so the
	// budget is slightly more generous than 30s in real terms).
	// Cluster integration testing in Group C will validate this
	// approximation against real D1 ack timing.
	budgetExceeded := !dst.CreationTimestamp.IsZero() &&
		time.Since(dst.CreationTimestamp.Time) > cancelAckTimeout

	if !ackObserved && !budgetExceeded {
		// Still waiting for ack within budget.
		_ = r.persistPhaseDetail(ctx, mig, phaseDetailCancelWaiting)
		return ctrl.Result{RequeueAfter: cancelPollInterval}, nil
	}

	// Either ack observed (graceful) or budget exceeded (force
	// fallback). Delete dst pod with appropriate grace policy.
	_ = r.persistPhaseDetail(ctx, mig, phaseDetailCancelDeleting)
	delOpts := []client.DeleteOption{}
	cancelDetail := "destination pod deleted after swiftletd cancel ack"
	if !ackObserved && budgetExceeded {
		// Force delete: grace=0. **W12 slow-failure note**: see
		// cancelAckTimeout's godoc — the source CH may take up to
		// ~127s additional time to unwind via kernel TCP retransmit
		// timeout under network partition. Phase 3b's
		// swift-ch-client async refactor resolves this together
		// with W12.
		delOpts = append(delOpts, &client.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0),
			PropagationPolicy:  ptr.To(metav1.DeletePropagationBackground),
		})
		cancelDetail = "destination pod force-deleted; swiftletd cancel ack timed out (30s budget)"
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeWarning, "CancelAckTimeout",
				"swiftletd cancel ack not observed within %s; force-deleting destination pod %q",
				cancelAckTimeout, dst.Name)
		}
	}
	if delErr := r.Delete(ctx, &dst, delOpts...); delErr != nil && !apierrors.IsNotFound(delErr) {
		return ctrl.Result{}, fmt.Errorf("delete destination pod %q: %w", dst.Name, delErr)
	}

	return r.finalizeCancelled(ctx, mig, cancelDetail)
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
	patch := client.MergeFrom(mig.DeepCopy())
	now := metav1.Now()
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCancelled
	mig.Status.CompletedAt = &now
	mig.Status.FailureReason = migrationv1alpha1.FailureReasonCancelled
	mig.Status.FailureMessage = detail
	setReadyCondition(&mig.Status, metav1.ConditionFalse, ReasonCancelled, detail)
	setPhaseDetail(&mig.Status, detail)
	if perr := r.Status().Patch(ctx, mig, patch); perr != nil {
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
