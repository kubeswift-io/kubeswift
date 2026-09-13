package swiftguestpool

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

var (
	testPoolKey    = types.NamespacedName{Namespace: "ns", Name: "p"}
	testReplicaKey = types.NamespacedName{Namespace: "ns", Name: "p-0"}
)

func failingPool() *swiftv1alpha1.SwiftGuestPool {
	return &swiftv1alpha1.SwiftGuestPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pool-uid"},
		Spec: swiftv1alpha1.SwiftGuestPoolSpec{
			Replicas: 1,
			Template: swiftv1alpha1.SwiftGuestTemplateSpec{Spec: swiftv1alpha1.SwiftGuestSpec{
				KernelRef:     &corev1.LocalObjectReference{Name: "k"},
				GuestClassRef: corev1.LocalObjectReference{Name: "cls"},
				RunPolicy:     swiftv1alpha1.RunPolicyRunning,
			}},
		},
	}
}

// stampedPoolClient stamps creationTimestamp on create from clock, as the
// apiserver would; the fake client leaves it zero.
func stampedPoolClient(clock *time.Time, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&swiftv1alpha1.SwiftGuestPool{}, &swiftv1alpha1.SwiftGuest{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetCreationTimestamp(metav1.NewTime(*clock))
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
}

func reconcilePool(t *testing.T, r *SwiftGuestPoolReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: testPoolKey})
	if err != nil {
		t.Fatalf("reconcile pool: %v", err)
	}
	return res
}

// failReplica marks p-0 as the SwiftGuest controller does on a rejection that a
// new copy of the replica would hit too, and tags it so a replacement is
// distinguishable from the original.
func failReplica(t *testing.T, c client.Client, tag, reason string) {
	t.Helper()
	ctx := context.Background()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, testReplicaKey, &g); err != nil {
		t.Fatalf("replica p-0: %v", err)
	}
	if g.Labels == nil {
		g.Labels = map[string]string{}
	}
	g.Labels["test-generation"] = tag
	if err := c.Update(ctx, &g); err != nil {
		t.Fatal(err)
	}
	g.Status.Phase = swiftv1alpha1.SwiftGuestPhaseFailed
	g.Status.Conditions = []metav1.Condition{{
		Type: "Resolved", Status: metav1.ConditionFalse, Reason: "ResolutionFailed", Message: reason,
		LastTransitionTime: metav1.Now(),
	}}
	if err := c.Status().Update(ctx, &g); err != nil {
		t.Fatal(err)
	}
}

func replicaTag(t *testing.T, c client.Client) string {
	t.Helper()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), testReplicaKey, &g); err != nil {
		t.Fatalf("replica p-0: %v", err)
	}
	return g.Labels["test-generation"]
}

// The reported churn: a replica that fails as soon as it exists was deleted and
// recreated in the same pass, and the copy failed the same way, over and over.
// A replica that has only just been created and already failed must be left in
// place for now -- visible, with its reason -- not replaced straight away.
func TestPool_FreshlyFailedReplicaIsNotReplacedStraightAway(t *testing.T) {
	clock := time.Now()
	c := stampedPoolClient(&clock, failingPool())
	r := &SwiftGuestPoolReconciler{Client: c}
	reconcilePool(t, r) // creates p-0
	failReplica(t, c, "first", "SwiftImage not found: img")

	reconcilePool(t, r)
	if tag := replicaTag(t, c); tag != "first" {
		t.Fatalf("replica p-0 was replaced the moment it failed (tag %q); a deterministic failure loops", tag)
	}
}

func replicaAttempt(t *testing.T, c client.Client) string {
	t.Helper()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), testReplicaKey, &g); err != nil {
		t.Fatalf("replica p-0: %v", err)
	}
	return g.Annotations[swiftv1alpha1.AnnotationReplacementAttempt]
}

// Each replacement that fails again waits twice as long, measured from when the
// replacement was created, and the pool requeues for exactly when it falls due.
func TestPool_ReplacementBacksOffAndDoubles(t *testing.T) {
	t0 := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	clock := t0
	c := stampedPoolClient(&clock, failingPool())
	r := &SwiftGuestPoolReconciler{Client: c, now: func() time.Time { return clock }}
	reconcilePool(t, r) // p-0 created at t0

	failReplica(t, c, "first", "SwiftImage not found: img")
	clock = t0.Add(time.Second)
	if res := reconcilePool(t, r); res.RequeueAfter != 9*time.Second {
		t.Errorf("first back-off: requeue %s, want 9s (10s from creation, 1s gone)", res.RequeueAfter)
	}
	if tag := replicaTag(t, c); tag != "first" {
		t.Fatalf("replaced inside the first 10s back-off")
	}

	clock = t0.Add(10 * time.Second)
	reconcilePool(t, r)
	if tag := replicaTag(t, c); tag == "first" {
		t.Fatal("not replaced once the 10s back-off was up")
	}
	if a := replicaAttempt(t, c); a != "1" {
		t.Errorf("replacement carries attempt %q, want 1", a)
	}

	failReplica(t, c, "second", "SwiftImage not found: img")
	clock = t0.Add(11 * time.Second)
	if res := reconcilePool(t, r); res.RequeueAfter != 19*time.Second {
		t.Errorf("second back-off: requeue %s, want 19s (20s from the replacement's creation)", res.RequeueAfter)
	}
	if tag := replicaTag(t, c); tag != "second" {
		t.Fatal("replaced inside the second, 20s back-off")
	}

	clock = t0.Add(30 * time.Second)
	reconcilePool(t, r)
	if tag := replicaTag(t, c); tag == "second" {
		t.Fatal("not replaced once the 20s back-off was up")
	}
	if a := replicaAttempt(t, c); a != "2" {
		t.Errorf("second replacement carries attempt %q, want 2", a)
	}
}

