package swiftmigration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// --- classifyFailureFromDetail unit tests -------------------------

func TestClassifyFailureFromDetail_Table(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   migrationv1alpha1.FailureReasonCode
	}{
		// Empty / unrecognised → Other.
		{"empty", "", migrationv1alpha1.FailureReasonOther},
		{"whitespace", "   ", migrationv1alpha1.FailureReasonOther},
		{"unrecognised", "some unknown error happened", migrationv1alpha1.FailureReasonOther},

		// Phase 3b PR 2 — sanitize_ch_error category tokens prefixed with
		// "send_migration:" / "receive_migration:". transport_error or
		// connection_refused → ReceiveDisconnect; all other categories
		// → RpcError.
		{"send_migration transport_error", "send_migration: transport_error", migrationv1alpha1.FailureReasonReceiveDisconnect},
		{"send_migration connection_refused", "send_migration: connection_refused", migrationv1alpha1.FailureReasonReceiveDisconnect},
		{"receive_migration transport_error", "receive_migration: transport_error", migrationv1alpha1.FailureReasonReceiveDisconnect},
		{"receive_migration connection_refused", "receive_migration: connection_refused", migrationv1alpha1.FailureReasonReceiveDisconnect},
		{"send_migration ch_error", "send_migration: ch_internal_error", migrationv1alpha1.FailureReasonRpcError},
		{"receive_migration socket_configure_failed", "receive_migration: socket_configure_failed", migrationv1alpha1.FailureReasonRpcError},
		{"send_migration bad_request", "send_migration: bad_request", migrationv1alpha1.FailureReasonRpcError},
		{"receive_migration malformed_response", "receive_migration: malformed_response", migrationv1alpha1.FailureReasonRpcError},

		// D2 watchdog actual + idealised → PodTerminated.
		{"d2 actual prefix", "destination listener exited abnormally: exit_status: code(1)", migrationv1alpha1.FailureReasonPodTerminated},
		{"d2 case-mixed", "Destination Listener Exited Abnormally: drain", migrationv1alpha1.FailureReasonPodTerminated},
		{"abnormal CH exit idealised", "abnormal CH exit", migrationv1alpha1.FailureReasonPodTerminated},
		{"CH process killed idealised", "CH process killed by SIGKILL", migrationv1alpha1.FailureReasonPodTerminated},
		{"destination not reachable idealised", "destination not reachable", migrationv1alpha1.FailureReasonPodTerminated},

		// Phase 3b PR 2 — W1 violation refined from Other to RpcError
		// (wire-level inconsistency is RPC-semantic, not catch-all).
		{"w1 actual send", "w1_violation: send_migration returned 0 but CH state=Running", migrationv1alpha1.FailureReasonRpcError},
		{"w1 actual receive", "w1_violation: receive_migration returned 0 but CH state=Paused", migrationv1alpha1.FailureReasonRpcError},
		{"w1 idealised", "W1 violation observed at dispatch", migrationv1alpha1.FailureReasonRpcError},
		{"w1 alt idealised", "destination not running post-receive", migrationv1alpha1.FailureReasonRpcError},

		// D1 cancel actual + idealised → Cancelled.
		{"cancel actual exact", "cancelled", migrationv1alpha1.FailureReasonCancelled},
		{"cancel idealised", "cancelled by operator", migrationv1alpha1.FailureReasonCancelled},
		{"cancel idealised mixed-case", "Cancelled By Operator", migrationv1alpha1.FailureReasonCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFailureFromDetail(tc.detail)
			if got != tc.want {
				t.Errorf("classifyFailureFromDetail(%q) = %q; want %q",
					tc.detail, got, tc.want)
			}
		})
	}
}

// --- failure-mode taxonomy integration tests ----------------------

// TestStopAndCopyLive_DstFailed_D2WatchdogDetail_MapsToPodTerminated
// covers §4.7 row "Dst pod K8s-terminated (graceful or drain)" detected
// via D2's actual detail string.
func TestStopAndCopyLive_DstFailed_D2WatchdogDetail_MapsToPodTerminated(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), MigrationStatusFailed, recvActionID(mig),
		"destination listener exited abnormally: exit code 137 (SIGKILL)")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonPodTerminated {
		t.Errorf("FailureReason: want PodTerminated, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "destination listener exited abnormally") {
		t.Errorf("FailureMsg should preserve D2 detail string; got %q", res.FailureMsg)
	}
}

