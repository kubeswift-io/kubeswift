package swiftgpu

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gpuv1alpha1 "github.com/kubeswift-io/kubeswift/api/gpu/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// gpuAllocatedReason returns the reason on the GPUAllocated condition (or "").
func gpuAllocatedReason(guest *swiftv1alpha1.SwiftGuest) string {
	for _, c := range guest.Status.Conditions {
		if c.Type == swiftv1alpha1.ConditionGPUAllocated {
			return c.Reason
		}
	}
	return ""
}

// A tier: hgx-full (Tier 3) guest must be REJECTED with a GPUAllocated=False /
// UnsupportedTier condition, NOT booted with GPUs and no NVSwitch fabric. The
// guest-side NVSwitch passthrough an in-guest Fabric Manager needs is not wired
// (buildGPUIntent omits GPUIntent.NVSwitches), so allocating for Tier 3 would be a
// silent failure. LOAD-BEARING: if a future change wires NVSwitches and lifts this
// rejection, it must be hardware-validated first.
func TestReconcile_HGXFull_RejectedNotSilentlyBooted(t *testing.T) {
	node := testGPUNode("boba", eightGPUs(), &gpuv1alpha1.FabricManagerStatus{
		Installed: true, Running: true, Version: "550.90.07",
		Partitions: []gpuv1alpha1.FMPartitionStatus{{ID: 0, GPUIndices: []int{0, 1, 2, 3, 4, 5, 6, 7}, Active: true}},
	})
	profile := testGPUProfile("hgxfull", "default", "hgx-full", "", 8, "full")
	guest := testSwiftGuest("t3", "default", &corev1.LocalObjectReference{Name: "hgxfull"})
	r := newReconciler(node, profile, guest)

	// First reconcile adds the finalizer; reconcile until the condition settles.
	for i := 0; i < 3; i++ {
		if _, err := reconcileGuest(r, "t3", "default"); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	g, err := getGuest(r, "t3", "default")
	if err != nil {
		t.Fatal(err)
	}
	if !hasGPUAllocatedCondition(g, metav1.ConditionFalse) {
		t.Fatalf("Tier 3 guest must have GPUAllocated=False, conditions=%+v", g.Status.Conditions)
	}
	if reason := gpuAllocatedReason(g); reason != "UnsupportedTier" {
		t.Errorf("GPUAllocated reason = %q, want UnsupportedTier", reason)
	}
	// No GPUs may have been allocated on the node (the tier check precedes allocation).
	n, _ := getGPUNode(r, "boba")
	if n.Status.FreeGPUs != 8 {
		t.Errorf("Tier 3 rejection must not allocate GPUs: node free = %d, want 8", n.Status.FreeGPUs)
	}
	// And status.gpu must NOT be stamped (no fabric-less boot).
	if g.Status.GPU != nil {
		t.Errorf("Tier 3 guest must not get a GPUStatus, got %+v", g.Status.GPU)
	}
}

// Tier 2 (hgx-shared) is NOT rejected by the tier gate — it uses the host Fabric
// Manager and passes no NVSwitches into the guest, so it stays allocatable.
func TestReconcile_HGXShared_NotRejectedByTierGate(t *testing.T) {
	node := testGPUNode("boba", eightGPUs(), &gpuv1alpha1.FabricManagerStatus{
		Installed: true, Running: true, Version: "550.90.07",
		Partitions: []gpuv1alpha1.FMPartitionStatus{{ID: 0, GPUIndices: []int{0, 1, 2, 3, 4, 5, 6, 7}, Active: true}},
	})
	profile := testGPUProfile("hgx8", "default", "hgx-shared", "", 8, "shared")
	guest := testSwiftGuest("t2", "default", &corev1.LocalObjectReference{Name: "hgx8"})
	r := newReconciler(node, profile, guest)

	for i := 0; i < 3; i++ {
		if _, err := reconcileGuest(r, "t2", "default"); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	g, _ := getGuest(r, "t2", "default")
	if reason := gpuAllocatedReason(g); reason == "UnsupportedTier" {
		t.Errorf("hgx-shared (Tier 2) must NOT be rejected by the Tier 3 gate, reason=%q", reason)
	}
}
