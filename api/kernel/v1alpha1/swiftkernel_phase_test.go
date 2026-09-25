package v1alpha1

import "testing"

// A launcher pinned to a node needs the kernel there only; any other launcher
// can land on any kernel node, so it needs the kernel everywhere.
func TestPhaseOn(t *testing.T) {
	sk := &SwiftKernel{Status: SwiftKernelStatus{
		Phase: SwiftKernelPhasePulling,
		NodeStatuses: []NodeKernelStatus{
			{NodeName: "worker-1", Phase: SwiftKernelPhaseReady},
			{NodeName: "worker-2", Phase: SwiftKernelPhasePulling},
		},
	}}
	cases := []struct {
		node string
		want SwiftKernelPhase
	}{
		{"", SwiftKernelPhasePulling},         // not pinned: overall
		{"worker-1", SwiftKernelPhaseReady},   // pinned, reported Ready there
		{"worker-2", SwiftKernelPhasePulling}, // pinned, still pulling there
		{"worker-9", SwiftKernelPhasePulling}, // pinned, not reported: overall
	}
	for _, c := range cases {
		if got := sk.PhaseOn(c.node); got != c.want {
			t.Errorf("PhaseOn(%q) = %q, want %q", c.node, got, c.want)
		}
	}

	// A kernel that has reported nothing yet is not Ready anywhere.
	if got := (&SwiftKernel{}).PhaseOn("worker-1"); got == SwiftKernelPhaseReady {
		t.Errorf("a kernel with no status reads as Ready on a node")
	}
}
