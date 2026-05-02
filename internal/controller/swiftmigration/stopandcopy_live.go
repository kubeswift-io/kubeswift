package swiftmigration

import (
	"context"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// handleStopAndCopyLive is the live-mode StopAndCopy phase entry point.
//
// B1 SCOPE: stub only. This is the load-bearing handler for B3 — the
// 6-substate machine and the 3-step cutover ordering invariant land
// here, gated by the architect-discipline review's Q3 mitigations:
//
//   - Q3.1: cutover MUST fire from a single reconcile invocation. The
//     B3 implementation will encode this via a comment + a unit test
//     asserting the cutover branch never returns phaseRequeue.
//   - Q3.3 (c): PodRefSwapped condition is DERIVED from cluster state
//     (SwiftGuest.status.podRef.name == dst-pod-name) on every cutover-
//     substate reconcile entry, not written as a separate atomic step.
//     status.cutoverStep1At timestamp is stamped alongside the podRef
//     patch in a single combined patch (operator-visible audit data).
//
// B3 SCOPE: full 6-substate machine —
//
//	recv-issued → recv-accepted → send-issued → send-in-flight →
//	src-completed → cutover-3-steps.
//
// Plus the failure handler split: pre-cutover failures restore source;
// mid-cutover failures drive forward; post-cutover failures land
// terminally without source restore.
//
// Defensive guard: assert isLiveMode at entry per the architect-
// discipline review answer to Q1.
func (r *SwiftMigrationReconciler) handleStopAndCopyLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig) {
		return phaseFailure(
			"internal: handleStopAndCopyLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}
	return phaseFailure(
		"live-mode StopAndCopy not yet implemented (B3 work)",
		migrationv1alpha1.FailureReasonOther,
	)
}
