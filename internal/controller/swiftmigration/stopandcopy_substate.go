package swiftmigration

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// StopAndCopy live-mode sub-state. Phase 3a's StopAndCopy phase
// orchestrates a multi-step migration body via observable cluster
// state alone — no in-memory state survives reconcile. Each reconcile
// derives the current sub-state from src/dst pod annotations + the
// SwiftMigration's RecvAttempts/SendAttempts counters and dispatches
// the next action.
//
// Per design §2.3 StopAndCopy phase. The 6 sub-states from the design
// table are collapsed into this enum (substatePreSend collapses
// "recv-accepted" + "send-issued" into a single observable state, since
// once recv-accepted is observed the controller's next action is to
// write the send-action; substateSendPending collapses "send-issued" +
// "send-in-flight" since once the send-action is written, both states
// are observably "waiting for src migration-status").
//
// The cutover sub-state is intentionally OMITTED from this enum —
// it's a separate handler invocation in B3.2. B3.1 parks at
// substateSrcCompleted and returns phaseRequeue; B3.2 replaces the
// park with the 3-step cutover sequence.
type stopAndCopySubstate int

const (
	// substatePreRecv is the initial state when entering StopAndCopy.
	// The controller must write the receive-action annotation on the
	// dst pod (with $RECV_ID). Detected when the dst pod has no
	// migration-action matching $RECV_ID.
	substatePreRecv stopAndCopySubstate = iota
	// substateRecvPending is "receive-action written; waiting for
	// swiftletd-on-dst to acknowledge via migration-status=receive-ready
	// with matching status-id." Detected when dst pod's migration-action
	// matches $RECV_ID but no migration-status=receive-ready yet.
	substateRecvPending
	// substatePreSend is "dst acknowledged receive; controller must
	// write send-action on src pod." Detected when dst pod's
	// migration-status=receive-ready with matching $RECV_ID, AND src
	// pod has no migration-action matching $SEND_ID. Collapses the
	// design table's "recv-accepted" + "send-issued" into one state
	// since the controller's only action in this state is to write the
	// send-action. Gates on receive-ready (pre-dispatch), NOT running
	// (terminal) — Finding 2 fix; see migrationStatusReceiveReady.
	substatePreSend
	// substateSendPending is "send-action written; waiting for
	// swiftletd-on-src to write migration-status=complete (success)
	// or =failed (failure)." Detected when src pod's migration-action
	// matches $SEND_ID but no terminal status yet. Collapses
	// "send-issued" + "send-in-flight" since both are observably the
	// same.
	substateSendPending
	// substateSrcCompleted is the W1 gate per F1.2: src pod's
	// migration-status=complete with matching $SEND_ID. Cutover
	// sequence (B3.2) starts from here.
	substateSrcCompleted
	// substateDstFailed is "dst pod reported migration-status=failed
	// with matching $RECV_ID." Detection mechanism for D2 watchdog
	// abnormal-exit AND for receive-side Cloud Hypervisor errors
	// (per Phase 2 PR-B's failure signaling).
	substateDstFailed
	// substateSrcFailed is "src pod reported migration-status=failed
	// with matching $SEND_ID." Detection mechanism for W1 violations,
	// CH-internal errors, network failures during send. Per F4 the
	// controller maps the FailureReason based on detail string.
	substateSrcFailed
	// substateDstRejected is "dst pod reported migration-status=
	// rejected with matching $RECV_ID." swiftletd's decide() refused
	// the receive-action (e.g., missing phase2-ack — addressed by W13
	// for happy-path; namespace mismatch; action-id mismatch). Without
	// W14's recognition the migration would stall at substateRecvPending
	// until spec.timeout.
	substateDstRejected
	// substateSrcRejected is the src-side equivalent — swiftletd
	// refused the send-action with status=rejected. Same default
	// rationale as substateDstRejected.
	substateSrcRejected
)

