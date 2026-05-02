package swiftmigration

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Self-tests for selectiveFailingClient. These tests demonstrate the
// scaffolding works as documented before any Group B code depends on
// it. Each test exercises one design property:
//
//   - typeKeyOf produces stable, distinguishable keys
//   - Count tracks per-(type, verb) invocation totals
//   - FailNext queues failures that fire in order then dequeue
//   - Per-(type, verb) isolation: failure on TypeA.Patch does NOT
//     affect TypeB.Patch
//   - VerbPatch and VerbStatusPatch are tracked separately, so a
//     metadata patch and a status patch on the same object are
//     distinguishable
//   - Reset clears both counters and queued failures
//   - Underlying state is not mutated when a failure is injected
//     (object in fake store must be unchanged)

func newSelfTestClient(t *testing.T) (*selectiveFailingClient, *swiftv1alpha1.SwiftGuest, *migrationv1alpha1.SwiftMigration) {
	t.Helper()
	scheme := testScheme(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
	}
	mig := &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(guest, mig).
		WithStatusSubresource(guest, mig).
		Build()
	return newSelectiveFailingClient(base), guest, mig
}

func TestSelectiveFailingClient_TypeKeyOf_StableAndDistinguishable(t *testing.T) {
	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	migKey := typeKeyOf(&migrationv1alpha1.SwiftMigration{})
	podKey := typeKeyOf(&corev1.Pod{})

	if guestKey == migKey {
		t.Errorf("SwiftGuest and SwiftMigration keys must differ; got both %q", guestKey)
	}
	if guestKey == podKey {
		t.Errorf("SwiftGuest and Pod keys must differ; got both %q", guestKey)
	}
	if guestKey != typeKeyOf(&swiftv1alpha1.SwiftGuest{}) {
		t.Errorf("typeKeyOf must be stable across calls; got %q then %q", guestKey, typeKeyOf(&swiftv1alpha1.SwiftGuest{}))
	}
}

func TestSelectiveFailingClient_Count_TracksPerTypeVerb(t *testing.T) {
	c, guest, mig := newSelfTestClient(t)
	ctx := context.Background()

	if err := c.Get(ctx, client.ObjectKeyFromObject(guest), &swiftv1alpha1.SwiftGuest{}); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(guest), &swiftv1alpha1.SwiftGuest{}); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(mig), &migrationv1alpha1.SwiftMigration{}); err != nil {
		t.Fatalf("Get mig: %v", err)
	}

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	migKey := typeKeyOf(&migrationv1alpha1.SwiftMigration{})

	if got := c.Count(guestKey, VerbGet); got != 2 {
		t.Errorf("SwiftGuest VerbGet count: want 2, got %d", got)
	}
	if got := c.Count(migKey, VerbGet); got != 1 {
		t.Errorf("SwiftMigration VerbGet count: want 1, got %d", got)
	}
	if got := c.Count(guestKey, VerbPatch); got != 0 {
		t.Errorf("SwiftGuest VerbPatch count: want 0, got %d", got)
	}
}

func TestSelectiveFailingClient_FailNext_FiresThenDequeues(t *testing.T) {
	c, guest, _ := newSelfTestClient(t)
	ctx := context.Background()

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	injected := apierrors.NewConflict(schema.GroupResource{Group: "swift.kubeswift.io", Resource: "swiftguests"}, guest.Name, errors.New("conflict"))

	c.FailNext(guestKey, VerbPatch, 2, injected)

	patch := client.MergeFrom(guest.DeepCopy())
	guest.Annotations = map[string]string{"k": "v"}

	// First two patches fail with the injected error.
	if err := c.Patch(ctx, guest, patch); !apierrors.IsConflict(err) {
		t.Fatalf("call 1: want IsConflict, got %v", err)
	}
	if err := c.Patch(ctx, guest, patch); !apierrors.IsConflict(err) {
		t.Fatalf("call 2: want IsConflict, got %v", err)
	}
	// Third call succeeds (passes through to fake).
	if err := c.Patch(ctx, guest, patch); err != nil {
		t.Fatalf("call 3: want nil, got %v", err)
	}

	// All three calls counted, failed or not.
	if got := c.Count(guestKey, VerbPatch); got != 3 {
		t.Errorf("Count after 3 patches: want 3, got %d", got)
	}
}

func TestSelectiveFailingClient_PerTypeVerbIsolation(t *testing.T) {
	c, guest, mig := newSelfTestClient(t)
	ctx := context.Background()

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	migKey := typeKeyOf(&migrationv1alpha1.SwiftMigration{})
	injected := errors.New("guest patch broken")

	// Only SwiftGuest.Patch is set to fail; SwiftMigration.Patch must
	// pass through cleanly.
	c.FailNext(guestKey, VerbPatch, 1, injected)

	gPatch := client.MergeFrom(guest.DeepCopy())
	guest.Annotations = map[string]string{"a": "b"}
	if err := c.Patch(ctx, guest, gPatch); err != injected {
		t.Fatalf("guest patch: want injected err, got %v", err)
	}

	mPatch := client.MergeFrom(mig.DeepCopy())
	mig.Annotations = map[string]string{"c": "d"}
	if err := c.Patch(ctx, mig, mPatch); err != nil {
		t.Errorf("mig patch must succeed (no failures queued for SwiftMigration); got %v", err)
	}

	if got := c.Count(guestKey, VerbPatch); got != 1 {
		t.Errorf("SwiftGuest VerbPatch count: want 1, got %d", got)
	}
	if got := c.Count(migKey, VerbPatch); got != 1 {
		t.Errorf("SwiftMigration VerbPatch count: want 1, got %d", got)
	}
}

