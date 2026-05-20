package swiftmigration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// stopAndCopyFixture builds a SwiftMigration in mode=live + StopAndCopy
// phase, the SwiftGuest, src pod (Running with srcUID), and dst pod
// (Running with PodIP set so pre-send can derive target_url). Tests
// adjust per-scenario.
func stopAndCopyFixture(t *testing.T, srcUID types.UID) (*migrationv1alpha1.SwiftMigration, *swiftv1alpha1.SwiftGuest, *corev1.Pod, *corev1.Pod) {
	t.Helper()
	mig := newMigrationWithUID("m1", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Target.NodeName = "miles"
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy
	mig.Status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Status.SourcePodUID = srcUID
	// W26: src pod name locked at Validating; mirror that in the
	// fixture so srcPodLookupName resolves to the original src pod
	// regardless of guest.Status.PodRef drift across cutover/race.
	mig.Status.SourcePodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: "guest"}
	startedAt := metav1.NewTime(time.Now().Add(-30 * time.Second))
	mig.Status.StartedAt = &startedAt

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	src := templateSrcPod("guest", "default")
	src.UID = srcUID
	src.Annotations = map[string]string{
		AnnotationGuestIP: "10.0.0.5",
	}
	dst := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "guest-mig-abcdef",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "miles"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.244.1.42",
		},
	}
	return mig, guest, src, dst
}

// stamp is a small helper for setting action/status annotations
// concisely in tests.
func stamp(pod *corev1.Pod, action, actionID, status, statusID, detail string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	if action != "" {
		pod.Annotations[AnnotationMigrationAction] = action
		pod.Annotations[AnnotationMigrationActionID] = actionID
	}
	if status != "" {
		pod.Annotations[AnnotationMigrationStatus] = status
		pod.Annotations[AnnotationMigrationStatusID] = statusID
	}
	if detail != "" {
		pod.Annotations[AnnotationMigrationStatusDtl] = detail
	}
}

// --- deriveSubstate unit tests ------------------------------------

func TestDeriveSubstate_NothingObserved_ReturnsPreRecv(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	if got := deriveSubstate(mig, src, dst); got != substatePreRecv {
		t.Errorf("substate: want pre-recv, got %v", got)
	}
}

func TestDeriveSubstate_RecvActionWritten_ReturnsRecvPending(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), "", "", "")
	if got := deriveSubstate(mig, src, dst); got != substateRecvPending {
		t.Errorf("substate: want recv-pending, got %v", got)
	}
}

func TestDeriveSubstate_DstRunning_ReturnsPreSend(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "")
	if got := deriveSubstate(mig, src, dst); got != substatePreSend {
		t.Errorf("substate: want pre-send, got %v", got)
	}
}

func TestDeriveSubstate_SendActionWritten_ReturnsSendPending(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "")
	stamp(src, migrationActionVerbSend, sendActionID(mig), "", "", "")
	if got := deriveSubstate(mig, src, dst); got != substateSendPending {
		t.Errorf("substate: want send-pending, got %v", got)
	}
}

func TestDeriveSubstate_SrcComplete_ReturnsSrcCompleted(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "ok")
	if got := deriveSubstate(mig, src, dst); got != substateSrcCompleted {
		t.Errorf("substate: want src-completed, got %v", got)
	}
}

func TestDeriveSubstate_SrcFailed_ReturnsSrcFailed(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig), "ch error")
	if got := deriveSubstate(mig, src, dst); got != substateSrcFailed {
		t.Errorf("substate: want src-failed, got %v", got)
	}
}

func TestDeriveSubstate_DstFailed_ReturnsDstFailed(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), MigrationStatusFailed, recvActionID(mig), "watchdog")
	if got := deriveSubstate(mig, src, dst); got != substateDstFailed {
		t.Errorf("substate: want dst-failed, got %v", got)
	}
}

func TestDeriveSubstate_SrcPriorityOverDst(t *testing.T) {
	// Once src is in terminal state, dst-side observations are stale.
	// Verify src-side check takes priority.
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "ok")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "")
	if got := deriveSubstate(mig, src, dst); got != substateSrcCompleted {
		t.Errorf("src-complete must override dst-running; want src-completed, got %v", got)
	}
}

