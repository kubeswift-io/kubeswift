package swiftmigration

import (
	"context"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// handlePreparingLive is the live-mode Preparing phase entry point.
//
// B1 SCOPE: stub only.
//
// B2 SCOPE: live-mode Preparing body —
//   - Build destination launcher pod (`<guest>-mig-<short-uid>`) with
//     ownerRef to the SwiftGuest (F1.5 Option 2 + (P)) and
//     migration-role=destination label.
//   - Wait for destination pod ready (CH-receiver listening).
//   - Stamp status.RecvAttempts on each retry.
//   - Run F4.2 source-pod-UID-change detection (gated by
//     shouldCheckSourcePodUID).
//   - Advance to StopAndCopy on receiver-listening confirmation.
//
// Defensive guard: assert isLiveMode at entry per the architect-
// discipline review answer to Q1.
func (r *SwiftMigrationReconciler) handlePreparingLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig) {
		return phaseFailure(
			"internal: handlePreparingLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}
	return phaseFailure(
		"live-mode Preparing not yet implemented (B2 work)",
		migrationv1alpha1.FailureReasonOther,
	)
}
