package swiftmigration

import (
	"context"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// handleResumingLive is the live-mode Resuming phase entry point.
//
// B1 SCOPE: stub only.
//
// B2 SCOPE: live-mode Resuming body —
//   - Poll destination guest's GuestRunning condition + primaryIP.
//   - Stamp status.TargetIP from dst pod annotation.
//   - Stamp status.ObservedPauseWindow from cutover-step-1 timestamps.
//   - Advance to Completed on observed running + IP discovered.
//
// Defensive guard: assert isLiveMode at entry per the architect-
// discipline review answer to Q1.
func (r *SwiftMigrationReconciler) handleResumingLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig) {
		return phaseFailure(
			"internal: handleResumingLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}
	return phaseFailure(
		"live-mode Resuming not yet implemented (B2 work)",
		migrationv1alpha1.FailureReasonOther,
	)
}