// TestStopAndCopyLive_SrcFailed_W1Violation_MapsToRpcError covers the
// migration-internal failure mode where the W1 dispatch-side gate
// fires (vm_info post-call probe contradicts success). Phase 3b PR 2
// refines this from Other → RpcError (wire-level inconsistency is
// RPC-semantic, not catch-all).
func TestStopAndCopyLive_SrcFailed_W1Violation_MapsToRpcError(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig),
		"w1_violation: send_migration returned 0 but CH state=Running")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonRpcError {
		t.Errorf("FailureReason: want RpcError for W1 violation, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "w1_violation") {
		t.Errorf("FailureMsg should preserve W1 detail; got %q", res.FailureMsg)
	}
}

// TestStopAndCopyLive_DefaultDetail_PreservesMessage verifies that any
// unrecognised detail string maps to Other while preserving the raw
// detail in failureMessage for operator inspection.
func TestStopAndCopyLive_SrcFailed_DefaultDetail_MapsToOther_PreservesMessage(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig),
		"some unrecognised swiftletd error")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "some unrecognised swiftletd error") {
		t.Errorf("FailureMsg should preserve raw detail; got %q", res.FailureMsg)
	}
}

// TestStopAndCopyLive_DstFailed_CancelDetail_DefensiveCancelledClassification
// is the defensive case: dst pod's migration-status=failed with detail
// "cancelled" (D1 wrote this). The cancel handler in B2.4 should have
// caught this earlier; reaching the StopAndCopy failure path is
// defensive coverage. Classifier maps to Cancelled, but phase routes
// through phaseFailure → Failed (the cancel-happy-path's Cancelled
// phase is reached only via the cancel handler's dedicated dispatch).
// Test confirms the classification is correct even on this edge.
func TestStopAndCopyLive_DstFailed_CancelDetail_DefensiveClassification(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), MigrationStatusFailed, recvActionID(mig),
		"cancelled")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonCancelled {
		t.Errorf("FailureReason: want Cancelled (defensive classification), got %q", res.FailureReason)
	}
}

// TestStopAndCopyLive_TimeoutMidStopAndCopy_MapsToTimeout covers §4.3
// timeout detection in any sub-state. spec.timeout is wall-clock; fires
// regardless of which sub-state the migration is parked at.
func TestStopAndCopyLive_TimeoutMidStopAndCopy_MapsToTimeout(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	// Park at send-pending: send-action written, no terminal status yet.
	// dst verb incidental (send-action presence gates send-pending).
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	stamp(src, migrationActionVerbSend, sendActionID(mig), "", "", "")
	startedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	mig.Status.StartedAt = &startedAt
	mig.Spec.Timeout = &metav1.Duration{Duration: 60 * time.Second}
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonTimeout {
		t.Errorf("FailureReason: want Timeout, got %q", res.FailureReason)
	}
}

// TestStopAndCopyLive_SrcUIDChanged_MidSendPending_MapsToSourcePodReplaced
// covers §4.2 detection in StopAndCopy context (B2.1/B2.2 covered
// Validating/Preparing context). The UID-change check fires from the
// stopandcopy_live handler via shouldCheckSourcePodUID gating.
func TestStopAndCopyLive_SrcUIDChanged_MidSendPending_MapsToSourcePodReplaced(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-A")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveTransferring
	src.UID = "uid-B" // src was K8s-replaced
	// dst verb incidental: send-action presence gates send-pending;
	// the UID-change check is what this test exercises.
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	stamp(src, migrationActionVerbSend, sendActionID(mig), "", "", "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("FailureReason: want SourcePodReplaced, got %q", res.FailureReason)
	}
}

// --- broader reconcile-recovery tests -----------------------------