func TestReplacementBackoff_DoublesToACap(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		-1: 10 * time.Second, 0: 10 * time.Second, 1: 20 * time.Second, 2: 40 * time.Second,
		4: 160 * time.Second, 5: 5 * time.Minute, 64: 5 * time.Minute,
	} {
		if got := replacementBackoff(attempt); got != want {
			t.Errorf("attempt %d: back-off %s, want %s", attempt, got, want)
		}
	}
}

// A replica that ran for a while and then failed is not part of a failure loop:
// it is replaced at once and its replacement starts the back-off over.
func TestPool_ReplicaThatRanAWhileIsReplacedAtOnce(t *testing.T) {
	t0 := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	clock := t0
	c := stampedPoolClient(&clock, failingPool())
	r := &SwiftGuestPoolReconciler{Client: c, now: func() time.Time { return clock }}
	reconcilePool(t, r)

	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), testReplicaKey, &g); err != nil {
		t.Fatal(err)
	}
	g.Annotations[swiftv1alpha1.AnnotationReplacementAttempt] = "4" // it had a rough start
	if err := c.Update(context.Background(), &g); err != nil {
		t.Fatal(err)
	}
	failReplica(t, c, "long-lived", "launcher pod failed")

	clock = t0.Add(30 * time.Minute)
	reconcilePool(t, r)
	if tag := replicaTag(t, c); tag == "long-lived" {
		t.Fatal("a replica that ran for 30m before failing was held in back-off")
	}
	if a := replicaAttempt(t, c); a != "" {
		t.Errorf("replacement carries attempt %q, want none: the back-off starts over", a)
	}
}

// Changing the template may be the fix, so a failed replica on the old template
// is replaced at once rather than waiting out its back-off.
func TestPool_TemplateChangeReplacesAFailedReplicaAtOnce(t *testing.T) {
	t0 := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	clock := t0
	c := stampedPoolClient(&clock, failingPool())
	r := &SwiftGuestPoolReconciler{Client: c, now: func() time.Time { return clock }}
	reconcilePool(t, r)
	failReplica(t, c, "old-template", "SwiftImage not found: img")

	var pool swiftv1alpha1.SwiftGuestPool
	if err := c.Get(context.Background(), testPoolKey, &pool); err != nil {
		t.Fatal(err)
	}
	pool.Spec.Template.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k2"}
	if err := c.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}

	clock = t0.Add(time.Second)
	reconcilePool(t, r)
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), testReplicaKey, &g); err != nil {
		t.Fatal(err)
	}
	if g.Labels["test-generation"] == "old-template" {
		t.Fatal("a failed replica on the old template waited out its back-off")
	}
	if g.Annotations[swiftv1alpha1.AnnotationTemplateHash] != computeTemplateHash(&pool.Spec.Template) {
		t.Error("replacement is not on the new template")
	}
	if a := g.Annotations[swiftv1alpha1.AnnotationReplacementAttempt]; a != "" {
		t.Errorf("replacement carries attempt %q, want none", a)
	}
}

// The reason a replica keeps failing is the one thing an operator needs, so it
// goes on the pool's events, both while waiting and when replacing.
func TestPool_BackOffAndReplacementAreReportedWithTheReason(t *testing.T) {
	t0 := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	clock := t0
	c := stampedPoolClient(&clock, failingPool())
	rec := record.NewFakeRecorder(10)
	r := &SwiftGuestPoolReconciler{Client: c, Recorder: rec, now: func() time.Time { return clock }}
	reconcilePool(t, r)
	failReplica(t, c, "first", "spec.filesystems[0].source.hostPath is not permitted")

	clock = t0.Add(time.Second)
	reconcilePool(t, r)
	wantEvent(t, rec, "Warning BackOff replica p-0 failed (spec.filesystems[0].source.hostPath is not permitted); back-off 10s before replacing it")

	clock = t0.Add(10 * time.Second)
	reconcilePool(t, r)
	wantEvent(t, rec, "Warning ReplacedFailedReplica replaced failed replica p-0 (spec.filesystems[0].source.hostPath is not permitted)")
}

func wantEvent(t *testing.T, rec *record.FakeRecorder, want string) {
	t.Helper()
	select {
	case got := <-rec.Events:
		if got != want {
			t.Errorf("event:\n got  %q\n want %q", got, want)
		}
	default:
		t.Errorf("no event; want %q", want)
	}
}