func TestDeriveSubstate_StaleActionID_IgnoredAsNoMatch(t *testing.T) {
	// Annotations from a previous SwiftMigration's $RECV_ID don't
	// match this migration's recvActionID. Treat as pre-recv (fresh
	// entry).
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(dst, migrationActionVerbReceive, "previous-mig:recv:1", migrationStatusRunning, "previous-mig:recv:1", "")
	if got := deriveSubstate(mig, src, dst); got != substatePreRecv {
		t.Errorf("stale action-id must not match; want pre-recv, got %v", got)
	}
}

// --- recvActionID / sendActionID unit tests -----------------------

func TestRecvActionID_DefaultsToOne(t *testing.T) {
	mig := newMigration("m1", "default")
	if got := recvActionID(mig); got != "m1:recv:1" {
		t.Errorf("recvActionID with counter=0: want m1:recv:1, got %q", got)
	}
}

func TestRecvActionID_Stable_AfterIncrement(t *testing.T) {
	mig := newMigration("m1", "default")
	mig.Status.RecvAttempts = 1
	if got := recvActionID(mig); got != "m1:recv:1" {
		t.Errorf("recvActionID with counter=1: want m1:recv:1, got %q", got)
	}
}

func TestSendActionID_DefaultsToOne(t *testing.T) {
	mig := newMigration("m1", "default")
	if got := sendActionID(mig); got != "m1:send:1" {
		t.Errorf("sendActionID with counter=0: want m1:send:1, got %q", got)
	}
}

// --- handler integration tests ------------------------------------

func newStopAndCopyReconciler(t *testing.T, objs ...interface{}) *SwiftMigrationReconciler {
	t.Helper()
	scheme := testScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		switch v := o.(type) {
		case *migrationv1alpha1.SwiftMigration:
			builder = builder.WithObjects(v).WithStatusSubresource(v)
		case *swiftv1alpha1.SwiftGuest:
			builder = builder.WithObjects(v).WithStatusSubresource(v)
		case *corev1.Pod:
			builder = builder.WithObjects(v)
		default:
			t.Fatalf("unsupported fixture type %T", o)
		}
	}
	c := builder.Build()
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}
}

func TestStopAndCopyLive_PreRecv_WritesReceiveAction(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	if res.Requeue == 0 {
		t.Errorf("expected requeue while waiting for ack")
	}
	if status.RecvAttempts != 1 {
		t.Errorf("RecvAttempts: want 1, got %d", status.RecvAttempts)
	}
	if status.PhaseDetail != migrationv1alpha1.PhaseDetailLiveIssuingRecv {
		t.Errorf("phaseDetail: want IssuingRecv, got %q", status.PhaseDetail)
	}

	// Verify receive-action was written on dst pod.
	var got corev1.Pod
	if err := r.Get(context.Background(), key(dst), &got); err != nil {
		t.Fatalf("re-get dst: %v", err)
	}
	if got.Annotations[AnnotationMigrationAction] != migrationActionVerbReceive {
		t.Errorf("dst migration-action: want %q, got %q", migrationActionVerbReceive, got.Annotations[AnnotationMigrationAction])
	}
	if got.Annotations[AnnotationMigrationActionID] != "m1:recv:1" {
		t.Errorf("dst migration-action-id: want m1:recv:1, got %q", got.Annotations[AnnotationMigrationActionID])
	}
	// Args JSON should include the listen URL and guest IP from src.
	var args migrationReceiveArgs
	if err := json.Unmarshal([]byte(got.Annotations[AnnotationMigrationActionArgs]), &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args.ListenURL != "tcp:0.0.0.0:6789" {
		t.Errorf("listen_url: want tcp:0.0.0.0:6789, got %q", args.ListenURL)
	}
	if args.GuestIP != "10.0.0.5" {
		t.Errorf("guest_ip: want 10.0.0.5, got %q", args.GuestIP)
	}
}

// W13 regression: substatePreRecv must patch src pod with BOTH the
// migration-name label (informer observability per architect F-3) AND
// the Phase 2 plaintext-ack annotation. Without the ack, swiftletd's
// decide() rejects the send-action with status=rejected and the
// migration stalls at substateSendPending until spec.timeout.
//
// Cluster walkthrough surfaced this against PR 1's merged image
// (sha-4cde589): src pod's migration-status annotation went to
// "rejected" with detail "phase2_plaintext_ack_missing"; controller
// stayed at substateSendPending; spec.timeout fired ~5min later.
func TestStopAndCopyLive_PreRecv_PatchesSrcPodWithLabelAndAck_W13(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	// Confirm src pod starts WITHOUT the ack — fixture realism.
	if src.Annotations[AnnotationMigrationPhase2Ack] != "" {
		t.Fatalf("fixture invalid: src pod should not pre-set ack")
	}
	if src.Labels[LabelMigrationName] != "" {
		t.Fatalf("fixture invalid: src pod should not pre-set migration label")
	}

	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}

	var got corev1.Pod
	if err := r.Get(context.Background(), key(src), &got); err != nil {
		t.Fatalf("re-get src: %v", err)
	}
	if got.Labels[LabelMigrationName] != mig.Name {
		t.Errorf("src migration-name label: want %q, got %q",
			mig.Name, got.Labels[LabelMigrationName])
	}
	if got.Annotations[AnnotationMigrationPhase2Ack] != AnnotationMigrationPhase2AckValue {
		t.Errorf("src phase2-ack annotation: want %q, got %q",
			AnnotationMigrationPhase2AckValue,
			got.Annotations[AnnotationMigrationPhase2Ack])
	}
}

