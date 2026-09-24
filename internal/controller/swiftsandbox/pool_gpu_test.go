package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func gpuPool(name, ns, profileName string) *sandboxv1alpha1.SwiftSandboxPool {
	return &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sandboxv1alpha1.SwiftSandboxPoolSpec{
			Image: "cuda:12", MinWarm: 1,
			GPUProfileRef: &corev1.LocalObjectReference{Name: profileName},
		},
	}
}

func TestAllocateSlotGPU_StampsSlotAndConsumesNode(t *testing.T) {
	node := oneGPUNode("worker-1")
	profile := pcieProfile("gtx", "default")
	pool := gpuPool("gp", "default", "gtx")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, profile).WithStatusSubresource(node).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}

	slot := r.slotTemplate(pool, "gp-slot-aaaaa")
	if err := r.allocateSlotGPU(context.Background(), pool, slot); err != nil {
		t.Fatalf("allocateSlotGPU: %v", err)
	}
	if slot.Spec.GPUProfileRef == nil || slot.Status.GPU == nil ||
		len(slot.Status.GPU.Devices) != 1 || slot.Status.GPU.Devices[0] != "0000:01:00.0" ||
		slot.Status.GPU.NodeName != "worker-1" {
		t.Fatalf("slot not stamped for GPU: spec.gpuProfileRef=%v status.gpu=%+v", slot.Spec.GPUProfileRef, slot.Status.GPU)
	}
	var after gpuv1alpha1.SwiftGPUNode
	_ = c.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &after)
	if after.Status.FreeGPUs != 0 || after.Status.GPUs[0].AllocatedTo != "sandbox:default/gp-slot-aaaaa" {
		t.Errorf("node not allocated to the slot: free=%d allocatedTo=%q", after.Status.FreeGPUs, after.Status.GPUs[0].AllocatedTo)
	}
}

func TestAllocateSlotGPU_RejectsHGXTier(t *testing.T) {
	node := oneGPUNode("worker-1")
	profile := pcieProfile("hgx", "default")
	profile.Spec.Tier = "hgx-shared"
	pool := gpuPool("gp", "default", "hgx")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, profile).WithStatusSubresource(node).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}

	err := r.allocateSlotGPU(context.Background(), pool, r.slotTemplate(pool, "gp-slot-x"))
	if err == nil {
		t.Fatal("hgx tier must be rejected for a warm GPU pool")
	}
}

// poolPod is a slot pod of pool gp (warm unless state says otherwise).
func poolPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default",
		Labels: map[string]string{PoolLabelKey: "gp", SlotStateLabelKey: slotStateWarm},
	}}
}

func allocatedNode(allocatedTo string) *gpuv1alpha1.SwiftGPUNode {
	node := oneGPUNode("worker-1")
	node.Status.FreeGPUs = 0
	node.Status.GPUs[0].Allocated = true
	node.Status.GPUs[0].AllocatedTo = allocatedTo
	return node
}

func nodeAllocatedTo(t *testing.T, c client.Client) string {
	t.Helper()
	var n gpuv1alpha1.SwiftGPUNode
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &n); err != nil {
		t.Fatal(err)
	}
	return n.Status.GPUs[0].AllocatedTo
}

// The GC sweep releases the GPU of a slot whose pod is gone, and keeps the GPU
// of a slot whose pod still exists.
func TestReconcileSlotGPUGC_ReleasesOrphanedSlots(t *testing.T) {
	pool := gpuPool("gp", "default", "gtx")

	// No live pod → the dead slot's GPU is released.
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(allocatedNode("sandbox:default/gp-slot-dead0")).WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	if held, err := r.reconcileSlotGPUGC(context.Background(), pool); err != nil || held {
		t.Fatalf("held=%v err=%v", held, err)
	}
	if got := nodeAllocatedTo(t, c); got != "" {
		t.Fatalf("orphaned slot GPU not released: allocatedTo=%q", got)
	}

	// A live pod → the GPU is KEPT, and reported held.
	c = fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(allocatedNode("sandbox:default/gp-slot-live0"), poolPod("gp-slot-live0")).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r = &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	held, err := r.reconcileSlotGPUGC(context.Background(), pool)
	if err != nil || !held {
		t.Fatalf("a live slot must be reported held: held=%v err=%v", held, err)
	}
	if got := nodeAllocatedTo(t, c); got != "sandbox:default/gp-slot-live0" {
		t.Errorf("a live slot's GPU must NOT be released, got allocatedTo=%q", got)
	}
}

