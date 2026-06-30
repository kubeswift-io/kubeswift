package swiftmigration

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
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

// TestReconcile_TerminalPhase_NoFinalizer_NoPatch verifies the Bug B
// refinement: a terminal-phase SwiftMigration without our finalizer
// short-circuits with zero API roundtrips. Pod and SwiftGuest watches
// fan out to every active migration in the namespace and a single
// completed migration receives many spurious enqueues over its
// lifetime — none of those should trigger a Patch.
//
// The fake client's countingClient wraps Patch to count invocations.
// A no-op Reconcile is expected to issue zero Patch calls.
func TestReconcile_TerminalPhase_NoFinalizer_NoPatch(t *testing.T) {
	for _, phase := range []migrationv1alpha1.SwiftMigrationPhase{
		migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled,
	} {
		t.Run(string(phase), func(t *testing.T) {
			scheme := testScheme(t)
			mig := newMigration("m", "default")
			mig.Status.Phase = phase
			// Finalizer already absent (cleanup ran on terminal-phase transition).

			base := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(mig).
				WithStatusSubresource(mig).
				Build()
			counter := &patchCountingClient{Client: base}

			r := &SwiftMigrationReconciler{
				Client:   counter,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(10),
			}
			res, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"},
			})
			if err != nil {
				t.Fatalf("Reconcile returned err = %v", err)
			}
			if res.Requeue || res.RequeueAfter != 0 {
				t.Errorf("Reconcile should be no-op; got %+v", res)
			}
			if counter.patches != 0 {
				t.Errorf("Reconcile on terminal+finalizer-absent should issue zero Patch calls; got %d", counter.patches)
			}
		})
	}
}

// TestReconcile_TerminalPhase_StaleFinalizer_RemovesIt verifies that
// a terminal-phase SwiftMigration WITH the finalizer still attached
// (e.g., after a controller crash between the terminal-phase status
// write and the finalizer-removal patch) drops the finalizer on the
// next reconcile. Idempotent: a second reconcile is a no-op.
func TestReconcile_TerminalPhase_StaleFinalizer_RemovesIt(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted
	mig.Finalizers = []string{FinalizerName}

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	counter := &patchCountingClient{Client: base}

	r := &SwiftMigrationReconciler{
		Client:   counter,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(10),
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"},
	}); err != nil {
		t.Fatalf("Reconcile returned err = %v", err)
	}

	var got migrationv1alpha1.SwiftMigration
	if err := base.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hasFinalizer(&got) {
		t.Errorf("finalizer should have been removed on terminal-phase reconcile; got %v", got.Finalizers)
	}
	if counter.patches != 1 {
		t.Errorf("expected exactly 1 Patch call (finalizer removal); got %d", counter.patches)
	}

	// Second reconcile: now the finalizer is gone — must issue zero
	// further Patches.
	priorPatches := counter.patches
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"},
	}); err != nil {
		t.Fatalf("second Reconcile returned err = %v", err)
	}
	if counter.patches != priorPatches {
		t.Errorf("second reconcile on terminal+finalizer-absent should issue zero further Patch calls; got %d new", counter.patches-priorPatches)
	}
}

// TestReconcile_InFlight_GuestDisappeared_DrivesToFailed — Bug C
// regression at the controller level. An in-flight (Validating)
// SwiftMigration whose source SwiftGuest has been deleted mid-flight
// must be driven to Failed, not trapped in a non-terminal phase.
// The companion webhook test (TestValidateUpdate_InFlight_NoClusterState)
// proves the metadata patches the controller issues here are admitted;
// this test proves the controller's state machine actually uses that
// permission to drive cleanup.
//
// Flow:
//
//  1. Reconcile #1: phase=Validating, source guest absent.
//     - ensureFinalizer adds finalizer (idempotent, no-op if already there).
//     - handleValidating returns errMsg "source SwiftGuest no longer exists".
//     - dispatchResult sets phase=Failed and persists status.
//  2. Reconcile #2: phase=Failed, finalizer still present.
//     - Terminal-phase short-circuit drops the finalizer and returns.
//  3. Reconcile #3 (idempotent check): phase=Failed, finalizer absent.
//     - Zero API roundtrips.
//
// Pre-fix the live cluster trapped this scenario: the validating
// webhook rejected ensureFinalizer's metadata patch on every reconcile
// because it tried to validate cluster state against a missing source
// guest. The controller couldn't make forward progress; the migration
// stayed Validating forever.
func TestReconcile_InFlight_GuestDisappeared_DrivesToFailed(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseValidating
	// Source SwiftGuest intentionally absent — operator deleted it
	// mid-migration.
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
	key := client.ObjectKey{Name: "m", Namespace: "default"}

	// Reconcile #1: should drive Validating → Failed.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile #1: %v", err)
	}
	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("Get after #1: %v", err)
	}
	if got.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Errorf("phase after Reconcile #1 = %q, want Failed", got.Status.Phase)
	}
	if got.Status.FailureMessage == "" || !strings.Contains(got.Status.FailureMessage, "no longer exists") {
		t.Errorf("FailureMessage = %q, want mention of missing source guest", got.Status.FailureMessage)
	}
	if !hasFinalizer(&got) {
		t.Error("finalizer should still be present after Reconcile #1 (transition to Failed)")
	}

	// Reconcile #2: terminal phase → drop finalizer.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatalf("Get after #2: %v", err)
	}
	if hasFinalizer(&got) {
		t.Errorf("finalizer should be removed after Reconcile #2; got %v", got.Finalizers)
	}

	// Reconcile #3 (idempotent): zero work expected.
	counter := &patchCountingClient{Client: c}
	r.Client = counter
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile #3: %v", err)
	}
	if counter.patches != 0 {
		t.Errorf("Reconcile #3 on Failed+finalizer-absent should issue zero patches; got %d", counter.patches)
	}
}