// W18 (PR #46 Scenario 4): when dst pod is being K8s-terminated mid-
// StopAndCopy, src CH errors with generic detail. Without a dst-state
// check, classifyFailureFromDetail defaults to Other; §4.7 says
// PodTerminated. The fix overrides classification when
// dst.DeletionTimestamp is set.
func TestStopAndCopyLive_SrcFailed_DstTerminating_MapsToPodTerminated_W18(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig),
		MigrationStatusFailed, sendActionID(mig),
		"send_migration: internal_server_error")
	// dst pod is K8s-terminating: DeletionTimestamp set.
	now := metav1.Now()
	dst.DeletionTimestamp = &now
	dst.Finalizers = []string{"kubernetes.io/grace-period"} // required for fake client to honor DeletionTimestamp
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonPodTerminated {
		t.Errorf("FailureReason: want PodTerminated (W18 override), got %q",
			res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "source reported migration failure") {
		t.Errorf("FailureMsg should still describe src-failed; got %q", res.FailureMsg)
	}
	if !strings.Contains(res.FailureMsg, "send_migration: internal_server_error") {
		t.Errorf("FailureMsg should preserve src detail; got %q", res.FailureMsg)
	}
}

// W18 negative case: when dst is healthy (no DeletionTimestamp), the
// classifier output flows through unchanged (no PodTerminated
// override). Phase 3b PR 2 reclassifies "send_migration:
// internal_server_error" from Other → RpcError; the W18 test continues
// to assert that the classifier output is preserved (not overridden to
// PodTerminated), now matching the refined RpcError taxonomy.
func TestStopAndCopyLive_SrcFailed_DstHealthy_PreservesClassifier_W18(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig),
		MigrationStatusFailed, sendActionID(mig),
		"send_migration: internal_server_error")
	// dst pod healthy — no DeletionTimestamp.
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonRpcError {
		t.Errorf("FailureReason: want RpcError (W18 preserves classifier when dst healthy; PR 2 refined send_migration: internal_server_error from Other → RpcError), got %q",
			res.FailureReason)
	}
}

