package swiftmigration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// cancelFixture builds a SwiftMigration in mode=live + Preparing
// phase with spec.cancelRequested=true, plus the SwiftGuest and a
// dst pod that already exists. Tests adjust per scenario.
func cancelFixture(t *testing.T, dstAge time.Duration) (*migrationv1alpha1.SwiftMigration, *swiftv1alpha1.SwiftGuest, *corev1.Pod) {
	t.Helper()
	mig := newMigrationWithUID("m1", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.CancelRequested = true
	mig.Spec.Target.NodeName = "miles"
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	mig.Status.Mode = migrationv1alpha1.SwiftMigrationModeLive

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	dst := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "guest-mig-abcdef",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-dstAge)),
		},
		Spec:   corev1.PodSpec{NodeName: "miles"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return mig, guest, dst
}

func TestCancelLive_PreCutover_FirstReconcile_WritesAnnotation(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := cancelFixture(t, 5*time.Second)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, res, err := r.honorCancel(context.Background(), mig)
	if !handled {
		t.Fatalf("expected handled=true for pre-cutover live cancel")
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue for ack polling")
	}

	var got corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: dst.Name, Namespace: "default"}, &got); err != nil {
		t.Fatalf("re-get dst pod: %v", err)
	}
	if got.Annotations[AnnotationMigrationAction] != MigrationActionCancel {
		t.Errorf("cancel action annotation: want %q, got %q", MigrationActionCancel, got.Annotations[AnnotationMigrationAction])
	}
	if got.Annotations[AnnotationMigrationActionID] != cancelID(mig) {
		t.Errorf("cancel action-id annotation: want %q, got %q", cancelID(mig), got.Annotations[AnnotationMigrationActionID])
	}
}

func TestCancelLive_PreCutover_AckObserved_DeletesAndFinalizes(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := cancelFixture(t, 5*time.Second)
	cid := cancelID(mig)
	dst.Annotations = map[string]string{
		AnnotationMigrationAction:    MigrationActionCancel,
		AnnotationMigrationActionID:  cid,
		AnnotationMigrationStatus:    MigrationStatusFailed,
		AnnotationMigrationStatusID:  cid,
		AnnotationMigrationStatusDtl: "cancelled by operator",
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, _, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil {
		t.Fatalf("expected handled=true err=nil; got handled=%v err=%v", handled, err)
	}

	var updated migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: mig.Name, Namespace: "default"}, &updated); err != nil {
		t.Fatalf("re-get mig: %v", err)
	}
	if updated.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Errorf("phase: want Cancelled, got %q", updated.Status.Phase)
	}
	if updated.Status.FailureReason != migrationv1alpha1.FailureReasonCancelled {
		t.Errorf("FailureReason: want Cancelled, got %q", updated.Status.FailureReason)
	}
	// Dst pod should be deleted.
	if err := c.Get(context.Background(), client.ObjectKey{Name: dst.Name, Namespace: "default"}, &corev1.Pod{}); err == nil {
		t.Errorf("dst pod still exists; should have been deleted")
	}
}

func TestCancelLive_PreCutover_AckIdempotent_NoExtraAnnotationWrite(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := cancelFixture(t, 5*time.Second)
	cid := cancelID(mig)
	// Cancel annotation already present (re-entry case).
	dst.Annotations = map[string]string{
		AnnotationMigrationAction:   MigrationActionCancel,
		AnnotationMigrationActionID: cid,
		// No ack yet → re-entry must continue polling.
	}

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	c := newSelectiveFailingClient(base)
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, res, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil {
		t.Fatalf("expected handled=true err=nil; got %v %v", handled, err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected requeue while waiting for ack")
	}
	// Should have done a Get on Pod but NOT a Patch on Pod.
	if c.Count(typeKeyOf(&corev1.Pod{}), VerbPatch) != 0 {
		t.Errorf("expected 0 Pod patches (annotation already present); got %d", c.Count(typeKeyOf(&corev1.Pod{}), VerbPatch))
	}
}

func TestCancelLive_PreCutover_AckTimeout_ForceDeletes(t *testing.T) {
	scheme := testScheme(t)
	// Dst pod is 60s old (well past 30s ack budget); cancel
	// annotation present but no ack.
	mig, guest, dst := cancelFixture(t, 60*time.Second)
	cid := cancelID(mig)
	dst.Annotations = map[string]string{
		AnnotationMigrationAction:   MigrationActionCancel,
		AnnotationMigrationActionID: cid,
		// No ack → past budget → force delete.
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, _, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil {
		t.Fatalf("expected handled=true err=nil; got handled=%v err=%v", handled, err)
	}

	var updated migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: mig.Name, Namespace: "default"}, &updated); err != nil {
		t.Fatalf("re-get mig: %v", err)
	}
	if updated.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Errorf("phase: want Cancelled, got %q", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.FailureMessage, "force-deleted") {
		t.Errorf("FailureMessage: want force-delete reason, got %q", updated.Status.FailureMessage)
	}
}

