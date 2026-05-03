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

func TestStopAndCopyLive_DstFailed_GenericDetail_FailsWithOther(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	// Generic CH-internal error string (no D2 / W1 / cancel match).
	// B3.3 classifier returns Other for unrecognised details.
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), MigrationStatusFailed, recvActionID(mig), "receive_migration: ch_internal_error")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other for unrecognised detail, got %q", res.FailureReason)
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

// key extracts the namespacedKey for a fixture pod (mirrors what
// existing tests use ad-hoc; small local helper for readability).
func key(p *corev1.Pod) namespacedKey {
	return namespacedKey{Name: p.Name, Namespace: p.Namespace}
}

type namespacedKey = struct {
	Namespace string
	Name      string
}