// W14: deriveSubstate must recognise migration-status=rejected on src
// and dst as terminal sub-states. Without this, swiftletd's decide()
// rejection (e.g., missing phase2 ack, action-id mismatch) leaves the
// migration parked at substateSendPending/substateRecvPending until
// spec.timeout — operators see Timeout when the real cause is the
// rejection detail.
func TestDeriveSubstate_SrcRejected_W14(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(src, migrationActionVerbSend, sendActionID(mig),
		MigrationStatusRejected, sendActionID(mig),
		"phase2_plaintext_ack_missing")
	if got := deriveSubstate(mig, src, dst); got != substateSrcRejected {
		t.Errorf("substate: want src-rejected, got %v", got)
	}
}

func TestDeriveSubstate_DstRejected_W14(t *testing.T) {
	mig, _, src, dst := stopAndCopyFixture(t, "uid-1")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig),
		MigrationStatusRejected, recvActionID(mig),
		"phase2_plaintext_ack_missing")
	if got := deriveSubstate(mig, src, dst); got != substateDstRejected {
		t.Errorf("substate: want dst-rejected, got %v", got)
	}
}

func TestStopAndCopyLive_SrcRejected_FailsWithOther_W14(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig),
		MigrationStatusRejected, sendActionID(mig),
		"phase2_plaintext_ack_missing")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "source rejected migration action") {
		t.Errorf("FailureMsg: want src-rejected message, got %q", res.FailureMsg)
	}
	if !strings.Contains(res.FailureMsg, "phase2_plaintext_ack_missing") {
		t.Errorf("FailureMsg should preserve rejection detail; got %q", res.FailureMsg)
	}
}

func TestStopAndCopyLive_DstRejected_FailsWithOther_W14(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig),
		MigrationStatusRejected, recvActionID(mig),
		"phase2_plaintext_ack_missing")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "destination rejected migration action") {
		t.Errorf("FailureMsg: want dst-rejected message, got %q", res.FailureMsg)
	}
	if !strings.Contains(res.FailureMsg, "phase2_plaintext_ack_missing") {
		t.Errorf("FailureMsg should preserve rejection detail; got %q", res.FailureMsg)
	}
}

// W13 idempotency: re-running the handler when src pod already has
// the label + ack must not re-patch (verified via ResourceVersion
// stability across reconciles).
func TestStopAndCopyLive_PreRecv_W13Idempotent(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	if src.Labels == nil {
		src.Labels = map[string]string{}
	}
	if src.Annotations == nil {
		src.Annotations = map[string]string{}
	}
	src.Labels[LabelMigrationName] = mig.Name
	src.Annotations[AnnotationMigrationPhase2Ack] = AnnotationMigrationPhase2AckValue

	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	var pre corev1.Pod
	_ = r.Get(context.Background(), key(src), &pre)
	preRV := pre.ResourceVersion

	status := mig.Status.DeepCopy()
	_ = r.handleStopAndCopyLive(context.Background(), mig, status)

	var post corev1.Pod
	_ = r.Get(context.Background(), key(src), &post)
	if post.ResourceVersion != preRV {
		t.Errorf("src pod ResourceVersion changed; W13 patch should be a no-op when label+ack already present (pre=%q post=%q)",
			preRV, post.ResourceVersion)
	}
}

func TestStopAndCopyLive_RecvPending_RequeuesWithDetail(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), "", "", "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.Advanced {
		t.Errorf("expected requeue; got %+v", res)
	}
	if status.PhaseDetail != migrationv1alpha1.PhaseDetailLiveIssuingRecv {
		t.Errorf("phaseDetail unchanged in recv-pending; got %q", status.PhaseDetail)
	}
}

