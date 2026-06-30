package swiftgpu

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func dev(idx int, pci string, numa int) gpuv1alpha1.GPUDevice {
	return gpuv1alpha1.GPUDevice{Index: idx, PCIAddress: pci, NUMANode: numa, Model: "GeForce GTX 1080"}
}

func gpuNodeWithDevices(name string, vfioReady bool, devs ...gpuv1alpha1.GPUDevice) *gpuv1alpha1.SwiftGPUNode {
	n := &gpuv1alpha1.SwiftGPUNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: gpuv1alpha1.SwiftGPUNodeStatus{
			VfioReady: vfioReady,
			GPUModel:  "NVIDIA Corporation GP104 [GeForce GTX 1080]",
			GPUs:      devs,
		},
	}
	n.Status.FreeGPUs = countFreeGPUs(devs)
	return n
}

func gpuGuest(name string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")}}
}

func newClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).
		WithObjects(objs...).Build()
}

func getNode(t *testing.T, c client.Client, name string) *gpuv1alpha1.SwiftGPUNode {
	t.Helper()
	var n gpuv1alpha1.SwiftGPUNode
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatalf("get node %q: %v", name, err)
	}
	return &n
}

func allocatedTo(n *gpuv1alpha1.SwiftGPUNode, pci string) string {
	for _, g := range n.Status.GPUs {
		if g.PCIAddress == pci {
			return g.AllocatedTo
		}
	}
	return "<no-such-gpu>"
}

func TestReserveOnNode_MarksWithoutTouchingStatus(t *testing.T) {
	g := gpuGuest("g")
	n := gpuNodeWithDevices("boba", true, dev(0, "0000:01:00.0", 0))
	c := newClient(n, g)

	devs, _, partID, err := ReserveOnNode(context.Background(), c, g, profile(1, "", "isolated"), "boba")
	if err != nil {
		t.Fatalf("ReserveOnNode: %v", err)
	}
	if len(devs) != 1 || devs[0].PCIAddress != "0000:01:00.0" {
		t.Errorf("returned devices = %+v, want the GTX 1080", devs)
	}
	if partID != -1 {
		t.Errorf("partitionID = %d, want -1 (isolated)", partID)
	}

	got := getNode(t, c, "boba")
	if allocatedTo(got, "0000:01:00.0") != "default/g" {
		t.Errorf("GPU must be AllocatedTo default/g; got %q", allocatedTo(got, "0000:01:00.0"))
	}
	if got.Status.FreeGPUs != 0 {
		t.Errorf("FreeGPUs = %d, want 0 after reserve", got.Status.FreeGPUs)
	}
	// status.GPU on the guest must be UNTOUCHED (reserve never writes it).
	var gg swiftv1alpha1.SwiftGuest
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "g"}, &gg)
	if gg.Status.GPU != nil {
		t.Errorf("ReserveOnNode must not write guest.status.GPU; got %+v", gg.Status.GPU)
	}
}

func TestReserveOnNode_Idempotent(t *testing.T) {
	g := gpuGuest("g")
	n := gpuNodeWithDevices("boba", true, dev(0, "0000:01:00.0", 0))
	c := newClient(n, g)

	if _, _, _, err := ReserveOnNode(context.Background(), c, g, profile(1, "", "isolated"), "boba"); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	devs, _, _, err := ReserveOnNode(context.Background(), c, g, profile(1, "", "isolated"), "boba")
	if err != nil {
		t.Fatalf("re-reserve must be idempotent, not error: %v", err)
	}
	if len(devs) != 1 {
		t.Errorf("re-reserve returned %d devices, want the existing 1", len(devs))
	}
	if got := getNode(t, c, "boba"); got.Status.FreeGPUs != 0 {
		t.Errorf("FreeGPUs = %d, want 0 (no double-allocation on re-reserve)", got.Status.FreeGPUs)
	}
}

func TestReserveOnNode_NotVfioReady(t *testing.T) {
	c := newClient(gpuNodeWithDevices("boba", false, dev(0, "0000:01:00.0", 0)), gpuGuest("g"))
	if _, _, _, err := ReserveOnNode(context.Background(), c, gpuGuest("g"), profile(1, "", "isolated"), "boba"); err == nil {
		t.Errorf("expected error reserving on a non-vfio-ready node")
	}
}

func TestReserveOnNode_Insufficient(t *testing.T) {
	c := newClient(gpuNodeWithDevices("boba", true), gpuGuest("g")) // no GPUs
	if _, _, _, err := ReserveOnNode(context.Background(), c, gpuGuest("g"), profile(1, "", "isolated"), "boba"); err == nil {
		t.Errorf("expected error reserving when no GPUs are free")
	}
}

// TestReserveOnNode_HoldsAgainstOtherGuests proves the reservation holds: once
// guest A reserves the only GPU, guest B cannot reserve on the same node.
func TestReserveOnNode_HoldsAgainstOtherGuests(t *testing.T) {
	gA, gB := gpuGuest("a"), gpuGuest("b")
	c := newClient(gpuNodeWithDevices("boba", true, dev(0, "0000:01:00.0", 0)), gA, gB)

	if _, _, _, err := ReserveOnNode(context.Background(), c, gA, profile(1, "", "isolated"), "boba"); err != nil {
		t.Fatalf("guest A reserve: %v", err)
	}
	if _, _, _, err := ReserveOnNode(context.Background(), c, gB, profile(1, "", "isolated"), "boba"); err == nil {
		t.Errorf("guest B must NOT be able to reserve the GPU A holds")
	}
}

func TestReleaseFromNode(t *testing.T) {
	g := gpuGuest("g")
	c := newClient(gpuNodeWithDevices("boba", true, dev(0, "0000:01:00.0", 0)), g)

	if _, _, _, err := ReserveOnNode(context.Background(), c, g, profile(1, "", "isolated"), "boba"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := ReleaseFromNode(context.Background(), c, g, "boba"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := getNode(t, c, "boba")
	if allocatedTo(got, "0000:01:00.0") != "" {
		t.Errorf("GPU must be freed; AllocatedTo = %q", allocatedTo(got, "0000:01:00.0"))
	}
	if got.Status.FreeGPUs != 1 {
		t.Errorf("FreeGPUs = %d, want 1 after release", got.Status.FreeGPUs)
	}
}

func TestReleaseFromNode_NodeGone_NoError(t *testing.T) {
	c := newClient(gpuGuest("g")) // no node
	if err := ReleaseFromNode(context.Background(), c, gpuGuest("g"), "ghost"); err != nil {
		t.Errorf("release on a missing node must be a no-op, not an error: %v", err)
	}
}
