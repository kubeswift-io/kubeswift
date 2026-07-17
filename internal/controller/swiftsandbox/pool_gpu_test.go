package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	node := oneGPUNode("boba")
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
		slot.Status.GPU.NodeName != "boba" {
		t.Fatalf("slot not stamped for GPU: spec.gpuProfileRef=%v status.gpu=%+v", slot.Spec.GPUProfileRef, slot.Status.GPU)
	}
	var after gpuv1alpha1.SwiftGPUNode
	_ = c.Get(context.Background(), client.ObjectKey{Name: "boba"}, &after)
	if after.Status.FreeGPUs != 0 || after.Status.GPUs[0].AllocatedTo != "sandbox:default/gp-slot-aaaaa" {
		t.Errorf("node not allocated to the slot: free=%d allocatedTo=%q", after.Status.FreeGPUs, after.Status.GPUs[0].AllocatedTo)
	}
}

func TestAllocateSlotGPU_RejectsHGXTier(t *testing.T) {
	node := oneGPUNode("boba")
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

// The GC sweep releases the GPU of a slot whose pod is gone, and keeps the GPU
// of a slot whose pod still exists.
func TestReconcileSlotGPUGC_ReleasesOrphanedSlots(t *testing.T) {
	node := oneGPUNode("boba")
	// Pre-allocate the GPU to a slot whose pod is gone.
	node.Status.FreeGPUs = 0
	node.Status.GPUs[0].Allocated = true
	node.Status.GPUs[0].AllocatedTo = "sandbox:default/gp-slot-dead"
	pool := gpuPool("gp", "default", "gtx")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node).WithStatusSubresource(node).Build()
	r := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme}

	// No live pods → the dead slot's GPU is released.
	if err := r.reconcileSlotGPUGC(context.Background(), pool, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	var after gpuv1alpha1.SwiftGPUNode
	_ = c.Get(context.Background(), client.ObjectKey{Name: "boba"}, &after)
	if after.Status.FreeGPUs != 1 || after.Status.GPUs[0].AllocatedTo != "" {
		t.Fatalf("orphaned slot GPU not released: free=%d allocatedTo=%q", after.Status.FreeGPUs, after.Status.GPUs[0].AllocatedTo)
	}

	// Re-allocate, then a live pod → the GPU is KEPT.
	after.Status.FreeGPUs = 0
	after.Status.GPUs[0].Allocated = true
	after.Status.GPUs[0].AllocatedTo = "sandbox:default/gp-slot-live"
	_ = c.Status().Update(context.Background(), &after)
	if err := r.reconcileSlotGPUGC(context.Background(), pool, map[string]bool{"gp-slot-live": true}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKey{Name: "boba"}, &after)
	if after.Status.GPUs[0].AllocatedTo != "sandbox:default/gp-slot-live" {
		t.Errorf("a live slot's GPU must NOT be released, got allocatedTo=%q", after.Status.GPUs[0].AllocatedTo)
	}
}