func TestStopAndCopyLive_PreSend_WritesSendAction(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	// dst accepted: migration-status=running with matching recv-id
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	if status.SendAttempts != 1 {
		t.Errorf("SendAttempts: want 1, got %d", status.SendAttempts)
	}
	if status.PhaseDetail != migrationv1alpha1.PhaseDetailLiveIssuingSend {
		t.Errorf("phaseDetail: want IssuingSend, got %q", status.PhaseDetail)
	}

	// Verify send-action on src pod with target_url derived from
	// dst.Status.PodIP.
	var got corev1.Pod
	if err := r.Get(context.Background(), key(src), &got); err != nil {
		t.Fatalf("re-get src: %v", err)
	}
	if got.Annotations[AnnotationMigrationAction] != migrationActionVerbSend {
		t.Errorf("src migration-action: want %q, got %q", migrationActionVerbSend, got.Annotations[AnnotationMigrationAction])
	}
	if got.Annotations[AnnotationMigrationActionID] != "m1:send:1" {
		t.Errorf("src migration-action-id: want m1:send:1, got %q", got.Annotations[AnnotationMigrationActionID])
	}
	var args migrationSendArgs
	if err := json.Unmarshal([]byte(got.Annotations[AnnotationMigrationActionArgs]), &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args.TargetURL != "tcp:10.244.1.42:6789" {
		t.Errorf("target_url: want tcp:10.244.1.42:6789, got %q", args.TargetURL)
	}
}

func TestStopAndCopyLive_SendPending_RequeuesWithDetail(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "")
	stamp(src, migrationActionVerbSend, sendActionID(mig), "", "", "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.Advanced {
		t.Errorf("expected requeue; got %+v", res)
	}
	if status.PhaseDetail != migrationv1alpha1.PhaseDetailLiveTransferring {
		t.Errorf("phaseDetail: want Transferring, got %q", status.PhaseDetail)
	}
}

func TestStopAndCopyLive_SrcCompleted_DispatchesCutoverStep1(t *testing.T) {
	// B3.2: when src reports complete with matching $SEND_ID (W1 gate
	// satisfied), handler dispatches into the 3-step cutover sequence.
	// First reconcile executes step 1 (SwiftGuest podRef.name patch +
	// cutoverStep1At timestamp). Phase remains StopAndCopy; phaseDetail
	// transitions through PodRef → DeleteSrc as the in-memory
	// status updates.
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "ok")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	if res.Advanced {
		t.Errorf("must NOT advance phase mid-cutover; step 1 just landed")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy {
		t.Errorf("phase must remain StopAndCopy until step 3; got %q", status.Phase)
	}
	if status.CutoverStep1At == nil {
		t.Errorf("CutoverStep1At not stamped after step 1")
	}

	// Verify SwiftGuest podRef.name was patched.
	var got swiftv1alpha1.SwiftGuest
	if err := r.Get(context.Background(), client.ObjectKey{Name: guest.Name, Namespace: guest.Namespace}, &got); err != nil {
		t.Fatalf("re-get guest: %v", err)
	}
	if got.Status.PodRef == nil || got.Status.PodRef.Name != "guest-mig-abcdef" {
		t.Errorf("SwiftGuest.status.podRef.name: want guest-mig-abcdef, got %+v", got.Status.PodRef)
	}
}

func TestStopAndCopyLive_DstFailed_ChInternalError_FailsWithRpcError(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	// Phase 3b PR 2: "receive_migration:" prefix + non-transport
	// category → RpcError (was Other in Phase 3a).
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), MigrationStatusFailed, recvActionID(mig), "receive_migration: ch_internal_error")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonRpcError {
		t.Errorf("FailureReason: want RpcError for receive_migration: non-transport category, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "destination reported migration failure") {
		t.Errorf("FailureMsg: want dst-failed message, got %q", res.FailureMsg)
	}
	if !strings.Contains(res.FailureMsg, "ch_internal_error") {
		t.Errorf("FailureMsg should preserve raw detail; got %q", res.FailureMsg)
	}
}

func TestStopAndCopyLive_SrcFailed_FailsWithOther(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig), "ch send error: connection refused")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "source reported migration failure") {
		t.Errorf("FailureMsg: want src-failed message, got %q", res.FailureMsg)
	}
}

