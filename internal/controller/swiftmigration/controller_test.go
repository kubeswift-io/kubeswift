package swiftmigration

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// scheme builds a scheme with migration + swift v1alpha1 + corev1.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvSwift)
	gvMig := schema.GroupVersion{Group: "migration.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvMig, &migrationv1alpha1.SwiftMigration{}, &migrationv1alpha1.SwiftMigrationList{})
	metav1.AddToGroupVersion(s, gvMig)
	return s
}

func newMigration(name, ns string) *migrationv1alpha1.SwiftMigration {
	return &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: migrationv1alpha1.SwiftMigrationSpec{
			GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "guest"},
			Target:   migrationv1alpha1.SwiftMigrationTarget{NodeName: "miles"},
			Mode:     migrationv1alpha1.SwiftMigrationModeOffline,
		},
	}
}

// TestReconcile_FirstReconcile_PendingToValidating verifies that an
// empty-phase SwiftMigration transitions to Validating on the first
// reconcile and stamps StartedAt. Subsequent commits implement the
// phase logic; the skeleton only needs to dispatch.
func TestReconcile_FirstReconcile_PendingToValidating(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()

	r := &SwiftMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("Reconcile returned err = %v, want nil", err)
	}
	if !res.Requeue {
		t.Errorf("Reconcile should requeue after Pending→Validating transition; got %+v", res)
	}

	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseValidating {
		t.Errorf("phase = %q, want Validating", got.Status.Phase)
	}
	if got.Status.StartedAt == nil {
		t.Error("StartedAt should be stamped on first reconcile")
	}
	if got.Status.PhaseDetail == "" {
		t.Error("PhaseDetail should be populated")
	}
}

// TestReconcile_TerminalPhase_NoOp verifies that re-reconciling a
// Completed/Failed/Cancelled SwiftMigration produces no API churn.
// Terminal phases are absorbing states; the controller must not
// re-enter the state machine for them.
func TestReconcile_TerminalPhase_NoOp(t *testing.T) {
	for _, phase := range []migrationv1alpha1.SwiftMigrationPhase{
		migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled,
	} {
		t.Run(string(phase), func(t *testing.T) {
			scheme := testScheme(t)
			mig := newMigration("m", "default")
			mig.Status.Phase = phase
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(mig).
				WithStatusSubresource(mig).
				Build()

			r := &SwiftMigrationReconciler{
				Client:   c,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(10),
			}

			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"},
			})
			if err != nil {
				t.Fatalf("Reconcile in terminal phase %s returned err = %v", phase, err)
			}
			if res.Requeue || res.RequeueAfter != 0 {
				t.Errorf("Reconcile in terminal phase %s should be no-op; got %+v", phase, res)
			}
		})
	}
}

// TestReconcile_NotFoundIsIgnored verifies that a Reconcile call for a
// SwiftMigration that doesn't exist returns nil error (NotFound is the
// expected race condition: deletion observed before the cache caught
// up). Mirrors snapshot/restore controllers.
func TestReconcile_NotFoundIsIgnored(t *testing.T) {
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"},
	})
	if err != nil {
		t.Errorf("Reconcile of missing resource should ignore NotFound; got %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("Reconcile of missing resource should not requeue; got %+v", res)
	}
}

// TestReconcile_UnknownPhase_Requeues verifies forward-compatibility
// with Phase 3+ phases (e.g., a future PreCopy phase). The Phase 1
// controller treats unknown phases as opaque and requeues without
// action — does NOT advance phase or set Failed.
func TestReconcile_UnknownPhase_Requeues(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = "PreCopy" // hypothetical Phase 3 phase
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"},
	})
	if err != nil {
		t.Errorf("Reconcile of unknown phase should not error; got %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("Reconcile of unknown phase should requeue; got %+v", res)
	}

	// Phase must NOT be advanced — observers of a hypothetical Phase 3
	// SwiftMigration must not see its phase changed by an old Phase 1
	// controller.
	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Status.Phase) != "PreCopy" {
		t.Errorf("phase = %q, want unchanged 'PreCopy' (forward compat)", got.Status.Phase)
	}
}

// TestSetCondition_AppendNew verifies the helper appends a new
// Condition entry when none of the right type exists.
func TestSetCondition_AppendNew(t *testing.T) {
	status := &migrationv1alpha1.SwiftMigrationStatus{}
	setCondition(status, "Ready", metav1.ConditionTrue, "Reason", "Message")
	if len(status.Conditions) != 1 {
		t.Fatalf("len(Conditions) = %d, want 1", len(status.Conditions))
	}
	if status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("Status = %q, want True", status.Conditions[0].Status)
	}
	if status.Conditions[0].LastTransitionTime.IsZero() {
		t.Error("LastTransitionTime should be set on first append")
	}
}

// TestSetCondition_UpdateInPlace verifies the helper updates an
// existing Condition without appending a duplicate.
func TestSetCondition_UpdateInPlace(t *testing.T) {
	status := &migrationv1alpha1.SwiftMigrationStatus{
		Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Old", Message: "Old"},
		},
	}
	setCondition(status, "Ready", metav1.ConditionTrue, "New", "New")
	if len(status.Conditions) != 1 {
		t.Errorf("len(Conditions) = %d, want 1 (no duplicate)", len(status.Conditions))
	}
	if status.Conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("Status not updated; got %q", status.Conditions[0].Status)
	}
	if status.Conditions[0].Reason != "New" {
		t.Errorf("Reason not updated; got %q", status.Conditions[0].Reason)
	}
}