// Allocations that merely share the "<pool>-slot-" prefix belong to someone
// else and must never be freed: a standalone sandbox named like a slot, or the
// slots of a pool whose own name starts with "<pool>-slot-".
func TestReconcileSlotGPUGC_IgnoresOtherWorkloadsSharingThePrefix(t *testing.T) {
	pool := gpuPool("gp", "default", "gtx")
	for _, owner := range []string{
		"sandbox:default/gp-slot-x",              // standalone sandbox "gp-slot-x"
		"sandbox:default/gp-slot-b-slot-abcde",   // a slot of pool "gp-slot-b"
		"sandbox:default/gp-slot-toolongsuffix0", // not a 5-char slot suffix
	} {
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(allocatedNode(owner)).WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
		r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
		if _, err := r.reconcileSlotGPUGC(context.Background(), pool); err != nil {
			t.Fatal(err)
		}
		if got := nodeAllocatedTo(t, c); got != owner {
			t.Errorf("GPU of %q was freed by pool gp's GC", owner)
		}
	}

	// Even an exactly slot-shaped name is left alone while a standalone
	// SwiftSandbox by that name exists — its own controller releases it.
	sb := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{Name: "gp-slot-abcde", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(allocatedNode("sandbox:default/gp-slot-abcde"), sb).WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	if _, err := r.reconcileSlotGPUGC(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if got := nodeAllocatedTo(t, c); got != "sandbox:default/gp-slot-abcde" {
		t.Error("the GPU of a standalone sandbox with a slot-shaped name was freed")
	}
}

// Deleting a GPU pool must not free the GPU of a claimed slot whose checkout is
// still running: the finalizer stays (requeue) while that pod exists, idle warm
// slots are deleted outright, and the GPU is released only once the claimed pod
// is gone. Deletion used to pass an EMPTY live set and free every slot's GPU,
// handing a device that was in use to the next consumer.
func TestPoolDeletion_WaitsForClaimedSlotBeforeReleasingItsGPU(t *testing.T) {
	ctx := context.Background()
	pool := gpuPool("gp", "default", "gtx")
	pool.Finalizers = []string{poolGPUFinalizer}
	claimed := poolPod("gp-slot-busy0")
	claimed.Labels[SlotStateLabelKey] = slotStateClaimed
	warm := poolPod("gp-slot-idle0")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(pool, claimed, warm, allocatedNode("sandbox:default/gp-slot-busy0")).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}
	if err := c.Delete(ctx, pool); err != nil { // sets deletionTimestamp (finalizer holds it)
		t.Fatal(err)
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Error("deletion should requeue while a claimed slot still holds its GPU")
	}
	if got := nodeAllocatedTo(t, c); got != "sandbox:default/gp-slot-busy0" {
		t.Fatalf("a running checkout's GPU was released during pool deletion (allocatedTo=%q)", got)
	}
	var p sandboxv1alpha1.SwiftSandboxPool
	if err := c.Get(ctx, req.NamespacedName, &p); err != nil {
		t.Fatalf("pool should still exist (finalizer held): %v", err)
	}
	var w corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(warm), &w); !apierrors.IsNotFound(err) {
		t.Errorf("idle warm slot should be deleted during pool deletion, got err=%v", err)
	}

	// The checkout ends: its pod goes away. Now the GPU is released and the
	// finalizer removed.
	if err := c.Delete(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := nodeAllocatedTo(t, c); got != "" {
		t.Errorf("GPU should be released once the claimed pod is gone, allocatedTo=%q", got)
	}
	if err := c.Get(ctx, req.NamespacedName, &p); !apierrors.IsNotFound(err) {
		t.Errorf("pool should be gone once nothing is held, got err=%v", err)
	}
}