func TestStopAndCopyLive_TimeoutExceeded_FailsWithTimeout(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
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

func TestStopAndCopyLive_DstPodMissing_FailsWithPodTerminated(t *testing.T) {
	mig, guest, src, _ := stopAndCopyFixture(t, "uid-1")
	r := newStopAndCopyReconciler(t, mig, guest, src) // no dst pod

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonPodTerminated {
		t.Errorf("FailureReason: want PodTerminated, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "disappeared during StopAndCopy") {
		t.Errorf("FailureMsg: got %q", res.FailureMsg)
	}
}

func TestStopAndCopyLive_SrcPodReplaced_FailsWithSourcePodReplaced(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-A")
	// phaseDetail must be set to a pre-cutover live value for
	// shouldCheckSourcePodUID's StopAndCopy branch to gate on (B1
	// helper). Steady-state: first reconcile sets phaseDetail; UID
	// check fires on subsequent reconciles.
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveIssuingRecv
	src.UID = "uid-B" // src was K8s-recreated mid-StopAndCopy
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("FailureReason: want SourcePodReplaced, got %q", res.FailureReason)
	}
}

func TestStopAndCopyLive_SrcPodGone_FailsWithSourcePodReplaced(t *testing.T) {
	mig, guest, _, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveIssuingRecv
	r := newStopAndCopyReconciler(t, mig, guest, dst) // no src pod

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("FailureReason: want SourcePodReplaced, got %q", res.FailureReason)
	}
}

// W15 regression: when SwiftGuest.status.podRef.name has been patched
// to the dst pod (post-cutoverStep1), the controller must NOT use
// canonicalPodName to resolve src — that would fetch the dst pod and
// false-positive the UID check.
//
// Cluster walkthrough surfaced this against PR 1 + W13/W14 hotfix
// image (sha-ce68fa9): cutoverStep1 issues two separate apiserver
// writes (SwiftGuest podRef + SwiftMigration status); the SwiftGuest
// patch fires the SwiftGuest informer watch, triggering a race-
// reconcile that reads stale phaseDetail (LiveSrcCompleted = pre-
// cutover) and a fresh guest with podRef = dst. canonicalPodName
// resolves to dst, the Get fetches the dst pod, and the UID check
// false-fires SourcePodReplaced.
//
// Fix: src-pod fetch uses guest.Name (the original src pod name)
// regardless of canonicalPodName state. The UID check is specifically
// about the original src pod, not the canonical launcher pod.
func TestStopAndCopyLive_PostCutoverStep1_RaceReconcile_DoesNotFalseFireUIDCheck_W15(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-A")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	// Race state: cutoverStep1 has patched SwiftGuest podRef but the
	// SwiftMigration status patch hasn't persisted yet. Phase still
	// StopAndCopy, phaseDetail still LiveSrcCompleted (pre-cutover
	// vocabulary; shouldCheckSourcePodUID returns true).
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveSrcCompleted
	guest.Status.PodRef = &corev1.ObjectReference{
		Name:      "guest-mig-abcdef", // matches dst pod name
		Namespace: guest.Namespace,
	}
	// src pod still exists with original UID (cutoverStep2 hasn't run);
	// dst pod has different UID. canonicalPodName would resolve to
	// dst-name and fetch dst (UID mismatch → false-fire). guest.Name
	// resolves to src-name and fetches src (UID match → no fire).
	src.UID = "uid-A"
	dst.UID = "uid-DST" // distinct from src
	stamp(src, migrationActionVerbSend, sendActionID(mig),
		migrationStatusComplete, sendActionID(mig), "ok")

	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("UID check false-fired SourcePodReplaced; W15 regression. msg=%q",
			res.FailureMsg)
	}
	// Healthy outcome: cutover-in-progress short-circuit fires;
	// executeCutover dispatches step 2 (delete src) since podRef-done
	// + cutoverStep1At unset means cutoverStep1Timestamp recovery path,
	// or step 2 if CutoverStep1At was set via fixture. Either way,
	// no SourcePodReplaced.
}

