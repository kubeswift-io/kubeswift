package swiftmigration

import (
	"strings"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

// classifyFailureFromDetail maps swiftletd's migration-status-detail
// string to a failureReason per the Phase 3a §4.7 / Phase 3b PR 2
// taxonomy. Phase 3a seeded the helper; Phase 3b PR 2 extends it to
// recognise the snake_case category tokens emitted by
// sanitize_ch_error and to route them into the new ReceiveDisconnect /
// RpcError codes.
//
// The detail strings come from multiple swiftletd sites:
//   - sanitize_ch_error (rust/swiftletd/src/action.rs:1285) emits
//     snake_case category tokens prefixed with "send_migration:" or
//     "receive_migration:" — e.g. "send_migration: transport_error",
//     "receive_migration: connection_refused".
//   - D2 watchdog (write_migration_failed_on_abnormal_exit) emits
//     "destination listener exited abnormally".
//   - W1 dispatch-side gate (Phase 2 PR-B) emits the "w1_violation:"
//     prefix.
//   - D1 cancel handler emits exact "cancelled".
//
// Recognition rules (case-insensitive on the matched substrings):
//
//   - "destination listener exited abnormally" (D2 watchdog actual)
//     OR "abnormal CH exit" / "CH process killed" /
//     "destination not reachable" (idealised vocabulary)
//     → PodTerminated (dst CH process died; the dst pod is in flight
//     of termination or already gone).
//
//   - exact "cancelled" (D1 cancel handler actual) OR
//     "cancelled by operator" (idealised) → Cancelled.
//
//   - "send_migration:" or "receive_migration:" prefix followed by
//     "transport_error" or "connection_refused" → ReceiveDisconnect
//     (peer dropped mid-RPC; the name reflects the event regardless
//     of side that observed it — see the FailureReasonReceiveDisconnect
//     constant docstring for the operator-visible naming asymmetry).
//
//   - any other "send_migration:" / "receive_migration:" prefix
//     (bad_request, internal_server_error, ch_status_error,
//     malformed_response, socket_configure_failed, ch_error
//     catch-all) → RpcError.
//
//   - "w1_violation" prefix (Phase 2 PR-B dispatch-side gate actual)
//     OR "W1 violation" / "destination not running post-receive"
//     (idealised) → RpcError (refined from Phase 3a's Other —
//     wire-level inconsistency between RPC return and CH state is
//     RPC-semantic).
//
//   - default (any unrecognised or empty detail) → Other (preserves
//     the message via failureMessage for operator visibility).
//
// Pure function: no I/O, no state. Easy to unit test in isolation.
//
// Phase 3b PR 2 leaves DstScheduleFailed / DstNeverReady /
// EligibilityMismatch / ImageTagMismatch / DstPodConflict outside
// this classifier — they are controller-side stamps, not swiftletd-
// reported failures.
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
	// "w1_violation: ..." prefix. Phase 3b PR 2 refines from Other
	// to RpcError: a wire transfer that reports success but leaves CH
	// in an inconsistent post-state is RPC-semantic, not a generic
	// catch-all. Idealised vocabulary uses spaces.
	if strings.HasPrefix(d, "w1_violation") ||
		strings.Contains(d, "w1 violation") ||
		strings.Contains(d, "destination not running post-receive") {
		return migrationv1alpha1.FailureReasonRpcError
	}

	// Phase 3b PR 2 — sanitize_ch_error category tokens prefixed
	// with "send_migration:" or "receive_migration:". Two specific
	// categories (transport_error, connection_refused) indicate
	// peer/network disconnect mid-RPC → ReceiveDisconnect; all other
	// categories under these prefixes → RpcError.
	if strings.HasPrefix(d, "send_migration:") ||
		strings.HasPrefix(d, "receive_migration:") {
		if strings.Contains(d, "transport_error") ||
			strings.Contains(d, "connection_refused") {
			return migrationv1alpha1.FailureReasonReceiveDisconnect
		}
		return migrationv1alpha1.FailureReasonRpcError
	}

	// Default: unrecognised swiftletd-internal error not matching any
	// known prefix. Map to Other; failureMessage carries the raw
	// detail for operator inspection.
	return migrationv1alpha1.FailureReasonOther
}
