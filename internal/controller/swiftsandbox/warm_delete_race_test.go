package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// claimSlot relabels a warm slot as a checkout does.
func claimSlot(ctx context.Context, c client.Client, name string) error {
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &p); err != nil {
		return err
	}
	p.Labels[SlotStateLabelKey] = slotStateClaimed
	p.Labels[SandboxLabelKey] = "sb"
	return c.Update(ctx, &p)
}

// The pool deletes from a warm set it read earlier. A slot a checkout claimed
// in between must survive: deleting it failed that checkout with SlotLost.
func TestDeleteWarmSlot_LeavesASlotClaimedSinceItWasRead(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(poolPod("gp-slot-aaaaa")).Build()
	var stale corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gp-slot-aaaaa"}, &stale); err != nil {
		t.Fatal(err)
	}
	if err := claimSlot(ctx, c, "gp-slot-aaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := deleteWarmSlot(ctx, c, &stale); err != nil {
		t.Fatalf("deleteWarmSlot: %v", err)
	}
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gp-slot-aaaaa"}, &p); err != nil {
		t.Fatalf("the claimed slot was deleted: %v", err)
	}
}

func TestDeleteWarmSlot_DeletesAnUnchangedWarmSlot(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(poolPod("gp-slot-aaaaa")).Build()
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gp-slot-aaaaa"}, &p); err != nil {
		t.Fatal(err)
	}
	if err := deleteWarmSlot(ctx, c, &p); err != nil {
		t.Fatalf("deleteWarmSlot: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&p), &p); err == nil {
		t.Fatal("warm slot not deleted")
	}
	if err := deleteWarmSlot(ctx, c, &p); err != nil {
		t.Errorf("deleting a slot already gone: %v", err)
	}
}

// Pool deletion sweeps warm slots the same way: one claimed mid-sweep stays
// with its sandbox.
func TestDeleteWarmSlots_SkipsASlotClaimedMidSweep(t *testing.T) {
	ctx := context.Background()
	claimedOnce := false
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(poolPod("gp-slot-aaaaa"), poolPod("gp-slot-bbbbb")).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "gp-slot-aaaaa" && !claimedOnce {
					claimedOnce = true
					if err := claimSlot(ctx, cl, "gp-slot-aaaaa"); err != nil {
						return err
					}
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	pool := &sandboxv1alpha1.SwiftSandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "gp", Namespace: "default"}}

	if err := r.deleteWarmSlots(ctx, pool); err != nil {
		t.Fatalf("deleteWarmSlots: %v", err)
	}
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gp-slot-aaaaa"}, &p); err != nil {
		t.Errorf("slot claimed mid-sweep was deleted: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gp-slot-bbbbb"}, &p); err == nil {
		t.Error("the untouched warm slot was not deleted")
	}
}
