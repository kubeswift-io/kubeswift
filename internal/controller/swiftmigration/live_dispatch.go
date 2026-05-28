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
//  1. status.Mode == "live" — set by resolveAutoMode (auto path) or
//     stamped explicitly by handleValidatingLive. All post-Validating
//     phase handlers gate on this.
//
//  2. status.Mode == "" AND spec.Mode == "live" — explicit-live initial
//     entry to Validating before status.Mode has been stamped.
//
// CRITICAL — reads the passed `status`, NOT mig.Status (Finding 1 fix):
// the Reconcile loop hands each phase handler a DeepCopy of mig.Status
// (controller.go); resolveAutoMode writes the resolved mode to THAT
// copy. mig.Status.Mode is not updated until the copy is persisted at
// the end of the reconcile. An earlier version read mig.Status.Mode
// here, so an auto-resolved-live migration (resolveAutoMode wrote
// status.Mode=live on the copy, mig.Status.Mode still "", spec.Mode
// "auto") returned false → dispatch fell through to offline; auto-mode
// could never reach live. Both the Validating dispatch AND the live
// handlers' entry guards must read the resolved `status` for the
// fix to hold (fixing only the dispatch would move the false-negative
// to the guard). See docs/migration/phase-3b-pr2-walkthrough.md
// Finding 1.
//
// Architect-discipline review answer (Q1) baked in: each handleXxxLive
// asserts isLiveMode(mig, status) at entry. If the assertion fails
// (which would indicate a bug, not a race), it returns phaseFailure
// with FailureReasonOther rather than fall through to offline.
// Default-to-explicit per PR #26.
func isLiveMode(mig *migrationv1alpha1.SwiftMigration, status *migrationv1alpha1.SwiftMigrationStatus) bool {
	if status.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		return true
	}
	if status.Mode == "" && mig.Spec.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		return true
	}
	return false
}
