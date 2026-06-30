package swiftmigration

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// shouldCheckSourcePodUID returns true when the controller should
// compare the source SwiftGuest's current pod UID against
// status.SourcePodUID and fail the migration with
// FailureReasonSourcePodReplaced on mismatch (F4.2 detection).
//
// The check is scoped to PRE-CUTOVER phases only. Once cutover step 1
// has succeeded (PodRefSwapped condition stamped True), the source pod
// has been intentionally retired by step 2 — observing src-pod NotFound
// or a different UID is the EXPECTED post-cutover state, not a
// pod-replacement failure. Continuing the check post-cutover would
// produce false-alarm Failed transitions on successful migrations.
//
// **Triple-gate per architect-discipline review C1**: condition-absence
// alone is not sufficient because cutover step 1's status patch and
// the PodRefSwapped condition write have a brief ordering window. The
// gate combines (phase, phaseDetail, PodRefSwapped) so:
//
//   - Phases Pending / Validating / Preparing → always check.
//   - Phase StopAndCopy → check ONLY when phaseDetail is one of the
//     pre-cutover live substates AND PodRefSwapped is not True.
//     Once phaseDetail crosses into cutover-pod-ref or cutover-delete-
//     src territory, OR PodRefSwapped flips True, the check stops.
//   - Phase Resuming / Completed / Failed / Cancelled → never check.
//
// Phase 1 offline migrations do NOT call this helper. F4.2 detection
// is a live-mode-only concern (offline already drains the source pod
// in Preparing before any cutover step). The helper's StopAndCopy
// branch's substate vocabulary is therefore live-only.
//
// Why a helper rather than inlining in each call site: tests covering
// "post-cutover phases do NOT trigger the check, even if UID changed"
// gate one helper instead of mirroring N call sites. Future code
// changes that accidentally invoke the check from a post-cutover
// handler are caught by the helper's behavior, not by spreading the
// gate logic across handlers.
func shouldCheckSourcePodUID(mig *migrationv1alpha1.SwiftMigration) bool {
	switch mig.Status.Phase {
	case migrationv1alpha1.SwiftMigrationPhasePending,
		migrationv1alpha1.SwiftMigrationPhaseValidating,
		migrationv1alpha1.SwiftMigrationPhasePreparing:
		return true
	case migrationv1alpha1.SwiftMigrationPhaseStopAndCopy:
		if isPostCutover(mig) {
			return false
		}
		return isPreCutoverPhaseDetail(mig.Status.PhaseDetail)
	default:
		// Resuming, Completed, Failed, Cancelled, and any unknown
		// future value: no check. New phases added in Phase 3b+ should
		// extend this switch explicitly rather than relying on
		// default-true; default-to-explicit per PR #26.
		return false
	}
}

// isPostCutover returns true when the cutover commit point has been
// crossed. Used by both shouldCheckSourcePodUID and the cancel
// authorization logic (cancel is authorized only pre-cutover). The
// signal is the PodRefSwapped condition: it lands at the same patch
// as SwiftGuest.status.podRef.name (per Q3.3 mitigation, derived from
// cluster state on every cutover-substate reconcile entry, so write-
// timing-window false-alarms are not possible).
func isPostCutover(mig *migrationv1alpha1.SwiftMigration) bool {
	for _, c := range mig.Status.Conditions {
		if c.Type == migrationv1alpha1.SwiftMigrationConditionPodRefSwapped &&
			c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

// isPreCutoverPhaseDetail returns true when the phaseDetail value is
// one of the live-mode pre-cutover substate strings. The vocabulary
// is closed (defined in api types.go's PhaseDetailLive* constants)
// per the §6.4 stability discipline; new substates added in future
// phases must extend this set explicitly.
//
// Phase 1 offline mode does not call shouldCheckSourcePodUID at all
// (offline's pod-replacement concerns are handled in Preparing's drain
// loop), so the live-only vocabulary here is correct.
func isPreCutoverPhaseDetail(detail string) bool {
	switch detail {
	case migrationv1alpha1.PhaseDetailLiveIssuingRecv,
		migrationv1alpha1.PhaseDetailLiveDestReceiving,
		migrationv1alpha1.PhaseDetailLiveIssuingSend,
		migrationv1alpha1.PhaseDetailLiveTransferring,
		migrationv1alpha1.PhaseDetailLiveSrcCompleted:
		return true
	default:
		return false
	}
}