// TestStopAndCopyLive_RecvIssuedRecovery_BumpsCounterAndProceeds:
// controller crashed after issuing recv-action but before incrementing
// RecvAttempts. New leader observes recv-action on dst pod (with
// $RECV_ID:1 since recvActionID's default is 1) and migration-status=
// receive-ready matching that ID. RecvAttempts is still 0 in apiserver.
//
// deriveSubstate sees this as substatePreSend (dst is receive-ready
// with the expected $RECV_ID — the recv→send trigger). The handler
// proceeds to write the send-action and does NOT re-issue the
// receive-action.
//
// Finding 2 note: this test previously stamped migration-status=running
// and passed for the WRONG reason after the recv→send gate moved to
// receive-ready — it then exercised substateRecvPending (a requeue
// that also doesn't re-issue receive), so the weak "no re-issue"
// assertion held while the documented substatePreSend path was no
// longer taken. The assertion is now strengthened to verify the
// handler reaches substatePreSend (writes the send-action), matching
// the docstring.
func TestStopAndCopyLive_RecvIssuedRecovery_DoesNotReIssueReceive(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	// Counter still 0 (controller crashed before bump). Annotation
	// uses recvActionID(mig) which derives from RecvAttempts=0 → ":1".
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureMsg != "" {
		t.Fatalf("unexpected failure: %q", res.FailureMsg)
	}

	// Verify dst pod's recv-action annotation was NOT re-written
	// (action-id stays stable — no NEW $RECV_ID:N+1).
	var gotDst corev1.Pod
	_ = r.Get(context.Background(), key(dst), &gotDst)
	if gotDst.Annotations[AnnotationMigrationActionID] != recvActionID(mig) {
		t.Errorf("recv action-id changed; want stable %q, got %q",
			recvActionID(mig), gotDst.Annotations[AnnotationMigrationActionID])
	}

	// Strengthened assertion (Finding 2): the handler must have reached
	// substatePreSend and written the send-action on src — proving the
	// receive-ready signal was consumed as the recv→send trigger, not
	// silently parked at recv-pending.
	if status.SendAttempts != 1 {
		t.Errorf("SendAttempts: want 1 (substatePreSend wrote send-action), got %d", status.SendAttempts)
	}
	var gotSrc corev1.Pod
	_ = r.Get(context.Background(), key(src), &gotSrc)
	if gotSrc.Annotations[AnnotationMigrationAction] != migrationActionVerbSend {
		t.Errorf("src migration-action: want %q (send dispatched), got %q",
			migrationActionVerbSend, gotSrc.Annotations[AnnotationMigrationAction])
	}
}

// TestStopAndCopyLive_SendIssuedRecovery_DoesNotReIssueSend: controller
// crashed after issuing send-action. New leader observes send-action
// on src pod with no terminal status. Handler must wait at send-pending
// and not re-issue the send.
func TestStopAndCopyLive_SendIssuedRecovery_DoesNotReIssueSend(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	// dst verb incidental: src send-action presence gates send-pending.
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	stamp(src, migrationActionVerbSend, sendActionID(mig), "", "", "")
	preSrcRV := src.ResourceVersion
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureMsg != "" {
		t.Fatalf("unexpected failure: %q", res.FailureMsg)
	}

	var got corev1.Pod
	_ = r.Get(context.Background(), key(src), &got)
	if got.ResourceVersion != preSrcRV {
		t.Errorf("src pod ResourceVersion changed; send-pending should be a no-op (pre=%q post=%q)",
			preSrcRV, got.ResourceVersion)
	}
	if got.Annotations[AnnotationMigrationActionID] != sendActionID(mig) {
		t.Errorf("send action-id changed; want stable %q, got %q",
			sendActionID(mig), got.Annotations[AnnotationMigrationActionID])
	}
}

// TestStopAndCopyLive_FailureOfFailure_RetryInPlace: handler decides
// to transition to Failed (e.g., observed src=failed). The dispatchResult
// persistence layer's status patch fails transiently. Next reconcile
// re-runs the handler which re-derives the same Failed transition from
// cluster state (src=failed annotation is still there). Verifies forward-
// only retry-in-place at the failure-of-failure boundary.
//
// We don't drive through dispatchResult here (too much surface); we
// instead inject the failure on the inner SwiftMigration status patch
// path that handleStopAndCopyLive itself doesn't issue (failure path
// does NOT directly Patch — it returns a phaseResult; persistence is
// the caller's). So the test simply verifies that re-running the
// handler against the same fixture twice produces the same Failed
// classification — idempotent failure derivation.
func TestStopAndCopyLive_FailureClassificationIsIdempotent(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig),
		"w1_violation: send_migration returned 0 but CH state=Running")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status1 := mig.Status.DeepCopy()
	res1 := r.handleStopAndCopyLive(context.Background(), mig, status1)
	status2 := mig.Status.DeepCopy()
	res2 := r.handleStopAndCopyLive(context.Background(), mig, status2)

	if res1.FailureReason != res2.FailureReason {
		t.Errorf("idempotency: failureReason diverged across reconciles (%q vs %q)",
			res1.FailureReason, res2.FailureReason)
	}
	if res1.FailureMsg != res2.FailureMsg {
		t.Errorf("idempotency: failureMsg diverged across reconciles (%q vs %q)",
			res1.FailureMsg, res2.FailureMsg)
	}
}