// W26 regression: chain migration. After a prior migration's cutover,
// SwiftGuest.status.podRef.Name points at the prior dst pod (= the new
// migration's src). Pre-W26 the StopAndCopy UID-check resolved src by
// literal guest.Name → NotFound → false-fired SourcePodReplaced on a
// perfectly healthy chain migration. The W26 fix locks the src pod
// name at Validating-live (status.SourcePodRef.Name) so resolution is
// chain-safe.
//
// Discovered on cluster during Phase 3a PR 1 disk-boot E12 validation
// (S1 run 2, miles→boba→miles). Same code path runs for kernel-boot
// and disk-boot — workload-class-independent bug; disk-boot validation
// happened to exercise back-to-back migrations and surface it.
func TestStopAndCopyLive_ChainMigration_SrcResolvesToPriorDstPod_NoFalseFire_W26(t *testing.T) {
	mig, guest, _, dst := stopAndCopyFixture(t, "uid-PRIOR-DST")
	// Chain state: SwiftGuest's canonical pod is now the prior
	// migration's dst-suffix pod. status.SourcePodRef.Name was locked
	// in at Validating-live to that same name (the "src" for THIS
	// migration).
	priorDstPodName := "guest-mig-firstrun"
	mig.Status.SourcePodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: priorDstPodName}
	guest.Status.PodRef = &corev1.ObjectReference{Name: priorDstPodName, Namespace: guest.Namespace}
	// The actual src pod for THIS migration has the prior-dst-suffix
	// name. Literal guest.Name lookup ("guest") would NotFound; fix
	// resolves via locked-in name.
	chainSrc := templateSrcPod(priorDstPodName, "default")
	chainSrc.UID = "uid-PRIOR-DST"
	chainSrc.Annotations = map[string]string{AnnotationGuestIP: "10.0.0.5"}
	r := newStopAndCopyReconciler(t, mig, guest, chainSrc, dst)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("UID check false-fired SourcePodReplaced; W26 chain regression. msg=%q",
			res.FailureMsg)
	}
}

// W26 first-migration smoke: nothing fancy here, but the W26 fix
// hinges on srcPodLookupName falling back to canonicalPodNameForGuest
// when status.SourcePodRef is unset. Regress against accidental
// removal of the fallback.
func TestStopAndCopyLive_FirstMigration_SrcResolvesToGuestName_W26Fallback(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	// Simulate a Validating-live that didn't run yet (e.g., recovery
	// reconcile or test scenario): SourcePodRef unset. Fallback should
	// land on canonicalPodNameForGuest = guest.Name (no prior podRef).
	mig.Status.SourcePodRef = nil
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason == migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("fallback path false-fired SourcePodReplaced; msg=%q", res.FailureMsg)
	}
}

func TestStopAndCopyLive_DefensiveGuard_NotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	res := r.handleStopAndCopyLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(res.FailureMsg, "without live mode") {
		t.Errorf("guard message: got %q", res.FailureMsg)
	}
}

func TestStopAndCopyLive_LeaderHandoverMidRecvIssued_ReentrantNoDuplicateWrite(t *testing.T) {
	// Simulates leader handover: counter=1 already (previous leader
	// incremented), annotation already on dst (previous leader wrote).
	// New leader's reconcile must derive recv-pending and NOT
	// re-write the annotation.
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), "", "", "")

	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	preDstRV := dst.ResourceVersion
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureMsg != "" {
		t.Fatalf("unexpected failure: %q", res.FailureMsg)
	}

	var got corev1.Pod
	_ = r.Get(context.Background(), key(dst), &got)
	if got.ResourceVersion != preDstRV {
		t.Errorf("dst pod ResourceVersion changed; reconcile should be a no-op (pre=%q post=%q)", preDstRV, got.ResourceVersion)
	}
}

