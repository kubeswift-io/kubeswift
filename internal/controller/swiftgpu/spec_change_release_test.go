package swiftgpu

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// allocateOne sets up guest "g" with one native GPU allocated on node n1.
func allocateOne(t *testing.T) *SwiftGPUReconciler {
	t.Helper()
	guest := testSwiftGuest("g", "default", &corev1.LocalObjectReference{Name: "p"})
	r := newReconciler(guest, testGPUProfile("p", "default", "pcie", "", 1, ""), testGPUNode("n1", eightGPUs(), nil))
	for i := 0; i < 2; i++ { // finalizer, then allocation
		if _, err := reconcileGuest(r, "g", "default"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := getGPUNode(r, "n1"); n.Status.FreeGPUs != 7 {
		t.Fatalf("setup: want 7 free after allocating one, got %d", n.Status.FreeGPUs)
	}
	return r
}

func allocatedToGuest(t *testing.T, r *SwiftGPUReconciler) int {
	t.Helper()
	n, err := getGPUNode(r, "n1")
	if err != nil {
		t.Fatal(err)
	}
	c := 0
	for _, g := range n.Status.GPUs {
		if g.AllocatedTo == "default/g" {
			c++
		}
	}
	return c
}

// Removing gpuProfileRef after allocation and then deleting the guest must
// still release the GPU and remove the finalizer. The controller used to key
// everything off the current spec, return early for "no GPU requested", and
// leave the guest stuck Terminating with its GPU allocated forever.
func TestRelease_RefRemovedThenDeleted(t *testing.T) {
	r := allocateOne(t)
	ctx := context.Background()

	g, _ := getGuest(r, "g", "default")
	g.Spec.GPUProfileRef = nil
	if err := r.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, g); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := reconcileGuest(r, "g", "default"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := getGuest(r, "g", "default"); !apierrors.IsNotFound(err) {
		t.Errorf("guest should be gone (finalizer removed), got err=%v", err)
	}
	if n := allocatedToGuest(t, r); n != 0 {
		t.Errorf("%d GPU(s) still AllocatedTo the deleted guest", n)
	}
}

// Removing gpuProfileRef from a guest that is NOT being deleted returns its
// native GPU once no launcher holds it, clears the stale allocation from
// status, and drops the now-pointless finalizer.
func TestRelease_RefRemovedWhileNotDeleting(t *testing.T) {
	r := allocateOne(t)
	ctx := context.Background()

	g, _ := getGuest(r, "g", "default")
	g.Spec.GPUProfileRef = nil
	if err := r.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileGuest(r, "g", "default"); err != nil {
		t.Fatal(err)
	}
	if n := allocatedToGuest(t, r); n != 0 {
		t.Errorf("GPU no longer requested should be released, %d still allocated", n)
	}
	g, _ = getGuest(r, "g", "default")
	if g.Status.GPU != nil {
		t.Errorf("stale status.gpu should be cleared, got %+v", g.Status.GPU)
	}
	if apimeta.FindStatusCondition(g.Status.Conditions, swiftv1alpha1.ConditionGPUAllocated) != nil {
		t.Error("stale GPUAllocated condition should be removed")
	}
	for _, f := range g.Finalizers {
		if f == GPUFinalizerName {
			t.Error("GPU finalizer should be removed once nothing is requested or held")
		}
	}
}
