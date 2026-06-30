package swiftgpu

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// TestReleaseFromNode_CountsOnlyChangedReleases guards the O3 release
// counter: a release that frees devices increments gpu_releases_total once;
// the idempotent no-op re-release must not count (deallocateGPUs calls
// ReleaseFromNode for EVERY SwiftGPUNode on every finalizer reconcile).
func TestReleaseFromNode_CountsOnlyChangedReleases(t *testing.T) {
	g := gpuGuest("metrics-g")
	c := newClient(gpuNodeWithDevices("boba", true, dev(0, "0000:01:00.0", 0)), g)

	if _, _, _, err := ReserveOnNode(context.Background(), c, g, profile(1, "", "isolated"), "boba"); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	before := testutil.ToFloat64(metrics.GPUReleasesTotal)
	if err := ReleaseFromNode(context.Background(), c, g, "boba"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := testutil.ToFloat64(metrics.GPUReleasesTotal); got != before+1 {
		t.Errorf("gpu_releases_total = %v, want %v after a freeing release", got, before+1)
	}

	// Idempotent re-release: nothing left to free, counter must not move.
	if err := ReleaseFromNode(context.Background(), c, g, "boba"); err != nil {
		t.Fatalf("no-op release: %v", err)
	}
	if got := testutil.ToFloat64(metrics.GPUReleasesTotal); got != before+1 {
		t.Errorf("no-op release must not count; gpu_releases_total = %v, want %v", got, before+1)
	}
}

// TestHasGPUAllocatedReason covers the transition gate that keeps the
// no_capacity counter from re-counting on every 30s retry tick.
func TestHasGPUAllocatedReason(t *testing.T) {
	g := gpuGuest("gate-g")
	if hasGPUAllocatedReason(g, "NoCapacity") {
		t.Error("no condition at all must not match")
	}
	g.Status.Conditions = []metav1.Condition{{
		Type: swiftv1alpha1.ConditionGPUAllocated, Status: metav1.ConditionFalse, Reason: "NoCapacity",
	}}
	if !hasGPUAllocatedReason(g, "NoCapacity") {
		t.Error("existing NoCapacity reason must match (gate suppresses re-count)")
	}
	if hasGPUAllocatedReason(g, "Allocated") {
		t.Error("different reason must not match")
	}
}