func TestCancelLive_PreCutover_DstNotFound_FinalizesDirectly(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, _ := cancelFixture(t, 0)
	// No dst pod added.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, _, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil {
		t.Fatalf("expected handled=true err=nil; got handled=%v err=%v", handled, err)
	}
	var updated migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: mig.Name, Namespace: "default"}, &updated); err != nil {
		t.Fatalf("re-get mig: %v", err)
	}
	if updated.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Errorf("phase: want Cancelled, got %q", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.FailureMessage, "never created") {
		t.Errorf("FailureMessage: want 'never created' detail, got %q", updated.Status.FailureMessage)
	}
}

func TestCancelLive_PostCutover_SetsCancelIgnoredCondition(t *testing.T) {
	scheme := testScheme(t)
	mig, _, _ := cancelFixture(t, 0)
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy
	// PodRefSwapped=True flips isPostCutover to true.
	mig.Status.Conditions = []metav1.Condition{{
		Type:               migrationv1alpha1.SwiftMigrationConditionPodRefSwapped,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "CutoverStep1Complete",
	}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, _, err := r.honorCancel(context.Background(), mig)
	if handled {
		t.Errorf("expected handled=false (phase dispatch must continue post-cutover)")
	}
	if err != nil {
		t.Fatalf("err=%v", err)
	}

	var updated migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: mig.Name, Namespace: "default"}, &updated); err != nil {
		t.Fatalf("re-get mig: %v", err)
	}
	// Phase MUST NOT change.
	if updated.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy {
		t.Errorf("phase: want StopAndCopy (unchanged), got %q", updated.Status.Phase)
	}
	// CancelIgnored condition MUST be set.
	found := false
	for _, c := range updated.Status.Conditions {
		if c.Type == migrationv1alpha1.SwiftMigrationConditionCancelIgnored {
			found = true
			if c.Status != metav1.ConditionTrue {
				t.Errorf("CancelIgnored.Status: want True, got %q", c.Status)
			}
			if c.Reason != migrationv1alpha1.ReasonPastCutover {
				t.Errorf("CancelIgnored.Reason: want PastCutover, got %q", c.Reason)
			}
		}
	}
	if !found {
		t.Errorf("CancelIgnored condition not set")
	}
}

func TestCancelLive_PostCutover_Idempotent(t *testing.T) {
	scheme := testScheme(t)
	mig, _, _ := cancelFixture(t, 0)
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming
	mig.Status.Conditions = []metav1.Condition{
		{
			Type:               migrationv1alpha1.SwiftMigrationConditionPodRefSwapped,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "CutoverStep1Complete",
		},
		// CancelIgnored already set with the right reason.
		{
			Type:               migrationv1alpha1.SwiftMigrationConditionCancelIgnored,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             migrationv1alpha1.ReasonPastCutover,
		},
	}

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	c := newSelectiveFailingClient(base)
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, _, err := r.honorCancel(context.Background(), mig)
	if handled {
		t.Errorf("expected handled=false")
	}
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// Idempotent: NO status patch should fire (condition already set).
	if c.Count(typeKeyOf(&migrationv1alpha1.SwiftMigration{}), VerbStatusPatch) != 0 {
		t.Errorf("expected 0 status patches on idempotent post-cutover; got %d",
			c.Count(typeKeyOf(&migrationv1alpha1.SwiftMigration{}), VerbStatusPatch))
	}
}

func TestCancelLive_OfflineMode_Ignored(t *testing.T) {
	scheme := testScheme(t)
	mig, _, _ := cancelFixture(t, 0)
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeOffline
	mig.Status.Mode = migrationv1alpha1.SwiftMigrationModeOffline

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	c := newSelectiveFailingClient(base)
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	handled, _, err := r.honorCancel(context.Background(), mig)
	if handled {
		t.Errorf("offline mode + cancelRequested=true must be silently ignored (handled=false)")
	}
	if err != nil {
		t.Errorf("err=%v", err)
	}
	// No writes at all.
	if c.Count(typeKeyOf(&corev1.Pod{}), VerbPatch) != 0 {
		t.Errorf("offline cancel must not patch pods")
	}
	if c.Count(typeKeyOf(&migrationv1alpha1.SwiftMigration{}), VerbStatusPatch) != 0 {
		t.Errorf("offline cancel must not patch SwiftMigration")
	}
}

func TestCancelLive_NotRequested_NoOp(t *testing.T) {
	mig, _, _ := cancelFixture(t, 0)
	mig.Spec.CancelRequested = false

	r := &SwiftMigrationReconciler{}
	handled, _, err := r.honorCancel(context.Background(), mig)
	if handled || err != nil {
		t.Errorf("CancelRequested=false must yield handled=false err=nil; got %v %v", handled, err)
	}
}

// TestCancelLive_DispatchOrdering verifies cancel handling fires
// BEFORE phase dispatch in Reconcile. Setup: live mig in Validating
// phase with cancel requested. Reconcile must take the cancel
// branch (transition to Cancelled), NOT advance through Validating's
// normal logic. Verified by asserting phase post-Reconcile is
// Cancelled.
func TestCancelLive_DispatchOrdering_FiresBeforePhaseHandler(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, _ := cancelFixture(t, 0)
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	// No dst pod (not yet created in Validating); cancel finalizes
	// directly without ack.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: mig.Name, Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: mig.Name, Namespace: "default"}, &updated); err != nil {
		t.Fatalf("re-get mig: %v", err)
	}
	if updated.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Errorf("phase: want Cancelled (cancel pre-empted phase dispatch), got %q", updated.Status.Phase)
	}
}