// W27b + Phase 3b PR 1 Commit E: stampTransferDuration (renamed from
// stampObservedPauseWindow) reads the swiftletd-on-src reported
// pause window from the src pod's
// kubeswift.io/migration-pause-window-ms annotation and DUAL-WRITES
// both status.ObservedTransferDuration (canonical) and
// status.ObservedPauseWindow (deprecated alias). Verifies the
// happy-path dual-write.
func TestStampTransferDuration_HappyPath_DualWrite(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{}
	status := &migrationv1alpha1.SwiftMigrationStatus{}
	src := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnotationMigrationPauseWindowMs: "312"},
		},
	}
	stampTransferDuration(context.Background(), mig, status, src)
	// Canonical field
	if status.ObservedTransferDuration == nil {
		t.Fatalf("ObservedTransferDuration not stamped")
	}
	if got := status.ObservedTransferDuration.Duration; got != 312*time.Millisecond {
		t.Errorf("ObservedTransferDuration: want 312ms, got %v", got)
	}
	// Deprecated alias (Phase 3b release ships both; alias dropped in
	// Phase 3b+1)
	if status.ObservedPauseWindow == nil {
		t.Fatalf("ObservedPauseWindow (deprecated alias) not stamped")
	}
	if got := status.ObservedPauseWindow.Duration; got != 312*time.Millisecond {
		t.Errorf("ObservedPauseWindow alias: want 312ms, got %v", got)
	}
	// Both fields read from the same source value, so they must
	// match. If a future refactor breaks the dual-write coupling
	// (e.g., introduces a second source for one field), this
	// assertion fires.
	if status.ObservedTransferDuration.Duration != status.ObservedPauseWindow.Duration {
		t.Errorf("dual-write fields diverged: canonical=%v alias=%v",
			status.ObservedTransferDuration.Duration,
			status.ObservedPauseWindow.Duration)
	}
}

// Empirical-baseline assertion: the Phase 3b spike Q2 baseline
// observation was 38.20s for a 4Gi guest with no stress. Operators
// reading status.observedTransferDuration on a typical cluster
// should see a value in this range; this test pins the field's
// Duration parsing against the empirical value the operator
// documentation cites.
func TestStampTransferDuration_SpikeQ2BaselineValue(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{}
	status := &migrationv1alpha1.SwiftMigrationStatus{}
	src := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnotationMigrationPauseWindowMs: "38200"},
		},
	}
	stampTransferDuration(context.Background(), mig, status, src)
	want := 38200 * time.Millisecond
	if status.ObservedTransferDuration == nil || status.ObservedTransferDuration.Duration != want {
		t.Errorf("ObservedTransferDuration: want %v (spike Q2 baseline), got %v",
			want, status.ObservedTransferDuration)
	}
	if status.ObservedPauseWindow == nil || status.ObservedPauseWindow.Duration != want {
		t.Errorf("ObservedPauseWindow alias: want %v, got %v",
			want, status.ObservedPauseWindow)
	}
}

// W27b defensive: malformed annotation leaves BOTH fields nil
// (operators see a missing field, never a wrong one). Same posture
// as W27a's defensive nil-check on missing CutoverStep2DispatchedAt.
func TestStampTransferDuration_ParseFailure_LeavesBothFieldsNil(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{}
	status := &migrationv1alpha1.SwiftMigrationStatus{}
	src := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{AnnotationMigrationPauseWindowMs: "garbage"},
		},
	}
	stampTransferDuration(context.Background(), mig, status, src)
	if status.ObservedTransferDuration != nil {
		t.Errorf("ObservedTransferDuration should be nil on parse failure; got %v",
			status.ObservedTransferDuration)
	}
	if status.ObservedPauseWindow != nil {
		t.Errorf("ObservedPauseWindow alias should be nil on parse failure; got %v",
			status.ObservedPauseWindow)
	}
}

// W27b: missing annotation (older swiftletd version, unexpected pod
// state) is silent — no log spam, both fields stay nil.
func TestStampTransferDuration_MissingAnnotation_LeavesBothFieldsNil(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{}
	status := &migrationv1alpha1.SwiftMigrationStatus{}
	src := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: nil}}
	stampTransferDuration(context.Background(), mig, status, src)
	if status.ObservedTransferDuration != nil {
		t.Errorf("ObservedTransferDuration should be nil when annotation absent; got %v",
			status.ObservedTransferDuration)
	}
	if status.ObservedPauseWindow != nil {
		t.Errorf("ObservedPauseWindow alias should be nil when annotation absent; got %v",
			status.ObservedPauseWindow)
	}
}

// key extracts the namespacedKey for a fixture pod (mirrors what
// existing tests use ad-hoc; small local helper for readability).
func key(p *corev1.Pod) namespacedKey {
	return namespacedKey{Name: p.Name, Namespace: p.Namespace}
}

type namespacedKey = struct {
	Namespace string
	Name      string
}
