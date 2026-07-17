package swiftgpu

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// sharedProfileWithFMVersion is a hgx-shared profile pinning a required Fabric
// Manager (== guest driver) version.
func sharedProfileWithFMVersion(name, ns string, count int, requiredVersion string) *gpuv1alpha1.SwiftGPUProfile {
	p := testGPUProfile(name, ns, "hgx-shared", "", count, "shared")
	p.Spec.FabricManager = &gpuv1alpha1.FabricManagerSpec{RequiredVersion: requiredVersion}
	return p
}

// The load-bearing NVIDIA WP-12736-002 rule: in shared NVSwitch mode the host
// Fabric Manager version MUST exactly match the guest driver version. Any
// mismatch is a broken fabric, not a boot failure, so it must be rejected
// before allocation — never silently allocated (Design Principle #6).
func TestFMVersionCompatible(t *testing.T) {
	node := func(v string) *gpuv1alpha1.SwiftGPUNode {
		return &gpuv1alpha1.SwiftGPUNode{Status: gpuv1alpha1.SwiftGPUNodeStatus{
			FabricManager: &gpuv1alpha1.FabricManagerStatus{Version: v, Running: true},
		}}
	}
	noFMNode := &gpuv1alpha1.SwiftGPUNode{}
	shared := func(req string) *gpuv1alpha1.SwiftGPUProfile {
		return sharedProfileWithFMVersion("p", "default", 4, req)
	}
	full := func(req string) *gpuv1alpha1.SwiftGPUProfile {
		p := testGPUProfile("p", "default", "hgx-full", "", 8, "full")
		p.Spec.FabricManager = &gpuv1alpha1.FabricManagerSpec{RequiredVersion: req}
		return p
	}
	pcie := testGPUProfile("p", "default", "pcie", "", 1, "isolated")

	cases := []struct {
		name    string
		node    *gpuv1alpha1.SwiftGPUNode
		profile *gpuv1alpha1.SwiftGPUProfile
		want    bool
	}{
		{"shared match", node("550.163.01"), shared("550.163.01"), true},
		{"shared mismatch", node("550.163.01"), shared("560.35.03"), false},
		{"shared no required version (no check)", node("550.163.01"), shared(""), true},
		{"shared but node has no FM", noFMNode, shared("550.163.01"), false},
		{"full mode ignores host FM version", node("550.163.01"), full("560.35.03"), true},
		{"non-shared (pcie) ignored", node("anything"), pcie, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmVersionCompatible(tc.node, tc.profile); got != tc.want {
				t.Errorf("fmVersionCompatible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFindAndAllocate_HGXShared_FMVersionMismatch_Rejected(t *testing.T) {
	node := hgxH100Node("hgx-0") // FM version 550.163.01
	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	profile := sharedProfileWithFMVersion("hgx4", "default", 4, "560.35.03") // mismatch

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, guest, profile).
		WithStatusSubresource(node, guest).
		Build()
	r := &SwiftGPUReconciler{Client: c, Scheme: scheme.Scheme}

	_, gpus, _, _, err := r.findAndAllocate(context.Background(), guest, profile)
	if err == nil {
		t.Fatalf("expected rejection on FM version mismatch, allocated %d GPU(s)", len(gpus))
	}
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("error should wrap ErrNoCapacity, got %v", err)
	}
	if !strings.Contains(err.Error(), "560.35.03") {
		t.Errorf("error should name the required version 560.35.03, got: %v", err)
	}
	// No GPUs / partition should have been marked allocated on the node.
	var after gpuv1alpha1.SwiftGPUNode
	if err := c.Get(context.Background(), client.ObjectKey{Name: "hgx-0"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.FreeGPUs != 8 {
		t.Errorf("no GPUs should be reserved on a version-mismatched node; FreeGPUs=%d, want 8", after.Status.FreeGPUs)
	}
}

func TestFindAndAllocate_HGXShared_FMVersionMatch_Allocates(t *testing.T) {
	node := hgxH100Node("hgx-0") // FM version 550.163.01
	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	profile := sharedProfileWithFMVersion("hgx4", "default", 4, "550.163.01") // match

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, guest, profile).
		WithStatusSubresource(node, guest).
		Build()
	r := &SwiftGPUReconciler{Client: c, Scheme: scheme.Scheme}

	_, gpus, _, partID, err := r.findAndAllocate(context.Background(), guest, profile)
	if err != nil {
		t.Fatalf("expected allocation on FM version match, got %v", err)
	}
	if partID < 0 || len(gpus) != 4 {
		t.Fatalf("expected a 4-GPU partition, got partID=%d gpus=%d", partID, len(gpus))
	}
}

func TestGPUNodeHasCapacity_HGXShared_FMVersionMismatch(t *testing.T) {
	node := hgxH100Node("hgx-0")
	profile := sharedProfileWithFMVersion("hgx4", "default", 4, "560.35.03")

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node).WithStatusSubresource(node).Build()

	err := GPUNodeHasCapacity(context.Background(), c, "hgx-0", profile)
	if err == nil {
		t.Fatal("expected a capacity rejection on FM version mismatch")
	}
	if !strings.Contains(err.Error(), "Fabric Manager version") || !strings.Contains(err.Error(), "560.35.03") {
		t.Errorf("error should name the FM version mismatch, got: %v", err)
	}
}

func TestReserveOnNode_HGXShared_FMVersionMismatch(t *testing.T) {
	node := hgxH100Node("hgx-0")
	guest := testSwiftGuest("g1", "default", &corev1.LocalObjectReference{Name: "hgx4"})
	profile := sharedProfileWithFMVersion("hgx4", "default", 4, "560.35.03")

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node, guest, profile).
		WithStatusSubresource(node, guest).Build()

	_, _, _, err := ReserveOnNode(context.Background(), c, guest, profile, "hgx-0")
	if err == nil {
		t.Fatal("expected reserve to fail on FM version mismatch")
	}
	if !strings.Contains(err.Error(), "560.35.03") {
		t.Errorf("error should name the required version, got: %v", err)
	}
}
