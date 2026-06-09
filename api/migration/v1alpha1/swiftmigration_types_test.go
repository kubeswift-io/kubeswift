package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestObservedTransferDuration_RoundTrip verifies the status field can
// be set, marshaled to JSON, and unmarshaled back without loss. (The
// deprecated ObservedPauseWindow alias was removed in the CH-v52
// observability work; ObservedTransferDuration is the canonical field.)
func TestObservedTransferDuration_RoundTrip(t *testing.T) {
	dur := metav1.Duration{Duration: 38200 * time.Millisecond}
	status := SwiftMigrationStatus{
		ObservedTransferDuration: &dur,
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}

	var got SwiftMigrationStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}

	if got.ObservedTransferDuration == nil {
		t.Fatalf("ObservedTransferDuration nil after round-trip")
	}
	if got.ObservedTransferDuration.Duration != dur.Duration {
		t.Errorf("ObservedTransferDuration = %v, want %v",
			got.ObservedTransferDuration.Duration, dur.Duration)
	}
}

// TestAppliedDowntimeMs_RoundTrip verifies the downtime_ms bound echo
// survives a JSON round-trip under its canonical key.
func TestAppliedDowntimeMs_RoundTrip(t *testing.T) {
	ms := int64(300)
	data, err := json.Marshal(SwiftMigrationStatus{AppliedDowntimeMs: &ms})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"appliedDowntimeMs":300`) {
		t.Errorf("JSON %s missing appliedDowntimeMs:300", data)
	}
	var got SwiftMigrationStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AppliedDowntimeMs == nil || *got.AppliedDowntimeMs != ms {
		t.Errorf("AppliedDowntimeMs = %v, want %d", got.AppliedDowntimeMs, ms)
	}
}

// TestObservedTransferDuration_JSONKey verifies the JSON field name
// matches the design doc Section 3.5 canonical name. A regression
// on this key would break CRD compatibility (operators reading
// .status.observedTransferDuration via kubectl/jsonpath).
func TestObservedTransferDuration_JSONKey(t *testing.T) {
	dur := metav1.Duration{Duration: time.Second}
	status := SwiftMigrationStatus{ObservedTransferDuration: &dur}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(data)
	if !contains(js, `"observedTransferDuration":`) {
		t.Errorf("JSON key 'observedTransferDuration' not in output: %s", js)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
