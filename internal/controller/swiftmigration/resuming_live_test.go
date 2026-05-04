package swiftmigration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// resumingLiveFixture builds the standard set of cluster objects
// handleResumingLive expects at entry: SwiftMigration in Resuming
// phase (mode=live, status.startedAt + resumingStartedAt set),
// SwiftGuest with status.podRef pointing at dst pod, and the dst pod
// itself. Tests adjust individual fields per scenario.
func resumingLiveFixture(t *testing.T) (*migrationv1alpha1.SwiftMigration, *swiftv1alpha1.SwiftGuest, *corev1.Pod) {
	t.Helper()
	mig := newMigrationWithUID("m1", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Target.NodeName = "miles"
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming
	mig.Status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Status.DestinationNode = "miles"
	startedAt := metav1.NewTime(time.Now().Add(-30 * time.Second))
	mig.Status.StartedAt = &startedAt

	dstName := "guest-mig-abcdef"
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
		Status: swiftv1alpha1.SwiftGuestStatus{
			PodRef: &corev1.ObjectReference{Name: dstName, Namespace: "default"},
		},
	}
	dst := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dstName,
			Namespace: "default",
		},
		Spec: corev1.PodSpec{NodeName: "miles"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	return mig, guest, dst
}

func setGuestRunningTrue(guest *swiftv1alpha1.SwiftGuest) {
	guest.Status.Conditions = append(guest.Status.Conditions, metav1.Condition{
		Type:               guestRunningConditionType,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "Running",
	})
}

func TestResumingLive_HappyPath_AdvancesToCompleted(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	setGuestRunningTrue(guest)
	dst.Annotations = map[string]string{AnnotationGuestIP: "10.0.0.42"}
	resumingStartedAt := metav1.NewTime(time.Now().Add(-2 * time.Second))
	mig.Status.ResumingStartedAt = &resumingStartedAt
	// W27a: ObservedDowntime now anchors on CutoverStep2DispatchedAt
	// (stamped by cutoverStep2). Stamp slightly before
	// ResumingStartedAt to mirror the production ordering.
	cutoverStep2At := metav1.NewTime(time.Now().Add(-3 * time.Second))
	mig.Status.CutoverStep2DispatchedAt = &cutoverStep2At

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	if !res.Advanced {
		t.Errorf("expected Advanced; got %+v", res)
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Errorf("phase: want Completed, got %q", status.Phase)
	}
	if status.TargetIP != "10.0.0.42" {
		t.Errorf("TargetIP: want 10.0.0.42, got %q", status.TargetIP)
	}
	if status.CompletedAt == nil {
		t.Errorf("CompletedAt not stamped")
	}
	if status.ObservedDowntime == nil {
		t.Errorf("ObservedDowntime not computed")
	} else if status.ObservedDowntime.Duration <= 0 {
		t.Errorf("ObservedDowntime should be positive, got %v", status.ObservedDowntime.Duration)
	}
}

func TestResumingLive_DstPodNotFound_FailsWithPodTerminated(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, _ := resumingLiveFixture(t)
	// No dst pod added — simulates K8s eviction post-cutover.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonPodTerminated {
		t.Errorf("FailureReason: want PodTerminated, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "terminated post-cutover") {
		t.Errorf("FailureMsg: want 'terminated post-cutover', got %q", res.FailureMsg)
	}
}

func TestResumingLive_DstPodNotRunning_Requeues(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	dst.Status.Phase = corev1.PodPending
	setGuestRunningTrue(guest)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.Advanced {
		t.Errorf("expected phaseRequeue; got %+v", res)
	}
	if res.Requeue == 0 {
		t.Errorf("expected non-zero Requeue")
	}
	if status.Phase == migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Errorf("must not advance to Completed when dst pod not Running")
	}
}

func TestResumingLive_GuestRunningMissing_Requeues(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	// No GuestRunning condition added.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.Advanced {
		t.Errorf("expected phaseRequeue; got %+v", res)
	}
	if status.Phase == migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Errorf("must not advance when GuestRunning condition missing")
	}
}

func TestResumingLive_GuestRunningFalse_Requeues(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	guest.Status.Conditions = []metav1.Condition{{
		Type:               guestRunningConditionType,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "NotRunning",
	}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.FailureMsg != "" || res.Advanced {
		t.Errorf("expected phaseRequeue; got %+v", res)
	}
}

func TestResumingLive_GuestIPAnnotationAbsent_NotFailure(t *testing.T) {
	// §3.6: guest-ip annotation absence is NOT a failure. D3 may not
	// have written yet; spec.timeout is the safety net.
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	setGuestRunningTrue(guest)
	// dst.Annotations not set.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	// Should advance to Completed even without IP annotation.
	if !res.Advanced {
		t.Fatalf("expected Advanced even without guest-ip annotation; got %+v", res)
	}
	if status.TargetIP != "" {
		t.Errorf("TargetIP should be empty when annotation absent; got %q", status.TargetIP)
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Errorf("phase: want Completed, got %q", status.Phase)
	}
}

func TestResumingLive_TimeoutExceeded_FailsWithTimeout(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	// StartedAt is 10 minutes ago; spec.timeout is 60s → exceeded.
	startedAt := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	mig.Status.StartedAt = &startedAt
	mig.Spec.Timeout = &metav1.Duration{Duration: 60 * time.Second}
	// Dst pod Running but GuestRunning never set.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonTimeout {
		t.Errorf("FailureReason: want Timeout, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "spec.timeout") {
		t.Errorf("FailureMsg: want spec.timeout message, got %q", res.FailureMsg)
	}
}

func TestResumingLive_EmptyPodRef_FailsWithOther(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	guest.Status.PodRef = nil // simulates entry-without-cutover bug

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "empty status.podRef") {
		t.Errorf("FailureMsg: want empty-podRef message, got %q", res.FailureMsg)
	}
}