// String returns the design-doc sub-state name for diagnostic logging
// + test failure messages. Not used as a phaseDetail value (those
// constants live in api/migration/v1alpha1).
func (s stopAndCopySubstate) String() string {
	switch s {
	case substatePreRecv:
		return "pre-recv"
	case substateRecvPending:
		return "recv-pending"
	case substatePreSend:
		return "pre-send"
	case substateSendPending:
		return "send-pending"
	case substateSrcCompleted:
		return "src-completed"
	case substateDstFailed:
		return "dst-failed"
	case substateSrcFailed:
		return "src-failed"
	case substateDstRejected:
		return "dst-rejected"
	case substateSrcRejected:
		return "src-rejected"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// recvActionID returns the deterministic $RECV_ID for this
// SwiftMigration per design F1.8:
//
//	$RECV_ID = "<swiftmigration.Name>:recv:<RecvAttempts>"
//
// PR 1 has no retry path — RecvAttempts is incremented to 1 alongside
// the receive-action write and stays at 1 for the migration's
// lifetime. Counter==0 on first reconcile (before any dispatch); the
// helper returns the same ID regardless of pre-increment vs post-
// increment timing by clamping the minimum to 1, so leader-handover
// between counter increment and annotation write produces the same
// $RECV_ID either way.
//
// Multiple counter values matter only for retry (Phase 5+); for PR 1
// the value 1 is invariant.
func recvActionID(mig *migrationv1alpha1.SwiftMigration) string {
	n := int32(1)
	if mig.Status.RecvAttempts > n {
		n = mig.Status.RecvAttempts
	}
	return fmt.Sprintf("%s:recv:%d", mig.Name, n)
}

// sendAttemptNumber returns the current send attempt number, clamped to a
// minimum of 1. Phase 3a kept this at 1 (no retry). Phase 3d's mTLS
// source-sidecar readiness retry increments mig.Status.SendAttempts, so
// the attempt number (and thus $SEND_ID) advances; this helper is the one
// place that derives it, shared by sendActionID and the substatePreSend
// counter write so the written action-id and the persisted counter never
// disagree.
func sendAttemptNumber(mig *migrationv1alpha1.SwiftMigration) int32 {
	n := int32(1)
	if mig.Status.SendAttempts > n {
		n = mig.Status.SendAttempts
	}
	return n
}

// sendActionID returns the deterministic $SEND_ID per design F1.8:
//
//	$SEND_ID = "<swiftmigration.Name>:send:<SendAttempts>"
//
// Same min-clamp semantics as recvActionID.
func sendActionID(mig *migrationv1alpha1.SwiftMigration) string {
	return fmt.Sprintf("%s:send:%d", mig.Name, sendAttemptNumber(mig))
}

// migrationActionVerbReceive is the action verb the controller writes
// to dispatch swiftletd-on-dst's MigrationReceive handler. Matches
// rust/swiftletd/src/action.rs::parse_migration_verb's "receive" arm.
const migrationActionVerbReceive = "receive"

// migrationActionVerbSend is the action verb for swiftletd-on-src's
// MigrationSend handler.
const migrationActionVerbSend = "send"

// migrationStatusReceiveReady matches the dst-side PRE-DISPATCH
// readiness verb (StatusKind::Custom("receive-ready") in swiftletd's
// migration namespace map — rust/swiftletd/src/action.rs
// pre_dispatch_status). swiftletd-on-dst writes this BEFORE it issues
// the blocking vm.receive-migration RPC: the CH receiver is listening
// and ready for the source to connect. This is the signal the
// controller gates the recv→send transition on (substatePreSend).
//
// LOAD-BEARING CONTRACT (Finding 2 fix): the recv→send gate MUST key
// on receive-ready, NOT on migrationStatusRunning. swiftletd writes
// receive-ready at pre-dispatch and "running" only at terminal
// (after vm.receive-migration completes, which requires the source to
// have connected and sent). Gating recv→send on "running" deadlocks:
// the controller waits for "running" before dispatching send on src,
// but the dst can only reach "running" after src sends. Phase 3b PR 1
// Commit C introduced this pre-dispatch/terminal split; the Phase 3a
// controller (written before PR 1) gated on "running" and was never
// exercised by PR 1's controller-less manual demo. See
// docs/migration/phase-3b-pr2-walkthrough.md Finding 2.
const migrationStatusReceiveReady = "receive-ready"

// migrationStatusRunning matches the dst-side TERMINAL-success verb
// (StatusKind::Custom("running"), action.rs success_status). swiftletd
// -on-dst writes "running" only AFTER vm.receive-migration completes
// (CH state=Running with the migrated guest live). The controller does
// NOT gate any transition on this verb: the W1 gate anchors on the
// src-side "complete" (which swiftletd-on-src writes only after its
// vm.send-migration internally probed dst CH for vm_info=Running per
// F1.2). Kept as a named constant to document the dst terminal verb
// and to keep the recv→send gate's "use receive-ready, not running"
// distinction legible to future maintainers.
const migrationStatusRunning = "running"

// migrationStatusComplete matches the src-side W1-gate-passing verb
// (StatusKind::Custom("complete")). When the controller observes this
// on the src pod with matching $SEND_ID, the W1 invariant per F1.2 is
// satisfied: swiftletd-on-src's vm.send-migration internally probed
// the dst CH for vm_info=Running before writing complete, so the
// controller can proceed to cutover without polling dst CH directly.
const migrationStatusComplete = "complete"

// deriveSubstate determines the current StopAndCopy live-mode
// sub-state from observable cluster state alone. Each reconcile
// invocation calls this to decide what to do next. The function reads
// from src and dst pod annotations + SwiftMigration counters; no
// in-memory state. Leader-handover survives by re-deriving on the
// new leader's first reconcile.
//
// Pod arguments may be nil — caller passes nil for "pod not yet
// fetched" or "pod NotFound." A nil pod cannot have annotations, so
// the corresponding state predicates short-circuit to false.
//
// **Order of evaluation matters**: failure states are checked first
// because a failed migration's annotations may also contain stale
// successful states from earlier sub-states; checking failures first
// surfaces the most-recent observable signal. Within failures, src
// is checked before dst (W1 gate per F1.2: src is the W1 anchor).
//
// **Source-side checks (failed/complete) take priority over dst-side
// checks** because once src reports terminal status, dst-side
// observations are stale (the migration body has finished from src's
// perspective). dst-side terminal status is checked separately for
// dst-only failure modes (D2 watchdog).
func deriveSubstate(mig *migrationv1alpha1.SwiftMigration, src, dst *corev1.Pod) stopAndCopySubstate {
	rid := recvActionID(mig)
	sid := sendActionID(mig)

	// Src-side terminal: complete (success) takes priority over failed
	// because complete with matching $SEND_ID is the W1 gate.
	if podStatusMatches(src, migrationStatusComplete, sid) {
		return substateSrcCompleted
	}
	if podStatusMatches(src, MigrationStatusFailed, sid) {
		return substateSrcFailed
	}
	// Src-side rejected (W14): swiftletd's decide() refused the send-
	// action. Terminal — fast-fail rather than wait for spec.timeout.
	if podStatusMatches(src, MigrationStatusRejected, sid) {
		return substateSrcRejected
	}

	// Dst-side terminal: failed (D2 watchdog or CH receive error).
	// We do NOT gate any transition on dst-side "running": post PR 1
	// Commit C, dst "running" is the TERMINAL receive-complete verb
	// (vm.receive-migration returned, guest live), not an intermediate
	// signal. The W1 gate anchors on src="complete" (which implies dst
	// is running per the F1.2 probe), so dst "running" needs no
	// separate consumer here. The recv→send transition gates on
	// dst="receive-ready" (the pre-dispatch readiness verb) below.
	if podStatusMatches(dst, MigrationStatusFailed, rid) {
		return substateDstFailed
	}
	// Dst-side rejected (W14): swiftletd-on-dst refused the receive-
	// action. Terminal — same rationale as src-side.
	if podStatusMatches(dst, MigrationStatusRejected, rid) {
		return substateDstRejected
	}

	// Send-action acknowledged on src (action-id matches $SEND_ID):
	// we wrote send-action but no terminal src status yet, so we're
	// in send-pending.
	if podActionMatches(src, migrationActionVerbSend, sid) {
		return substateSendPending
	}

	// Recv-action acknowledged on dst with receive-ready status: dst's
	// CH receiver is listening and ready; we need to write send-action
	// on src next. Gates on receive-ready (pre-dispatch), NOT running
	// (terminal) — see migrationStatusReceiveReady's load-bearing
	// contract note (Finding 2 fix).
	if podStatusMatches(dst, migrationStatusReceiveReady, rid) {
		return substatePreSend
	}

	// Recv-action written but no dst status yet: waiting for swiftletd
	// to ack.
	if podActionMatches(dst, migrationActionVerbReceive, rid) {
		return substateRecvPending
	}

	// Nothing observable yet: fresh entry (or post-leader-handover
	// before any annotation was written).
	return substatePreRecv
}

// podActionMatches returns true when pod has migration-action and
// migration-action-id annotations both matching the expected values.
// nil pod → false.
func podActionMatches(pod *corev1.Pod, expectedAction, expectedID string) bool {
	if pod == nil {
		return false
	}
	if pod.Annotations[AnnotationMigrationAction] != expectedAction {
		return false
	}
	if pod.Annotations[AnnotationMigrationActionID] != expectedID {
		return false
	}
	return true
}

// podStatusMatches returns true when pod has migration-status and
// migration-status-id annotations both matching the expected values.
// nil pod → false.
func podStatusMatches(pod *corev1.Pod, expectedStatus, expectedID string) bool {
	if pod == nil {
		return false
	}
	if pod.Annotations[AnnotationMigrationStatus] != expectedStatus {
		return false
	}
	if pod.Annotations[AnnotationMigrationStatusID] != expectedID {
		return false
	}
	return true
}
