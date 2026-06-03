package swiftgpu

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpuv1alpha1 "github.com/projectbeskar/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
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
// BOTH "boba" (first in the list) and "miles". findAndAllocate must return the
// node status.GPU references, NOT the first-found node — otherwise the SwiftGPU
// controller re-stamps status.GPU and races the migration controller.
func TestFindAndAllocate_PrefersStatusGPUNode(t *testing.T) {
	boba := nodeAllocatedTo("boba", "0000:01:00.0", "default/g")
	miles := nodeAllocatedTo("miles", "0000:ff:00.0", "default/g")
	profile := wgpu1Profile()

	t.Run("status.GPU=miles returns miles (not first-found boba)", func(t *testing.T) {
		r := newReconciler(boba, miles, wgpu1Guest("miles"))
		node, _, _, _, err := r.findAndAllocate(context.Background(), wgpu1Guest("miles"), profile)
		if err != nil {
			t.Fatalf("findAndAllocate: %v", err)
		}
		if node == nil || node.Name != "miles" {
			t.Fatalf("must prefer the status.GPU node (miles); got %v", node)
		}
	})

	t.Run("status.GPU=boba returns boba", func(t *testing.T) {
		r := newReconciler(boba, miles, wgpu1Guest("boba"))
		node, _, _, _, err := r.findAndAllocate(context.Background(), wgpu1Guest("boba"), profile)
		if err != nil {
			t.Fatalf("findAndAllocate: %v", err)
		}
		if node == nil || node.Name != "boba" {
			t.Fatalf("must prefer the status.GPU node (boba); got %v", node)
		}
	})

	t.Run("no status.GPU falls back to first-found", func(t *testing.T) {
		// Single allocation (no double-hold), no status.GPU → returns it.
		r := newReconciler(boba, wgpu1Guest(""))
		node, _, _, _, err := r.findAndAllocate(context.Background(), wgpu1Guest(""), profile)
		if err != nil {
			t.Fatalf("findAndAllocate: %v", err)
		}
		if node == nil || node.Name != "boba" {
			t.Fatalf("with no status.GPU and one allocation, returns that node; got %v", node)
		}
	})
}

// TestDeallocateGPUs_FreesAllNodes is the §10.1 regression: a guest allocated on
// BOTH the source ("miles", = status.GPU.NodeName) and the target ("boba", a
// held reservation) — the reserve-before-stop double-hold — must have BOTH
// freed when the guest is deleted. Releasing only status.GPU.NodeName would
// strand the target reservation forever (AllocatedTo a now-deleted guest).
func TestDeallocateGPUs_FreesAllNodes(t *testing.T) {
	source := nodeAllocatedTo("miles", "0000:01:00.0", "default/g")
	target := nodeAllocatedTo("boba", "0000:ff:00.0", "default/g")
	guest := wgpu1Guest("miles") // status.GPU.NodeName = miles (source)
	r := newReconciler(source, target, guest)

	if err := r.deallocateGPUs(context.Background(), guest); err != nil {
		t.Fatalf("deallocateGPUs: %v", err)
	}
	for _, name := range []string{"miles", "boba"} {
		n := getNode(t, r.Client, name)
		if got := n.Status.GPUs[0].AllocatedTo; got != "" {
			t.Errorf("node %q GPU still AllocatedTo %q after dealloc; reservation/allocation leaked", name, got)
		}
		if n.Status.FreeGPUs != 1 {
			t.Errorf("node %q FreeGPUs=%d, want 1 after dealloc", name, n.Status.FreeGPUs)
		}
	}
}
