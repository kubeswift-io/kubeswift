package swiftmigration

import (
	"context"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// handleValidatingLive is the live-mode Validating phase entry point.
//
// B1 SCOPE: stub only. Returns phaseFailure with FailureReasonOther so
// any premature live-mode SwiftMigration submitted against a controller
// that has B1 wiring but not B2's body fails fast and visibly rather
// than silently advancing to a broken Preparing.
//
// B2 SCOPE (next checkpoint): live-mode validation body —
//   - Re-resolve source SwiftGuest + class (defense in depth).
//   - Stamp status.Mode = live, status.SourceNode, status.DestinationNode.
//   - Stamp status.SourcePodUID for F4.2 pod-replacement detection.
//   - Run capacity check on destination node.
//   - For mode=auto, run resolution logic (live-capable storage +
//     non-VFIO + same-CH-version → live; else → offline).
//   - Set Compatible condition True/False.
//   - Advance to Preparing on success.
//
// Defensive guard: assert isLiveMode at entry. The dispatch wiring in
// handleValidating gates on isLiveMode, but a future code change that
// invokes this directly should not silently produce wrong-mode behavior.
func (r *SwiftMigrationReconciler) handleValidatingLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig) {
		return phaseFailure(
			"internal: handleValidatingLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}
	return phaseFailure(
		"live-mode Validating not yet implemented (B2 work)",
		migrationv1alpha1.FailureReasonOther,
	)
}
