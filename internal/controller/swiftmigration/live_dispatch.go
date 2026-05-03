package swiftmigration

import (
	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// isLiveMode returns true when the SwiftMigration should be handled by
// the live-mode handler family (validating_live, preparing_live,
// stopandcopy_live, resuming_live).
//
// Two cases dispatch to live:
//
//  1. status.Mode == "live" — set by handleValidatingLive after mode
//     resolution. All post-Validating phase handlers gate on this.
//
//  2. status.Mode == "" AND spec.Mode == "live" — initial entry to
//     Validating before mode resolution has run. Only handleValidating
//     uses this branch (the other phase handlers cannot reach an
//     unresolved-status path because Validating must run first).
//
// What this function does NOT do: resolve mode=auto. Auto-mode
// resolution is B2 work in handleValidatingLive's body; a SwiftMigration
// with spec.Mode=auto stays in the offline path until B2 lands the
// auto-resolution logic. The B1 stub deliberately scopes dispatch to
// explicit-live so the dispatch wiring itself is unambiguous and
// testable in isolation from mode-resolution logic.
//
// Architect-discipline review answer (Q1) baked in: each handleXxxLive
// asserts isLiveMode(mig) at entry. If the assertion fails (which would
// indicate a bug, not a race), it returns phaseFailure with
// FailureReasonOther rather than fall through to offline. Default-to-
// explicit per PR #26.
func isLiveMode(mig *migrationv1alpha1.SwiftMigration) bool {
	if mig.Status.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		return true
	}
	if mig.Status.Mode == "" && mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		return true
	}
	return false
}