// TestHasFinalizer covers the helper directly. Trivial but locks in
// the exact finalizer string and rejects accidental case/whitespace
// drift.
func TestHasFinalizer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		finalizers []string
		want       bool
	}{
		{"absent", nil, false},
		{"empty", []string{}, false},
		{"present", []string{FinalizerName}, true},
		{"present with siblings", []string{"other", FinalizerName, "yet-another"}, true},
		{"only-siblings", []string{"other", "yet-another"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mig := &migrationv1alpha1.SwiftMigration{
				ObjectMeta: metav1.ObjectMeta{Finalizers: tc.finalizers},
			}
			if got := hasFinalizer(mig); got != tc.want {
				t.Errorf("hasFinalizer(%v) = %v, want %v", tc.finalizers, got, tc.want)
			}
		})
	}
}

// patchCountingClient wraps client.Client and counts BOTH metadata and
// status Patch invocations. Used by terminal-phase short-circuit tests
// to assert exact API roundtrip counts. Counts both surfaces so a
// future regression that adds a status patch on the short-circuit path
// (e.g., bumping lastObservedAt on a stale completed migration) is
// caught — counting only metadata patches would silently miss it.
type patchCountingClient struct {
	client.Client
	patches int
}

func (p *patchCountingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	p.patches++
	return p.Client.Patch(ctx, obj, patch, opts...)
}

func (p *patchCountingClient) Status() client.SubResourceWriter {
	return &countingStatusWriter{inner: p.Client.Status(), parent: p}
}

type countingStatusWriter struct {
	inner  client.SubResourceWriter
	parent *patchCountingClient
}

func (c *countingStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return c.inner.Create(ctx, obj, subResource, opts...)
}

func (c *countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	c.parent.patches++
	return c.inner.Update(ctx, obj, opts...)
}

func (c *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	c.parent.patches++
	return c.inner.Patch(ctx, obj, patch, opts...)
}

func (c *countingStatusWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	c.parent.patches++
	return c.inner.Apply(ctx, obj, opts...)
}

// TestHandleCancellation_DeletedSourceGuest_RemovesFinalizer covers
// the Bug A controller-side flow end-to-end against the in-process
// fake client (the webhook isn't wired in, but the controller's
// removeFinalizer Patch shape is the same one the live cluster
// rejects pre-fix, so verifying the controller handles the
// happy path correctly closes the round-trip the QA review
// flagged as untested).
//
// Scenario: a mid-flight (Preparing) SwiftMigration is deleted by
// the operator AFTER they've already deleted the source SwiftGuest.
// handleCancellation runs cleanupSourceGuest (no-op, guest gone),
// then removeFinalizer. Assert the finalizer is removed and no
// error is returned.
func TestHandleCancellation_DeletedSourceGuest_RemovesFinalizer(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	mig.Finalizers = []string{FinalizerName}
	now := metav1.Now()
	mig.DeletionTimestamp = &now

	// Source SwiftGuest intentionally absent.
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

	if _, err := r.handleCancellation(context.Background(), mig); err != nil {
		t.Fatalf("handleCancellation should succeed when source guest is missing; got %v", err)
	}

	// The fake client honors finalizer removal: once the last
	// finalizer is dropped on a resource with DeletionTimestamp set,
	// the resource is deleted. Verify by Get → IsNotFound.
	var got migrationv1alpha1.SwiftMigration
	getErr := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &got)
	if getErr == nil {
		// Some fake-client versions retain the object — fall back to
		// asserting the finalizer is gone.
		if hasFinalizer(&got) {
			t.Errorf("finalizer should have been removed; got %v", got.Finalizers)
		}
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
