package swiftsandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The defect: a pool that wants a slot and cannot resolve its image re-issued a
// manifest GET every poll interval, forever. Against Docker Hub's anonymous
// allowance of 100/hour that is ~360 requests an hour spent arguing with a
// registry that has already answered 429.
func TestResolveBackoff_DoublesAndCaps(t *testing.T) {
	r := &SwiftSandboxPoolReconciler{}
	key := types.NamespacedName{Namespace: "ns", Name: "pool"}

	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 320 * time.Second,
		10 * time.Minute, // capped
		10 * time.Minute,
	}
	for i, w := range want {
		if got := r.nextResolveBackoff(key); got != w {
			t.Errorf("failure %d: backoff = %v, want %v", i+1, got, w)
		}
	}
}

// A capped pool must stay under the anonymous hourly allowance; the old flat
// interval did not.
func TestResolveBackoff_RequestsPerHourUnderAnonymousLimit(t *testing.T) {
	r := &SwiftSandboxPoolReconciler{}
	key := types.NamespacedName{Namespace: "ns", Name: "pool"}

	var elapsed time.Duration
	attempts := 0
	for elapsed < time.Hour {
		elapsed += r.nextResolveBackoff(key)
		attempts++
	}
	if attempts > 100 {
		t.Errorf("%d attempts in the first hour, which is over Docker Hub's anonymous allowance of 100", attempts)
	}
	// The old behaviour, for contrast: 3600/10 = 360.
	if attempts >= 360 {
		t.Errorf("%d attempts is no better than the flat 10s poll it replaces", attempts)
	}
	t.Logf("attempts in the first hour: %d (was ~360)", attempts)
}

// A success must clear the streak, so an unrelated failure later does not
// inherit a 10-minute wait.
func TestResolveBackoff_ResetsOnSuccess(t *testing.T) {
	r := &SwiftSandboxPoolReconciler{}
	key := types.NamespacedName{Namespace: "ns", Name: "pool"}

	for i := 0; i < 6; i++ {
		r.nextResolveBackoff(key)
	}
	r.clearResolveBackoff(key)
	if got := r.nextResolveBackoff(key); got != 10*time.Second {
		t.Errorf("after a success the next failure should start at the base wait, got %v", got)
	}
}

// Pools back off independently; one rate-limited pool must not slow another.
func TestResolveBackoff_PerPool(t *testing.T) {
	r := &SwiftSandboxPoolReconciler{}
	a := types.NamespacedName{Namespace: "ns", Name: "a"}
	b := types.NamespacedName{Namespace: "ns", Name: "b"}

	for i := 0; i < 5; i++ {
		r.nextResolveBackoff(a)
	}
	if got := r.nextResolveBackoff(b); got != 10*time.Second {
		t.Errorf("pool b inherited pool a's backoff: %v", got)
	}
}

// End to end through Reconcile: a pool that wants a slot and cannot resolve its
// image must requeue further out each time, not at the flat poll interval. An
// unparseable reference fails inside resolveImage exactly where a 429 does, and
// keeps the test off the network.
func TestPoolReconcile_ResolveFailureBacksOff(t *testing.T) {
	s := scheme.Scheme
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"},
		// MinWarm 1 so a slot is wanted, and no digest yet, so it resolves.
		Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "!!! not a ref !!!", MinWarm: 1},
	}
	// A Ready kernel, so warming gets past the kernel check to the resolve.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pool, readyKernel("ns", defaultKernelProfile)).
		WithStatusSubresource(pool).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, APIReader: c, Scheme: s, Recorder: record.NewFakeRecorder(20)}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}}

	var got []time.Duration
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		got = append(got, res.RequeueAfter)
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reconcile %d requeued after %v, want %v (flat %v is the bug)",
				i+1, got[i], want[i], poolPollInterval)
		}
	}

	// The wait has to be visible, not just internal.
	var after sandboxv1alpha1.SwiftSandboxPool
	if err := c.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Status.Message, "next attempt in") {
		t.Errorf("Degraded message should say when it retries, got %q", after.Status.Message)
	}
}
