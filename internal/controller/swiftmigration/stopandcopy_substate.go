package swiftmigration

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
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
	// swiftletd-on-dst to acknowledge via migration-status=running with
	// matching status-id." Detected when dst pod's migration-action
	// matches $RECV_ID but no migration-status=running yet.
	substateRecvPending
	// substatePreSend is "dst acknowledged receive; controller must
	// write send-action on src pod." Detected when dst pod's
	// migration-status=running with matching $RECV_ID, AND src pod
	// has no migration-action matching $SEND_ID. Collapses the design
	// table's "recv-accepted" + "send-issued" into one state since
	// the controller's only action in this state is to write the
	// send-action.
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

// sendActionID returns the deterministic $SEND_ID per design F1.8:
//
//	$SEND_ID = "<swiftmigration.Name>:send:<SendAttempts>"
//
// Same min-clamp semantics as recvActionID.
func sendActionID(mig *migrationv1alpha1.SwiftMigration) string {
	n := int32(1)
	if mig.Status.SendAttempts > n {
		n = mig.Status.SendAttempts
	}
	return fmt.Sprintf("%s:send:%d", mig.Name, n)
}

// migrationActionVerbReceive is the action verb the controller writes
// to dispatch swiftletd-on-dst's MigrationReceive handler. Matches
// rust/swiftletd/src/action.rs::parse_migration_verb's "receive" arm.
const migrationActionVerbReceive = "receive"

// migrationActionVerbSend is the action verb for swiftletd-on-src's
// MigrationSend handler.
const migrationActionVerbSend = "send"

// migrationStatusRunning matches the dst-side terminal-success verb
// (StatusKind::Custom("running") in swiftletd's namespace map for
// migration; per Phase 2 §3.1 the dst writes "running" while the
// src writes "complete" — distinct verbs for distinct semantics).
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

	// Dst-side terminal: failed (D2 watchdog or CH receive error).
	// We do NOT check dst-side "running" here as a terminal state —
	// dst running means recv-accepted, NOT cutover-ready. The W1 gate
	// is src=complete; dst=running is intermediate.
	if podStatusMatches(dst, MigrationStatusFailed, rid) {
		return substateDstFailed
	}

	// Send-action acknowledged on src (action-id matches $SEND_ID):
	// we wrote send-action but no terminal src status yet, so we're
	// in send-pending.
	if podActionMatches(src, migrationActionVerbSend, sid) {
		return substateSendPending
	}

	// Recv-action acknowledged on dst with running status: dst
	// accepted the receive; we need to write send-action on src next.
	if podStatusMatches(dst, migrationStatusRunning, rid) {
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