func TestResumingLive_FirstReconcile_StampsResumingStartedAt(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	// ResumingStartedAt is nil; first reconcile in Resuming phase.
	mig.Status.ResumingStartedAt = nil
	// Pod Running but GuestRunning not yet → reconcile returns
	// Requeue, but ResumingStartedAt should still be stamped.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	_ = r.handleResumingLive(context.Background(), mig, status)
	if status.ResumingStartedAt == nil {
		t.Errorf("ResumingStartedAt not stamped on first reconcile")
	}
}

func TestResumingLive_DefensiveGuard_NotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	res := r.handleResumingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(res.FailureMsg, "without live mode") {
		t.Errorf("guard message: got %q", res.FailureMsg)
	}
}

// Regression check on B1's shouldCheckSourcePodUID: the helper must
// return false in Resuming phase. Documented in B1 commit ae4c695;
// re-asserted here so any future change to the helper that breaks
// the post-cutover gate surfaces immediately.
func TestResumingLive_ShouldCheckSourcePodUID_ReturnsFalse(t *testing.T) {
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{
			Phase: migrationv1alpha1.SwiftMigrationPhaseResuming,
		},
	}
	if shouldCheckSourcePodUID(mig) {
		t.Errorf("shouldCheckSourcePodUID(Resuming) must return false; src pod is intentionally deleted by cutover step 2")
	}
}

func TestResumingLive_SwiftGuestDeleted_FailsWithOther(t *testing.T) {
	scheme := testScheme(t)
	mig, _, _ := resumingLiveFixture(t)
	// No SwiftGuest in cluster.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", res.FailureReason)
	}
}

func TestResumingLive_PreservesObservedPauseWindow(t *testing.T) {
	// B3 will populate status.ObservedPauseWindow during StopAndCopy
	// from src pod's migration-status-detail annotation. B2.3 must
	// NOT overwrite or clear the value.
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	setGuestRunningTrue(guest)
	preset := metav1.Duration{Duration: 1500 * time.Millisecond}
	mig.Status.ObservedPauseWindow = &preset

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handleResumingLive(context.Background(), mig, status)
	if !res.Advanced {
		t.Fatalf("expected Advanced; got %+v", res)
	}
	if status.ObservedPauseWindow == nil || status.ObservedPauseWindow.Duration != preset.Duration {
		t.Errorf("ObservedPauseWindow modified; want %v, got %+v", preset, status.ObservedPauseWindow)
	}
}

// W27a integration: observedDowntime is computed against
// CutoverStep2DispatchedAt, NOT ResumingStartedAt. This test simulates
// the production reality where step 2 dispatched ~45s ago and the dst
// is now reaching GuestRunning=True. Pre-W27a the value was a
// sub-millisecond nonsense reading from two adjacent metav1.Now()
// calls in the same reconcile.
func TestResumingLive_ObservedDowntime_AnchoredOnCutoverStep2_W27a(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	setGuestRunningTrue(guest)
	dst.Annotations = map[string]string{AnnotationGuestIP: "10.0.0.42"}
	// Production timing: step 2 dispatched ~45s ago.
	cutoverStep2At := metav1.NewTime(time.Now().Add(-45 * time.Second))
	mig.Status.CutoverStep2DispatchedAt = &cutoverStep2At
	// Resuming was entered shortly after step 2.
	resumingStartedAt := metav1.NewTime(time.Now().Add(-44 * time.Second))
	mig.Status.ResumingStartedAt = &resumingStartedAt

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	if res := r.handleResumingLive(context.Background(), mig, status); !res.Advanced {
		t.Fatalf("expected Advanced; got %+v", res)
	}
	if status.ObservedDowntime == nil {
		t.Fatalf("ObservedDowntime not computed")
	}
	// Must reflect ~45s; allow generous tolerance for test wall-clock
	// drift. Pre-W27a value was ~50µs.
	got := status.ObservedDowntime.Duration
	if got < 40*time.Second || got > 50*time.Second {
		t.Errorf("ObservedDowntime should reflect ~45s cutover-to-resume window; got %v", got)
	}
}

// W27a defensive: missing CutoverStep2DispatchedAt (state-machine
// invariant violation) leaves ObservedDowntime nil rather than
// reporting a wrong value. Operators see a missing field, never the
// pre-W27a sub-millisecond nonsense.
func TestResumingLive_ObservedDowntime_NilCutoverStamp_LeavesFieldNil_W27a(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := resumingLiveFixture(t)
	setGuestRunningTrue(guest)
	dst.Annotations = map[string]string{AnnotationGuestIP: "10.0.0.42"}
	resumingStartedAt := metav1.NewTime(time.Now().Add(-2 * time.Second))
	mig.Status.ResumingStartedAt = &resumingStartedAt
	// CutoverStep2DispatchedAt deliberately nil.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	if res := r.handleResumingLive(context.Background(), mig, status); !res.Advanced {
		t.Fatalf("expected Advanced; got %+v", res)
	}
	if status.ObservedDowntime != nil {
		t.Errorf("ObservedDowntime should be nil when CutoverStep2DispatchedAt is missing; got %v",
			status.ObservedDowntime)
	}
}
