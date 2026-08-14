package swiftgpu

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// nodeAllocatedTo builds a SwiftGPUNode whose single GPU is allocated to the
// given guest key.
func nodeAllocatedTo(name, pci, guestKey string) *gpuv1alpha1.SwiftGPUNode {
	return &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			VfioReady: true,
			GPUModel:  "GeForce GTX 1080",
			GPUs: []gpuv1alpha1.GPUDevice{
				{Index: 0, PCIAddress: pci, Vendor: "NVIDIA", Model: "GeForce GTX 1080", Allocated: true, AllocatedTo: guestKey},
			},
		},
	}
}

func wgpu1Profile() *gpuv1alpha1.SwiftGPUProfile {
	return &gpuv1alpha1.SwiftGPUProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Spec:       gpuv1alpha1.SwiftGPUProfileSpec{Count: 1, PartitionMode: "isolated"},
	}
}

func wgpu1Guest(statusNode string) *swiftv1alpha1.SwiftGuest {
	g := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"}}
	if statusNode != "" {
		g.Status.GPU = &swiftv1alpha1.GPUStatus{NodeName: statusNode}
	}
	return g
}

// TestFindAndAllocate_PrefersStatusGPUNode is the W-GPU-1 regression: during a
// VFIO migration's reserve-before-stop double-hold the guest is allocated on
// BOTH "worker-1" (first in the list) and "worker-2". findAndAllocate must return the
// node status.GPU references, NOT the first-found node — otherwise the SwiftGPU
// controller re-stamps status.GPU and races the migration controller.
func TestFindAndAllocate_PrefersStatusGPUNode(t *testing.T) {
	worker1 := nodeAllocatedTo("worker-1", "0000:01:00.0", "default/g")
	worker2 := nodeAllocatedTo("worker-2", "0000:ff:00.0", "default/g")
	profile := wgpu1Profile()

	t.Run("status.GPU=worker-2 returns worker-2 (not first-found worker-1)", func(t *testing.T) {
		r := newReconciler(worker1, worker2, wgpu1Guest("worker-2"))
		node, _, _, _, err := r.findAndAllocate(context.Background(), wgpu1Guest("worker-2"), profile)
		if err != nil {
			t.Fatalf("findAndAllocate: %v", err)
		}
		if node == nil || node.Name != "worker-2" {
			t.Fatalf("must prefer the status.GPU node (worker-2); got %v", node)
		}
	})

	t.Run("status.GPU=worker-1 returns worker-1", func(t *testing.T) {
		r := newReconciler(worker1, worker2, wgpu1Guest("worker-1"))
		node, _, _, _, err := r.findAndAllocate(context.Background(), wgpu1Guest("worker-1"), profile)
		if err != nil {
			t.Fatalf("findAndAllocate: %v", err)
		}
		if node == nil || node.Name != "worker-1" {
			t.Fatalf("must prefer the status.GPU node (worker-1); got %v", node)
		}
	})

	t.Run("no status.GPU falls back to first-found", func(t *testing.T) {
		// Single allocation (no double-hold), no status.GPU → returns it.
		r := newReconciler(worker1, wgpu1Guest(""))
		node, _, _, _, err := r.findAndAllocate(context.Background(), wgpu1Guest(""), profile)
		if err != nil {
			t.Fatalf("findAndAllocate: %v", err)
		}
		if node == nil || node.Name != "worker-1" {
			t.Fatalf("with no status.GPU and one allocation, returns that node; got %v", node)
		}
	})
}

// TestDeallocateGPUs_FreesAllNodes is the §10.1 regression: a guest allocated on
// BOTH the source ("worker-2", = status.GPU.NodeName) and the target ("worker-1", a
// held reservation) — the reserve-before-stop double-hold — must have BOTH
// freed when the guest is deleted. Releasing only status.GPU.NodeName would
// strand the target reservation forever (AllocatedTo a now-deleted guest).
func TestDeallocateGPUs_FreesAllNodes(t *testing.T) {
	source := nodeAllocatedTo("worker-2", "0000:01:00.0", "default/g")
	target := nodeAllocatedTo("worker-1", "0000:ff:00.0", "default/g")
	guest := wgpu1Guest("worker-2") // status.GPU.NodeName = worker2 (source)
	r := newReconciler(source, target, guest)

	if err := r.deallocateGPUs(context.Background(), guest); err != nil {
		t.Fatalf("deallocateGPUs: %v", err)
	}
	for _, name := range []string{"worker-2", "worker-1"} {
		n := getNode(t, r.Client, name)
		if got := n.Status.GPUs[0].AllocatedTo; got != "" {
			t.Errorf("node %q GPU still AllocatedTo %q after dealloc; reservation/allocation leaked", name, got)
		}
		if n.Status.FreeGPUs != 1 {
			t.Errorf("node %q FreeGPUs=%d, want 1 after dealloc", name, n.Status.FreeGPUs)
		}
	}
}
