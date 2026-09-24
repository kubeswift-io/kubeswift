package swiftgpu

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// oneFreeGPUNode is a Ready, VFIO-ready SwiftGPUNode with one free GPU.
func oneFreeGPUNode(name string) *gpuv1alpha1.SwiftGPUNode {
	return testGPUNode(name, []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, false, ""),
	}, nil)
}

// allocateOneUnder runs a one-GPU allocation over objs and returns the chosen node
// ("" when none was usable).
func allocateOneUnder(t *testing.T, nc NodeConstraint, objs ...client.Object) (string, error) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(&gpuv1alpha1.SwiftGPUNode{}).
		Build()
	profile := testGPUProfile("p", "default", "pcie", "", 1, "")
	node, _, _, _, err := FindAndAllocateFor(context.Background(), c, "default/g1", "", profile, nc)
	if node == nil {
		return "", err
	}
	return node.Name, err
}

// Each node the launcher could not run on is passed over for the one it can:
// the allocator used to take the first SwiftGPUNode with free GPUs.
func TestFindAndAllocateFor_SkipsNodesTheLauncherCannotUse(t *testing.T) {
	cordoned := kubeNode("a-cordoned")
	cordoned.Spec.Unschedulable = true
	notVfio := oneFreeGPUNode("b-novfio")
	notVfio.Status.VfioReady = false
	erroring := oneFreeGPUNode("c-error")
	erroring.Status.Phase = "Error"

	got, err := allocateOneUnder(t, NodeConstraint{},
		oneFreeGPUNode("a-cordoned"), cordoned,
		notVfio, kubeNode("b-novfio"),
		erroring, kubeNode("c-error"),
		oneFreeGPUNode("d-nonode"), // SwiftGPUNode left behind by a removed Node
		oneFreeGPUNode("e-good"), kubeNode("e-good"),
	)
	if err != nil || got != "e-good" {
		t.Fatalf("allocated on %q (err %v), want e-good", got, err)
	}
}

func TestFindAndAllocateFor_NoUsableNodeIsNoCapacity(t *testing.T) {
	cordoned := kubeNode("a")
	cordoned.Spec.Unschedulable = true
	got, err := allocateOneUnder(t, NodeConstraint{}, oneFreeGPUNode("a"), cordoned)
	if !errors.Is(err, ErrNoCapacity) || got != "" {
		t.Fatalf("got node %q err %v, want ErrNoCapacity", got, err)
	}
}

func TestFindAndAllocateFor_HonorsRequiredNode(t *testing.T) {
	got, err := allocateOneUnder(t, NodeConstraint{RequiredNode: "b"},
		oneFreeGPUNode("a"), kubeNode("a"),
		oneFreeGPUNode("b"), kubeNode("b"),
	)
	if err != nil || got != "b" {
		t.Fatalf("allocated on %q (err %v), want b", got, err)
	}
}

func TestFindAndAllocateFor_HonorsNodeSelector(t *testing.T) {
	labelled := kubeNode("b")
	labelled.Labels = map[string]string{"pool": "gpu"}
	got, err := allocateOneUnder(t, NodeConstraint{NodeSelector: map[string]string{"pool": "gpu"}},
		oneFreeGPUNode("a"), kubeNode("a"),
		oneFreeGPUNode("b"), labelled,
	)
	if err != nil || got != "b" {
		t.Fatalf("allocated on %q (err %v), want b", got, err)
	}
}

// An allocation the workload already holds is returned as-is even if its node
// has since been cordoned: the constraint gates NEW placements only, and
// re-placing a running workload's GPUs is the drain/migration path's job.
func TestFindAndAllocateFor_ExistingAllocationSurvivesCordon(t *testing.T) {
	held := testGPUNode("a", []gpuv1alpha1.GPUDevice{
		makeGPU(0, "0000:17:00.0", "NVIDIA H200 SXM", 0, true, "default/g1"),
	}, nil)
	cordoned := kubeNode("a")
	cordoned.Spec.Unschedulable = true
	got, err := allocateOneUnder(t, NodeConstraint{}, held, cordoned)
	if err != nil || got != "a" {
		t.Fatalf("got %q (err %v), want the existing allocation on a", got, err)
	}
}

// A SwiftGuest's spec.nodeName is the allocation's required node.
func TestFindAndAllocate_GuestNodeNameIsRequired(t *testing.T) {
	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "p"})
	guest.Spec.NodeName = "b"
	profile := testGPUProfile("p", "default", "pcie", "", 1, "")
	r := newReconciler(guest, profile, oneFreeGPUNode("a"), oneFreeGPUNode("b"))

	node, _, _, _, err := r.findAndAllocate(context.Background(), guest, profile)
	if err != nil || node == nil || node.Name != "b" {
		t.Fatalf("allocated on %v (err %v), want b", node, err)
	}
}