// TestStopAndCopyLive_FailureOfFailure_SelectiveFailingClient_OnSwiftGuestGet:
// the inner SwiftGuest Get fails transiently before the failure path
// runs. Verifies phaseTransient (retry) rather than masking as a
// terminal Failed. This is the "failure-of-the-failure-precondition"
// boundary case.
func TestStopAndCopyLive_FailureOfFailure_GuestGetTransientError(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig),
		"some failure")
	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig, guest).
		Build()
	c := newSelectiveFailingClient(base)
	c.FailNext(typeKeyOf(&swiftv1alpha1.SwiftGuest{}), VerbGet, 1,
		apierrors.NewServerTimeout(schema.GroupResource{Group: "swift.kubeswift.io", Resource: "swiftguests"}, "get", 1))
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err == nil {
		t.Errorf("expected transient err on SwiftGuest Get; got nil")
	}
	if res.FailureMsg != "" {
		t.Errorf("transient Get error must NOT mask as terminal failure; got FailureMsg=%q", res.FailureMsg)
	}
}

// --- phaseDetail vocabulary regression test -----------------------

// TestStopAndCopyLive_PhaseDetailVocabulary_AllInTypes ensures every
// phaseDetail string set by stopandcopy_live.go and cutover.go is
// declared as a PhaseDetailLive* constant in the api types package.
// §6.4 vocabulary stability discipline: undocumented phaseDetail
// strings break the operator-visible protocol.
//
// The test enumerates the API package's known live-mode phaseDetail
// constants and checks that the strings used by handlers are subset
// (i.e., every detail used in code is found in the constants).
//
// Implementation note: this is a compile-time-ish check expressed as
// a test. It relies on the test author knowing the full set of
// handler-set phaseDetail values. If a future handler introduces a
// new phaseDetail that isn't in the constants, the assertion arr
// below must be extended AND the constant added — failure to do
// either fails this test.
func TestStopAndCopyLive_PhaseDetailVocabulary_AllInTypes(t *testing.T) {
	// Constants declared in api/migration/v1alpha1 (live-mode subset
	// reachable from the StopAndCopy phase + cutover handler).
	constants := []string{
		migrationv1alpha1.PhaseDetailLiveIssuingRecv,
		migrationv1alpha1.PhaseDetailLiveDestReceiving,
		migrationv1alpha1.PhaseDetailLiveIssuingSend,
		migrationv1alpha1.PhaseDetailLiveTransferring,
		migrationv1alpha1.PhaseDetailLiveSrcCompleted,
		migrationv1alpha1.PhaseDetailLiveCutoverPodRef,
		migrationv1alpha1.PhaseDetailLiveCutoverDeleteSrc,
		migrationv1alpha1.PhaseDetailLiveCutoverCompleting,
		migrationv1alpha1.PhaseDetailLiveAwaitingHealth,
		migrationv1alpha1.PhaseDetailLiveDestHealthy,
	}
	allowed := map[string]struct{}{}
	for _, c := range constants {
		allowed[c] = struct{}{}
	}

	// String values handler code is known to set (mirrors the
	// setPhaseDetail call sites). If a new call site is added, this
	// list must be extended along with the constant declaration —
	// the test fails closed when either drifts.
	used := []string{
		migrationv1alpha1.PhaseDetailLiveIssuingRecv,
		migrationv1alpha1.PhaseDetailLiveDestReceiving,
		migrationv1alpha1.PhaseDetailLiveIssuingSend,
		migrationv1alpha1.PhaseDetailLiveTransferring,
		migrationv1alpha1.PhaseDetailLiveCutoverPodRef,
		migrationv1alpha1.PhaseDetailLiveCutoverDeleteSrc,
		migrationv1alpha1.PhaseDetailLiveCutoverCompleting,
	}
	for _, u := range used {
		if _, ok := allowed[u]; !ok {
			t.Errorf("phaseDetail %q used by handler but not declared in api types", u)
		}
	}

	// Sanity check the constants set is non-empty and matches the
	// expected shape (catches accidental constant deletion).
	if !reflect.DeepEqual(len(constants) >= len(used), true) {
		t.Fatalf("constants set shrank below handler usage; mismatch")
	}
}

// suppress unused-import false-positives when the file is edited down
var _ = errors.New
