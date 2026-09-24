package swiftguest

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// status.conditions is an atomic list: a status patch that changes any
// condition resends all of them. From a stale read it put back the
// GuestRunning that swiftletd had just changed. The patch must conflict
// instead, so the next pass works from the newer object.
func TestPatchStatus_StaleReadCannotOverwriteGuestRunning(t *testing.T) {
	ctx := context.Background()
	g := kernelGuest()
	g.Status.Conditions = []metav1.Condition{{Type: "GuestRunning", Status: metav1.ConditionFalse, Reason: "GuestStarting", LastTransitionTime: metav1.Now()}}
	c := guestClientBuilder(g).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	var stale swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, testGuestKey, &stale); err != nil {
		t.Fatal(err)
	}
	// swiftletd reports the VM up in between.
	var fresh swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, testGuestKey, &fresh); err != nil {
		t.Fatal(err)
	}
	fresh.Status.Conditions[0].Status = metav1.ConditionTrue
	fresh.Status.Conditions[0].Reason = "VmRunning"
	if err := c.Status().Update(ctx, &fresh); err != nil {
		t.Fatal(err)
	}

	// The controller, from its stale copy, changes another condition.
	status := stale.Status.DeepCopy()
	setCondition(status, metav1.Condition{Type: ConditionStorageReady, Status: metav1.ConditionTrue, Reason: "StorageReady"})
	if err := r.patchStatus(ctx, &stale, status); !apierrors.IsConflict(err) {
		t.Fatalf("stale status patch err = %v, want a conflict", err)
	}
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, testGuestKey, &got); err != nil {
		t.Fatal(err)
	}
	if cond := findCondition(&got.Status, "GuestRunning"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("GuestRunning = %+v; a stale controller write reverted swiftletd's report", cond)
	}
}

// The lock must never trip on the controller's own writes within a pass (a
// patch of a copy that leaves the reconcile's object at the old
// resourceVersion would conflict on every pass and never make progress).
func TestReconcile_NoSelfConflictOnStatusPatch(t *testing.T) {
	conflicts := 0
	c := guestClientBuilder(kernelGuest(), testGuestClass(), readyKernel(), runningLauncher("worker-1")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				err := cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
				if apierrors.IsConflict(err) {
					conflicts++
				}
				return err
			},
		}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	for i := 0; i < 3; i++ {
		if _, _, err := reconcileGuest(t, r); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if conflicts != 0 {
		t.Errorf("%d status-patch conflicts across quiet reconciles; the controller conflicts with itself", conflicts)
	}
}
