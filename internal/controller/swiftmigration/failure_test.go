package swiftmigration

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// TestCancellation_PreCutover_RestoresRunPolicy verifies the
// architect's Risk 2 split: cancellation in Preparing (pre-cutover)
// restores the source guest's runPolicy=Running and clears the
// in-progress annotation. The source guest then resumes naturally
// via the SwiftGuest controller.
func TestCancellation_PreCutover_RestoresRunPolicy(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped // set by Preparing
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	now := metav1.Now()
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	mig.DeletionTimestamp = &now
	mig.Finalizers = []string{FinalizerName}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.handleCancellation(context.Background(), mig); err != nil {
		t.Fatalf("handleCancellation returned err = %v", err)
	}

	// Source guest's runPolicy must be back to Running and the
	// annotation cleared.
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("pre-cutover cancellation did not restore runPolicy: got %q, want Running", got.Spec.RunPolicy)
	}
	if _, present := got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]; present {
		t.Error("annotation should be cleared on pre-cutover cancellation")
	}

	// Finalizer must be removed so the SwiftMigration deletion proceeds.
	var migGot migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &migGot); err == nil {
		for _, f := range migGot.Finalizers {
			if f == FinalizerName {
				t.Error("finalizer should be removed after cleanup")
			}
		}
	}
}

// TestCancellation_PostCutover_ClearsAnnotationOnly verifies that
// once spec.nodeName has been patched to the destination
// (StopAndCopy onwards), cancellation does NOT roll back the patch.
// The migration is committed; we just clear the annotation. The
// destination guest continues running.
func TestCancellation_PostCutover_ClearsAnnotationOnly(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	guest.Spec.NodeName = "miles" // post-cutover
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	now := metav1.Now()
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy
	mig.DeletionTimestamp = &now
	mig.Finalizers = []string{FinalizerName}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.handleCancellation(context.Background(), mig); err != nil {
		t.Fatalf("handleCancellation returned err = %v", err)
	}

	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	// nodeName patch must NOT be rolled back.
	if got.Spec.NodeName != "miles" {
		t.Errorf("post-cutover cancellation should not roll back nodeName: got %q, want miles", got.Spec.NodeName)
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("post-cutover cancellation should leave runPolicy=Running: got %q", got.Spec.RunPolicy)
	}
	if _, present := got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]; present {
		t.Error("annotation should be cleared on post-cutover cancellation")
	}
}

// TestCancellation_NoMatchingAnnotation_NoOp verifies idempotency:
// if the source guest's annotation was already cleared (cleanup ran
// previously) or names a different migration, handleCancellation
// does not modify the guest.
func TestCancellation_NoMatchingAnnotation_NoOp(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	// Annotation names a DIFFERENT migration.
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "other-m",
	}
	now := metav1.Now()
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	mig.DeletionTimestamp = &now
	mig.Finalizers = []string{FinalizerName}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.handleCancellation(context.Background(), mig); err != nil {
		t.Fatalf("handleCancellation returned err = %v", err)
	}

	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	// Other migration's annotation untouched.
	if got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] != "other-m" {
		t.Errorf("must not touch annotation when it names a different migration: got %q",
			got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress])
	}
	// runPolicy untouched (we don't roll back state we didn't write).
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyStopped {
		t.Errorf("runPolicy should be untouched when annotation names a different migration: got %q", got.Spec.RunPolicy)
	}
}

// TestCancellation_GuestDeleted_DropsFinalizerCleanly verifies that
// if the source guest is already gone when the SwiftMigration is
// being cancelled, the finalizer is still dropped (no work to do).
func TestCancellation_GuestDeleted_DropsFinalizerCleanly(t *testing.T) {
	scheme := preparingScheme(t)
	now := metav1.Now()
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	mig.DeletionTimestamp = &now
	mig.Finalizers = []string{FinalizerName}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if _, err := r.handleCancellation(context.Background(), mig); err != nil {
		t.Fatalf("handleCancellation with deleted guest should be no-op; got err=%v", err)
	}

	// Confirm finalizer was removed.
	var migGot migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &migGot); err == nil {
		for _, f := range migGot.Finalizers {
			if f == FinalizerName {
				t.Error("finalizer should be removed even when source guest is gone")
			}
		}
	}
}

// TestEnsureFinalizer_Idempotent verifies that adding the finalizer
// twice is a no-op (no duplicate entries).
func TestEnsureFinalizer_Idempotent(t *testing.T) {
	scheme := preparingScheme(t)
	mig := newMigration("m", "default")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	if err := r.ensureFinalizer(context.Background(), mig); err != nil {
		t.Fatalf("ensureFinalizer first call: %v", err)
	}
	if err := r.ensureFinalizer(context.Background(), mig); err != nil {
		t.Fatalf("ensureFinalizer second call: %v", err)
	}
	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	count := 0
	for _, f := range got.Finalizers {
		if f == FinalizerName {
			count++
		}
	}
	if count != 1 {
		t.Errorf("finalizer count = %d, want 1 (idempotent)", count)
	}
}

// TestOnTerminalPhase_FailedPreCutover_RestoresRunPolicy verifies the
// dispatchResult-driven cleanup: when a phase handler returns
// errMsg!=nil and we transition to Failed, the source guest's
// runPolicy is restored if we're pre-cutover.
func TestOnTerminalPhase_FailedPreCutover_RestoresRunPolicy(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	mig := newMigration("m", "default")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := &migrationv1alpha1.SwiftMigrationStatus{
		Phase:           migrationv1alpha1.SwiftMigrationPhaseFailed,
		SourceNode:      "boba",
		DestinationNode: "miles",
	}
	// guest.Spec.NodeName is "" → pre-cutover.
	if err := r.onTerminalPhase(context.Background(), mig, status); err != nil {
		t.Fatalf("onTerminalPhase returned err = %v", err)
	}

	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("pre-cutover Failed: runPolicy = %q, want Running (restored)", got.Spec.RunPolicy)
	}
}
