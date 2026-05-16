package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestObservedTransferDuration_RoundTrip verifies the new Phase 3b
// status field can be set, marshaled to JSON, and unmarshaled back
// without loss. Companion to W27b's pause-window stamping plumbing
// in stopandcopy_live; the controller dual-writes both
// ObservedTransferDuration (canonical) and ObservedPauseWindow
// (deprecated alias) from a single source value.
func TestObservedTransferDuration_RoundTrip(t *testing.T) {
	dur := metav1.Duration{Duration: 38200 * time.Millisecond}
	status := SwiftMigrationStatus{
		ObservedTransferDuration: &dur,
		ObservedPauseWindow:      &dur,
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
	if got.ObservedPauseWindow == nil {
		t.Fatalf("ObservedPauseWindow (deprecated alias) nil after round-trip")
	}
	if got.ObservedPauseWindow.Duration != dur.Duration {
		t.Errorf("ObservedPauseWindow = %v, want %v",
			got.ObservedPauseWindow.Duration, dur.Duration)
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
