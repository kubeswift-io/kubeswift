package swiftmigration

import (
	"strings"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// classifyFailureFromDetail maps swiftletd's migration-status-detail
// string to a Phase 3a failureReason per §4.7 failure-mode taxonomy.
//
// The detail strings come from PR #41's D2 watchdog
// (write_migration_failed_on_abnormal_exit), the W1 dispatch-side gate
// (Phase 2 PR-B), and D1 cancel handler. Recognized prefixes/patterns
// are matched against the actual swiftletd output strings AND the
// idealised vocabulary documented in the Phase 3a design doc; both
// shapes route to the same enum.
//
// Pure function: no I/O, no state. Easy to unit test in isolation.
//
// Recognition rules (case-insensitive on the matched substrings):
//
//   - "destination listener exited abnormally" (D2 watchdog actual)
//     OR "abnormal CH exit" / "CH process killed" /
//     "destination not reachable" (idealised vocabulary)
//     → PodTerminated (dst CH process died; the dst pod is in flight
//     of termination or already gone, so the migration target is
//     functionally lost).
//
//   - "w1_violation" prefix (Phase 2 PR-B dispatch-side gate actual)
//     OR "W1 violation" / "destination not running post-receive"
//     (idealised) → Other (migration-internal inconsistency: the wire
//     transfer claimed success but the post-condition probe failed).
//
//   - exact "cancelled" (D1 cancel handler actual) OR
//     "cancelled by operator" (idealised) → Cancelled (operator-
//     initiated termination via spec.cancelRequested=true).
//
//   - default (any unrecognised or empty detail) → Other (preserves
//     the message via failureMessage for operator visibility).
//
// Detail-string parsing is best-effort — Phase 3a's failure-mode
// visibility goal is "good enough"; Phase 3b's audit logging design
// will likely formalise the failure-detail schema with structured
// fields. Until then, prefix matching plus default-Other is sufficient
// for the operator-visible failureReason taxonomy.
func classifyFailureFromDetail(detail string) string {
	d := strings.ToLower(strings.TrimSpace(detail))
	if d == "" {
		return migrationv1alpha1.FailureReasonOther
	}

	// PodTerminated indicators — D2 watchdog actual + idealised set.
	if strings.Contains(d, "destination listener exited abnormally") ||
		strings.Contains(d, "abnormal ch exit") ||
		strings.Contains(d, "ch process killed") ||
		strings.Contains(d, "destination not reachable") {
		return migrationv1alpha1.FailureReasonPodTerminated
	}

	// Cancelled indicators — D1 actual + idealised. Match before W1
	// (exact "cancelled" is a short string; cheaper to test up front).
	if d == "cancelled" || strings.Contains(d, "cancelled by operator") {
		return migrationv1alpha1.FailureReasonCancelled
	}

	// W1 violation — Phase 2 PR-B's dispatch-side gate writes
	// "w1_violation: ..." prefix. Idealised vocabulary uses spaces.
	if strings.HasPrefix(d, "w1_violation") ||
		strings.Contains(d, "w1 violation") ||
		strings.Contains(d, "destination not running post-receive") {
		return migrationv1alpha1.FailureReasonOther
	}

	// Default: unrecognised swiftletd-internal error (e.g.,
	// "send_migration: <CH error>" or "receive_migration: <CH error>"
	// raw category-token output). Map to Other; failureMessage carries
	// the raw detail for operator inspection.
	return migrationv1alpha1.FailureReasonOther
}
