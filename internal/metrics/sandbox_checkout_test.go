package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMarkSandboxCheckoutObserved_DedupesPerUID(t *testing.T) {
	if !MarkSandboxCheckoutObserved("uid-A") {
		t.Fatal("first sighting of uid-A should return true")
	}
	if MarkSandboxCheckoutObserved("uid-A") {
		t.Error("second sighting of uid-A should return false (deduped)")
	}
	if !MarkSandboxCheckoutObserved("uid-B") {
		t.Error("first sighting of a different uid should return true")
	}
}

// The cold-fallback path can re-enter across reconciles until status.podRef
// persists (the cold createLaunch may requeue on image resolve before the
// status write lands). Gating SandboxCheckoutsTotal on the UID dedupe makes the
// counter fire once per sandbox regardless of how many times the decision runs.
func TestSandboxCheckout_CountsOncePerSandbox(t *testing.T) {
	before := testutil.ToFloat64(SandboxCheckoutsTotal.WithLabelValues("cold"))
	const uid = "uid-cold-dedupe"
	for i := 0; i < 3; i++ { // 3 cold-fallback reconciles of the SAME sandbox
		if MarkSandboxCheckoutObserved(uid) {
			SandboxCheckoutsTotal.WithLabelValues("cold").Inc()
		}
	}
	if delta := testutil.ToFloat64(SandboxCheckoutsTotal.WithLabelValues("cold")) - before; delta != 1 {
		t.Errorf("cold counter delta = %v, want 1 (deduped across 3 reconciles)", delta)
	}
}