func TestSelectiveFailingClient_StatusPatch_TrackedSeparatelyFromMetadataPatch(t *testing.T) {
	c, guest, _ := newSelfTestClient(t)
	ctx := context.Background()

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	injected := errors.New("status patch broken")

	// Inject a failure ONLY on status-patch. Metadata patches must
	// pass through. This is the load-bearing case for cutover step 1
	// vs. finalizer-add separation.
	c.FailNext(guestKey, VerbStatusPatch, 1, injected)

	// Metadata patch (no status touch) succeeds.
	mPatch := client.MergeFrom(guest.DeepCopy())
	guest.Annotations = map[string]string{"meta": "ok"}
	if err := c.Patch(ctx, guest, mPatch); err != nil {
		t.Errorf("metadata patch must succeed; got %v", err)
	}

	// Status patch fails with injected.
	sPatch := client.MergeFrom(guest.DeepCopy())
	guest.Status.Phase = "Running"
	if err := c.Status().Patch(ctx, guest, sPatch); err != injected {
		t.Errorf("status patch: want injected err, got %v", err)
	}

	if got := c.Count(guestKey, VerbPatch); got != 1 {
		t.Errorf("metadata Patch count: want 1, got %d", got)
	}
	if got := c.Count(guestKey, VerbStatusPatch); got != 1 {
		t.Errorf("status Patch count: want 1, got %d", got)
	}
}

func TestSelectiveFailingClient_QueuedFailures_FireInOrder(t *testing.T) {
	c, guest, _ := newSelfTestClient(t)
	ctx := context.Background()

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	errA := errors.New("first batch")
	errB := errors.New("second batch")

	c.FailNext(guestKey, VerbPatch, 2, errA)
	c.FailNext(guestKey, VerbPatch, 1, errB)

	patch := client.MergeFrom(guest.DeepCopy())
	guest.Annotations = map[string]string{"k": "v"}

	if err := c.Patch(ctx, guest, patch); err != errA {
		t.Errorf("call 1: want errA, got %v", err)
	}
	if err := c.Patch(ctx, guest, patch); err != errA {
		t.Errorf("call 2: want errA, got %v", err)
	}
	if err := c.Patch(ctx, guest, patch); err != errB {
		t.Errorf("call 3: want errB, got %v", err)
	}
	if err := c.Patch(ctx, guest, patch); err != nil {
		t.Errorf("call 4 (queue empty): want nil, got %v", err)
	}
}

func TestSelectiveFailingClient_Reset_ClearsCountsAndQueue(t *testing.T) {
	c, guest, _ := newSelfTestClient(t)
	ctx := context.Background()

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	c.FailNext(guestKey, VerbPatch, 5, errors.New("queued"))

	patch := client.MergeFrom(guest.DeepCopy())
	guest.Annotations = map[string]string{"k": "v"}
	_ = c.Patch(ctx, guest, patch)

	if got := c.Count(guestKey, VerbPatch); got != 1 {
		t.Fatalf("pre-reset Count: want 1, got %d", got)
	}

	c.Reset()

	if got := c.Count(guestKey, VerbPatch); got != 0 {
		t.Errorf("post-reset Count: want 0, got %d", got)
	}
	// Queue cleared too: next call must succeed (the original FailNext
	// had 4 remaining failures before reset).
	if err := c.Patch(ctx, guest, patch); err != nil {
		t.Errorf("post-reset call must succeed (queue was cleared); got %v", err)
	}
}

func TestSelectiveFailingClient_FailedCall_DoesNotMutateInnerStore(t *testing.T) {
	c, guest, _ := newSelfTestClient(t)
	ctx := context.Background()

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})
	injected := errors.New("injected")
	c.FailNext(guestKey, VerbPatch, 1, injected)

	// Capture original ResourceVersion to confirm no inner mutation.
	original := &swiftv1alpha1.SwiftGuest{}
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(guest), original); err != nil {
		t.Fatalf("baseline Get: %v", err)
	}
	rvBefore := original.ResourceVersion

	patch := client.MergeFrom(guest.DeepCopy())
	guest.Annotations = map[string]string{"would-mutate": "yes"}
	if err := c.Patch(ctx, guest, patch); err != injected {
		t.Fatalf("Patch: want injected, got %v", err)
	}

	// Re-read from inner client (bypassing wrapper); annotation must
	// NOT be set, ResourceVersion unchanged.
	after := &swiftv1alpha1.SwiftGuest{}
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(guest), after); err != nil {
		t.Fatalf("post-fail Get: %v", err)
	}
	if after.ResourceVersion != rvBefore {
		t.Errorf("inner store mutated despite injected failure: rv before=%q after=%q",
			rvBefore, after.ResourceVersion)
	}
	if _, ok := after.Annotations["would-mutate"]; ok {
		t.Errorf("inner store mutated despite injected failure: annotation persisted")
	}
}

func TestSelectiveFailingClient_DeleteAndStatusUpdate_Tracked(t *testing.T) {
	c, guest, _ := newSelfTestClient(t)
	ctx := context.Background()

	guestKey := typeKeyOf(&swiftv1alpha1.SwiftGuest{})

	// Status Update is tracked via VerbStatusUpdate (separately from
	// VerbStatusPatch). Most controllers patch, but the wrapper should
	// support both.
	guest.Status.Phase = "Running"
	if err := c.Status().Update(ctx, guest); err != nil {
		t.Fatalf("status update: %v", err)
	}
	if got := c.Count(guestKey, VerbStatusUpdate); got != 1 {
		t.Errorf("VerbStatusUpdate count: want 1, got %d", got)
	}

	// Delete tracked under VerbDelete.
	if err := c.Delete(ctx, guest); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := c.Count(guestKey, VerbDelete); got != 1 {
		t.Errorf("VerbDelete count: want 1, got %d", got)
	}
}
