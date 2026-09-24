package swiftguestpool

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// markAllServing marks every replica as its launcher would once the VM is up --
// except one on the "broken" kernel, which never comes up.
func markAllServing(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	var l swiftv1alpha1.SwiftGuestList
	if err := c.List(ctx, &l); err != nil {
		t.Fatal(err)
	}
	for i := range l.Items {
		g := &l.Items[i]
		if guestReady(g) || (g.Spec.KernelRef != nil && g.Spec.KernelRef.Name == "broken") {
			continue
		}
		g.Status.Phase = swiftv1alpha1.SwiftGuestPhaseRunning
		g.Status.Conditions = []metav1.Condition{{
			Type: "GuestRunning", Status: metav1.ConditionTrue, Reason: "VmRunning", LastTransitionTime: metav1.Now(),
		}}
		if err := c.Status().Update(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
}

// replicaHashes maps each replica to its template hash; servingCount counts
// the replicas that are serving.
func replicaHashes(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var l swiftv1alpha1.SwiftGuestList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, g := range l.Items {
		m[g.Name] = g.Annotations[swiftv1alpha1.AnnotationTemplateHash]
	}
	return m
}

func servingCount(t *testing.T, c client.Client) int {
	t.Helper()
	var l swiftv1alpha1.SwiftGuestList
	if err := c.List(context.Background(), &l); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i := range l.Items {
		if guestReady(&l.Items[i]) {
			n++
		}
	}
	return n
}

// rolloutPool is a two-replica pool with the given rolling-update bounds, its
// replicas created and serving.
func rolloutPool(t *testing.T, maxUnavailable, maxSurge int32) (*SwiftGuestPoolReconciler, client.Client) {
	t.Helper()
	clock := time.Now()
	pool := failingPool()
	pool.Spec.Replicas = 2
	pool.Spec.UpdateStrategy = &swiftv1alpha1.UpdateStrategy{
		Type:          swiftv1alpha1.UpdateStrategyRollingUpdate,
		RollingUpdate: &swiftv1alpha1.RollingUpdateConfig{MaxUnavailable: maxUnavailable, MaxSurge: maxSurge},
	}
	c := stampedPoolClient(&clock, pool)
	r := &SwiftGuestPoolReconciler{Client: c, now: func() time.Time { return clock }}
	reconcilePool(t, r)
	markAllServing(t, c)
	return r, c
}

// changeKernel edits the pool template and returns the new template hash.
func changeKernel(t *testing.T, c client.Client, kernel string) string {
	t.Helper()
	ctx := context.Background()
	var p swiftv1alpha1.SwiftGuestPool
	if err := c.Get(ctx, testPoolKey, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Template.Spec.KernelRef = &corev1.LocalObjectReference{Name: kernel}
	if err := c.Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	return computeTemplateHash(&p.Spec.Template)
}

func allOn(t *testing.T, c client.Client, hash string, want int) bool {
	t.Helper()
	h := replicaHashes(t, c)
	if len(h) != want {
		return false
	}
	for _, v := range h {
		if v != hash {
			return false
		}
	}
	return true
}

// With maxUnavailable 1, a replica whose replacement is still starting counts
// as unavailable. The replacement is created in one pass and is not serving
// yet; the rollout used to count only the replicas present, saw nothing
// unavailable, and deleted the second replica too -- taking the pool down.
func TestRollingUpdate_NeverTakesDownMoreThanMaxUnavailable(t *testing.T) {
	r, c := rolloutPool(t, 1, 0)
	newHash := changeKernel(t, c, "k2")

	for i := 0; i < 20 && !allOn(t, c, newHash, 2); i++ {
		reconcilePool(t, r)
		if n := servingCount(t, c); n < 1 {
			t.Fatalf("pass %d: %d replica(s) serving; maxUnavailable 1 of 2 must keep one up (%v)", i, n, replicaHashes(t, c))
		}
		// Replacements only come up after the check: a pass must not rely on
		// one it just created.
		markAllServing(t, c)
	}
	if !allOn(t, c, newHash, 2) {
		t.Fatalf("rollout did not complete: %v", replicaHashes(t, c))
	}
}

// maxUnavailable 0 / maxSurge 1, documented as the zero-downtime setting, never
// made progress: nothing could go down, and the "surge" path only filled
// missing indices below desired, of which there were none.
func TestRollingUpdate_ZeroUnavailableSurgeOneCompletesAtFullCapacity(t *testing.T) {
	r, c := rolloutPool(t, 0, 1)
	newHash := changeKernel(t, c, "k2")

	for i := 0; i < 30 && !allOn(t, c, newHash, 2); i++ {
		reconcilePool(t, r)
		if n := servingCount(t, c); n < 2 {
			t.Fatalf("pass %d: %d replica(s) serving; maxUnavailable 0 must keep both up (%v)", i, n, replicaHashes(t, c))
		}
		if n := len(replicaHashes(t, c)); n > 3 {
			t.Fatalf("pass %d: %d replicas exist; maxSurge 1 allows at most 3", i, n)
		}
		markAllServing(t, c)
	}
	if !allOn(t, c, newHash, 2) {
		t.Fatalf("zero-downtime rollout did not complete (or left its surge replica): %v", replicaHashes(t, c))
	}
}

// Rolling back a rollout whose new replicas never became ready used to
// deadlock: the broken replica was the unavailable one, so the budget was spent
// and nothing -- including the broken replica itself -- could be replaced.
func TestRollingUpdate_RollbackOfAnUnreadyRolloutCompletes(t *testing.T) {
	r, c := rolloutPool(t, 1, 0)
	goodHash := replicaHashes(t, c)["p-0"]

	changeKernel(t, c, "broken")
	for i := 0; i < 5; i++ {
		reconcilePool(t, r)
		markAllServing(t, c) // the broken replicas never become ready
	}
	changeKernel(t, c, "k") // roll back to the original template
	for i := 0; i < 20 && !allOn(t, c, goodHash, 2); i++ {
		reconcilePool(t, r)
		markAllServing(t, c)
	}
	if !allOn(t, c, goodHash, 2) {
		t.Fatalf("rollback did not complete: %v (want all on %s)", replicaHashes(t, c), goodHash)
	}
}

// A surge replica is only temporary capacity: once the rollout is done and the
// replicas it covered are serving, it is removed.
func TestRollingUpdate_SurgeReplicaIsRemovedAfterRollout(t *testing.T) {
	r, c := rolloutPool(t, 1, 1)
	newHash := changeKernel(t, c, "k2")
	for i := 0; i < 20; i++ {
		reconcilePool(t, r)
		markAllServing(t, c)
	}
	if !allOn(t, c, newHash, 2) {
		t.Fatalf("want exactly p-0 and p-1 on the new template, got %v", replicaHashes(t, c))
	}
}

// Both bounds 0 cannot make progress; it must stall visibly, not delete.
func TestRollingUpdate_BothBoundsZeroDoesNotDelete(t *testing.T) {
	r, c := rolloutPool(t, 0, 0)
	before := replicaHashes(t, c)
	changeKernel(t, c, "k2")
	for i := 0; i < 5; i++ {
		reconcilePool(t, r)
	}
	after := replicaHashes(t, c)
	for name, h := range before {
		if after[name] != h {
			t.Errorf("%s changed from %s to %s with maxUnavailable=maxSurge=0", name, h, after[name])
		}
	}
}
