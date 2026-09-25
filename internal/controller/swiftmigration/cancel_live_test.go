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

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// cancelFixture builds a SwiftMigration in mode=live + Preparing
// phase with spec.cancelRequested=true, plus the SwiftGuest and a
// dst pod that already exists. Tests adjust per scenario.
func cancelFixture(t *testing.T, dstAge time.Duration) (*migrationv1alpha1.SwiftMigration, *swiftv1alpha1.SwiftGuest, *corev1.Pod) {
	t.Helper()
	mig := newMigrationWithUID("m1", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.CancelRequested = true
	mig.Spec.Target.NodeName = "worker-2"
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
		Spec:   corev1.PodSpec{NodeName: "worker-2"},
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
	if at, err := time.Parse(time.RFC3339, got.Annotations[AnnotationMigrationCancelIssuedAt]); err != nil || time.Since(at) > time.Minute {
		t.Errorf("cancel issued-at annotation: want the time of the cancel, got %q (%v)", got.Annotations[AnnotationMigrationCancelIssuedAt], err)
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
		AnnotationMigrationAction:         MigrationActionCancel,
		AnnotationMigrationActionID:       cid,
		AnnotationMigrationCancelIssuedAt: time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339),
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
	// Cancel issued 60s ago (well past the 30s ack budget), no ack.
	mig, guest, dst := cancelFixture(t, 10*time.Minute)
	cid := cancelID(mig)
	dst.Annotations = map[string]string{
		AnnotationMigrationAction:         MigrationActionCancel,
		AnnotationMigrationActionID:       cid,
		AnnotationMigrationCancelIssuedAt: time.Now().Add(-60 * time.Second).UTC().Format(time.RFC3339),
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
	if !strings.Contains(updated.Status.FailureMessage, "does not exist") {
		t.Errorf("FailureMessage: want 'does not exist' detail, got %q", updated.Status.FailureMessage)
	}
}

// The budget runs from the cancel, not from the destination pod's creation:
// a migration is always well past 30s into its transfer before it can be
// cancelled, and anchoring on the pod force-deleted every destination at once,
// before swiftletd could stop the receive (lab validation of v0.15.0, D8).
func TestCancelLive_PreCutover_JustIssuedCancelWaitsForTheAck(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := cancelFixture(t, 10*time.Minute)
	dst.Annotations = map[string]string{
		AnnotationMigrationAction:         MigrationActionCancel,
		AnnotationMigrationActionID:       cancelID(mig),
		AnnotationMigrationCancelIssuedAt: time.Now().UTC().Format(time.RFC3339),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, dst).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	handled, res, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil || res.RequeueAfter == 0 {
		t.Fatalf("want a requeue while waiting for the ack; got handled=%v res=%+v err=%v", handled, res, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dst), &corev1.Pod{}); err != nil {
		t.Errorf("destination pod deleted before swiftletd could ack the cancel: %v", err)
	}
	var updated migrationv1alpha1.SwiftMigration
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(mig), &updated)
	if updated.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Error("cancel finalized without waiting for the ack")
	}
}

// A cancel issued by an older controller carries no issued-at: it gets one now
// and a full budget, rather than an immediate force-delete.
func TestCancelLive_PreCutover_CancelWithoutIssuedAtGetsAFreshBudget(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, dst := cancelFixture(t, 10*time.Minute)
	dst.Annotations = map[string]string{
		AnnotationMigrationAction:   MigrationActionCancel,
		AnnotationMigrationActionID: cancelID(mig),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, dst).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if handled, res, err := r.honorCancel(context.Background(), mig); !handled || err != nil || res.RequeueAfter == 0 {
		t.Fatalf("want a requeue; got handled=%v res=%+v err=%v", handled, res, err)
	}
	var got corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dst), &got); err != nil {
		t.Fatalf("destination pod deleted: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, got.Annotations[AnnotationMigrationCancelIssuedAt]); err != nil {
		t.Errorf("issued-at not stamped: %q", got.Annotations[AnnotationMigrationCancelIssuedAt])
	}
}

// A reconcile working from a stale copy must not overwrite the Cancelled
// status another reconcile wrote. In the lab a second pass, still seeing the
// migration in progress, found the destination gone and replaced "destination
// pod force-deleted" with "destination pod was never created" (D8).
func TestCancelLive_FinalizeFromAStaleCopyDoesNotOverwrite(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, _ := cancelFixture(t, 0)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	ctx := context.Background()

	var fresh, stale migrationv1alpha1.SwiftMigration
	_ = c.Get(ctx, client.ObjectKeyFromObject(mig), &fresh)
	_ = c.Get(ctx, client.ObjectKeyFromObject(mig), &stale)
	if _, err := r.finalizeCancelled(ctx, &fresh, "destination pod force-deleted"); err != nil {
		t.Fatal(err)
	}
	res, err := r.finalizeCancelled(ctx, &stale, "destination pod does not exist")
	if err != nil || !res.Requeue {
		t.Errorf("stale finalize: want a requeue, got res=%+v err=%v", res, err)
	}
	var got migrationv1alpha1.SwiftMigration
	_ = c.Get(ctx, client.ObjectKeyFromObject(mig), &got)
	if got.Status.FailureMessage != "destination pod force-deleted" {
		t.Errorf("FailureMessage = %q, want the first finalize's", got.Status.FailureMessage)
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
