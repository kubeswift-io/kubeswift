package swiftmigration

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// resumingLivePollInterval is the requeue cadence while waiting for
// GuestRunning=True on the dst pod's SwiftGuest. The labeled informer
// (§5.1) wakes the controller faster on actual condition transitions;
// the periodic requeue is the safety net for missed events. Matches
// Phase 1's resumingPollInterval value (5s) for consistency.
const resumingLivePollInterval = 5 * time.Second

// AnnotationGuestIP is the dst-pod-side annotation D3 writes after
// receive-complete (rust/swiftletd/src/action.rs::propagate_guest_ip_annotation).
// Resuming-live reads this for status.targetIP. The key matches
// rust/swiftletd/src/lease.rs::ANNOTATION_GUEST_IP, the same key
// Phase 1's first-boot lease discovery uses.
const AnnotationGuestIP = "kubeswift.io/guest-ip"

// Phase 3a phaseDetail vocabulary additions for Resuming-live per
// §6.4 stability discipline.
const (
	phaseDetailLiveResumingWaiting = "waiting for guest health on destination"
	phaseDetailLiveResumingHealthy = "destination guest healthy"
)

// handleResumingLive implements the live-mode Resuming phase per
// design §2.3 Resuming and §3.6 (completion gate).
//
// **Inputs at Resuming entry** (cutover already complete; B3 writes
// these):
//   - SwiftGuest.status.podRef.name points at the dst pod (cutover
//     step 1).
//   - src pod has been deleted (cutover step 2).
//   - SwiftMigration.phase has just transitioned to Resuming (cutover
//     step 3).
//
// **Responsibilities** (B2.3 scope):
//
//  1. Defensive guard: isLiveMode at entry → FailureReasonOther
//     (architect Q1).
//  2. Read SwiftGuest.status.podRef.name. Empty/nil is an internal
//     error: Resuming should only be entered post-cutover step 1.
//     Transition to Failed with FailureReasonOther.
//  3. Stamp status.ResumingStartedAt on first Resuming reconcile —
//     anchor for ObservedDowntime.
//  4. Get the dst pod by canonicalPodName(guest):
//     - NotFound → Failed with FailureReasonPodTerminated.
//     - Exists but pod.Status.Phase != Running → phaseRequeue.
//  5. Read SwiftGuest's GuestRunning condition (the §3.6 W1 gate):
//     - Missing or False → phaseRequeue.
//     - True → proceed.
//  6. Read kubeswift.io/guest-ip annotation from the dst pod (D3
//     writes; see swiftletd action.rs::propagate_guest_ip_annotation).
//     Set status.TargetIP. Annotation absence is NOT a failure —
//     §3.6 explicitly tolerates a brief gap between GuestRunning=True
//     and D3's write. spec.timeout is the safety net.
//  7. spec.timeout enforcement (F4.3 detection mechanism per §4.3):
//     elapsed > spec.timeout from status.StartedAt → Failed with
//     FailureReasonTimeout. Default 30m; webhook minimum
//     60s for mode=live (PR B1).
//  8. Compute and stamp:
//     - status.ObservedDowntime = now - status.ResumingStartedAt
//     - status.ObservedTransferDuration: NOT computed here. It is
//     parsed from the src pod's migration-pause-window-ms annotation
//     by stopandcopy_live during StopAndCopy-live; the Resuming
//     handler leaves it as-is (preserve whatever was already stamped).
//  9. Set status.CompletedAt, transition to Completed.
//
// **shouldCheckSourcePodUID returns False here** — Resuming is
// post-cutover per the helper's switch statement (B1's
// source_pod_uid.go covers Pending/Validating/Preparing/StopAndCopy
// only; Resuming defaults to false). UID-change detection in
// Resuming would produce false-alarm Failed transitions because
// cutover step 2 intentionally deleted the src pod.
//
// **canonicalPodName resolution** for dst pod lookup: post-cutover
// SwiftGuest.status.podRef.name carries the dst pod name. The helper
// from Group A.7 returns that value; B2.3 doesn't hardcode the dst
// pod name format — that's B2.2's concern, B2.3 reads what cutover
// wrote.
func (r *SwiftMigrationReconciler) handleResumingLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig, status) {
		return phaseFailure(
			"internal: handleResumingLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}

	// Re-resolve SwiftGuest. Resuming reads multiple status fields
	// from the guest (podRef, GuestRunning condition); a fresh Get is
	// cheaper than threading state through reconciles.
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source SwiftGuest %q deleted during Resuming", mig.Spec.GuestRef.Name),
				migrationv1alpha1.FailureReasonOther)
		}
		return phaseTransient(fmt.Errorf("get source guest: %w", err))
	}

	// Defensive: Resuming should only be entered after cutover step 1
	// (which sets podRef.name to the dst pod). Empty podRef indicates
	// a state-machine bug — fail-fast rather than continue.
	if guest.Status.PodRef == nil || guest.Status.PodRef.Name == "" {
		return phaseFailure(
			fmt.Sprintf("Resuming entered but SwiftGuest %q has empty status.podRef.name; cutover step 1 must run before Resuming", guest.Name),
			migrationv1alpha1.FailureReasonOther)
	}

	// Stamp ResumingStartedAt on first Resuming reconcile.
	if status.ResumingStartedAt == nil {
		now := metav1.Now()
		status.ResumingStartedAt = &now
	}

	// spec.timeout enforcement. F4.3 per §4.3: total-migration cap
	// from status.StartedAt. Default 30m; webhook minimum
	// 60s for mode=live.
	if mig.Spec.Timeout != nil && mig.Spec.Timeout.Duration > 0 && status.StartedAt != nil {
		if time.Since(status.StartedAt.Time) > mig.Spec.Timeout.Duration {
			return phaseFailure(
				fmt.Sprintf("spec.timeout=%s exceeded since StartedAt; migration did not complete in time", mig.Spec.Timeout.Duration),
				migrationv1alpha1.FailureReasonTimeout)
		}
	}

	// Get the dst pod via canonicalPodName resolution (Group A.7's
	// helper; same shape as the SwiftGuest controller's pod lookups).
	dstPodName := canonicalPodNameForGuest(&guest)
	var dstPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: dstPodName, Namespace: guest.Namespace}, &dstPod); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("destination pod %q terminated post-cutover; SwiftGuest is now orphaned and requires operator intervention", dstPodName),
				migrationv1alpha1.FailureReasonPodTerminated)
		}
		return phaseTransient(fmt.Errorf("get destination pod: %w", err))
	}

	// Pod must be Running before checking GuestRunning. A Pending or
	// terminating dst pod can't have a GuestRunning condition flipped
	// True yet.
	if dstPod.Status.Phase != corev1.PodRunning {
		setPhaseDetail(status, phaseDetailLiveResumingWaiting)
		setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
			fmt.Sprintf("destination pod %q phase=%s; awaiting Running", dstPodName, dstPod.Status.Phase))
		return phaseRequeue(resumingLivePollInterval)
	}

	// Check the §3.6 completion gate: GuestRunning=True on the
	// SwiftGuest. swiftletd-on-dst writes this via DynamicObject the
	// same way Phase 1's first-boot does.
	if !isGuestRunningTrue(&guest) {
		setPhaseDetail(status, phaseDetailLiveResumingWaiting)
		setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
			"awaiting GuestRunning=True on destination (live resume + swiftletd condition write)")
		return phaseRequeue(resumingLivePollInterval)
	}

	// GuestRunning=True. Read the dst pod's guest-ip annotation (D3
	// writes; absence is tolerable per §3.6). spec.timeout is the
	// safety net for "annotation never appears".
	if ip := dstPod.Annotations[AnnotationGuestIP]; ip != "" {
		status.TargetIP = ip
	}

	// W27a fix (PR #54 follow-up, Tracked Follow-up #7):
	// observedDowntime is the wall-clock window between cutover step 2
	// dispatch (src pod Delete; vCPU pause begins inside CH on src)
	// and dst guest reaching GuestRunning=True (vCPU pause ends on
	// dst). The prior implementation anchored on
	// status.ResumingStartedAt — stamped one apiserver round-trip
	// after step 2 AND consumed in the same reconcile invocation,
	// producing sub-millisecond observedDowntime values across all 17
	// PR #46 + E12 walkthrough runs. CutoverStep2DispatchedAt is the
	// correct anchor.
	//
	// Defensive nil-check: CutoverStep2DispatchedAt is stamped by
	// cutoverStep2 unconditionally (even on NotFound recovery), so
	// reaching Resuming with it nil indicates a state-machine
	// invariant violation. Log + leave ObservedDowntime nil so
	// operators see a missing field, never a wrong one.
	now := metav1.Now()
	if status.CutoverStep2DispatchedAt != nil {
		downtime := metav1.Duration{Duration: now.Sub(status.CutoverStep2DispatchedAt.Time)}
		status.ObservedDowntime = &downtime
	} else {
		log.FromContext(ctx).Info(
			"observedDowntime not computed: status.CutoverStep2DispatchedAt is nil at Resuming completion (state-machine invariant violation; W27a)",
			"migration", mig.Name)
	}
	// status.ObservedTransferDuration is stamped by stopandcopy_live's
	// substateSendComplete handler (W27b fix) reading the src pod's
	// kubeswift.io/migration-pause-window-ms annotation. The Resuming
	// handler leaves it as-is — preserves whatever StopAndCopy wrote.

	status.CompletedAt = &now
	setPhase(status, migrationv1alpha1.SwiftMigrationPhaseCompleted)
	setReadyCondition(status, metav1.ConditionTrue, ReasonCompleted,
		fmt.Sprintf("live migration to %q complete; guest running on destination pod %q",
			status.DestinationNode, dstPodName))
	completionDetail := phaseDetailLiveResumingHealthy
	if status.TargetIP != "" {
		completionDetail = fmt.Sprintf("%s (IP %s)", phaseDetailLiveResumingHealthy, status.TargetIP)
	}
	setPhaseDetail(status, completionDetail)
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, ReasonCompleted,
			fmt.Sprintf("live migration completed: SwiftGuest %q running on dst pod %q (node %q)",
				guest.Name, dstPodName, status.DestinationNode))
	}
	return phaseAdvance()
}

// isGuestRunningTrue returns true when the SwiftGuest's GuestRunning
// condition is present with Status=True. Mirrors the offline-path
// check in resuming.go but factored out so the live-mode body reads
// linearly. swiftletd-on-dst writes this condition via the same
// kube-rs DynamicObject path Phase 1 uses (§3.6).
func isGuestRunningTrue(guest *swiftv1alpha1.SwiftGuest) bool {
	for _, c := range guest.Status.Conditions {
		if c.Type == guestRunningConditionType && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}
