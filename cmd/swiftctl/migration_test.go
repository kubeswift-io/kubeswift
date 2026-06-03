package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

func TestParsePreferredMode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    migrationv1alpha1.SwiftMigrationMode
		wantErr bool
	}{
		{"empty defaults to auto", "", migrationv1alpha1.SwiftMigrationModeAuto, false},
		{"auto", "auto", migrationv1alpha1.SwiftMigrationModeAuto, false},
		{"live", "live", migrationv1alpha1.SwiftMigrationModeLive, false},
		{"offline", "offline", migrationv1alpha1.SwiftMigrationModeOffline, false},
		{"invalid value", "fast", "", true},
		{"case-sensitive: Live is invalid", "Live", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePreferredMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePreferredMode(%q) = %q, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePreferredMode(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsePreferredMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func dur(s string) *metav1.Duration {
	d, _ := time.ParseDuration(s)
	return &metav1.Duration{Duration: d}
}

// TestRenderMigrationDescribe_CompletedLive: a completed live migration
// shows both Downtime and Transfer, plus the semantic gloss distinguishing
// them.
func TestRenderMigrationDescribe_CompletedLive(t *testing.T) {
	m := &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec: migrationv1alpha1.SwiftMigrationSpec{
			GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "g"},
			Target:   migrationv1alpha1.SwiftMigrationTarget{NodeName: "boba"},
			Mode:     migrationv1alpha1.SwiftMigrationModeLive,
		},
		Status: migrationv1alpha1.SwiftMigrationStatus{
			Phase:                    migrationv1alpha1.SwiftMigrationPhaseCompleted,
			Mode:                     migrationv1alpha1.SwiftMigrationModeLive,
			ObservedDowntime:         dur("2.8s"),
			ObservedTransferDuration: dur("38.2s"),
		},
	}
	var buf bytes.Buffer
	renderMigrationDescribe(&buf, m, nil)
	out := buf.String()
	for _, want := range []string{"Downtime:", "Transfer:", "Downtime is the operator-visible", "vm.send-migration RPC"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

// TestRenderMigrationDescribe_CompletedOffline: offline leaves
// ObservedTransferDuration nil, so neither the Transfer line nor the
// live-only gloss appear (would confuse operators — offline has no send RPC).
func TestRenderMigrationDescribe_CompletedOffline(t *testing.T) {
	m := &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "g"}},
		Status: migrationv1alpha1.SwiftMigrationStatus{
			Phase:            migrationv1alpha1.SwiftMigrationPhaseCompleted,
			Mode:             migrationv1alpha1.SwiftMigrationModeOffline,
			ObservedDowntime: dur("40s"),
		},
	}
	var buf bytes.Buffer
	renderMigrationDescribe(&buf, m, nil)
	out := buf.String()
	if !strings.Contains(out, "Downtime:") {
		t.Errorf("offline output should show Downtime\n---\n%s", out)
	}
	if strings.Contains(out, "Transfer:") || strings.Contains(out, "vm.send-migration RPC") {
		t.Errorf("offline output must not show Transfer line or live gloss\n---\n%s", out)
	}
}

// TestRenderMigrationDescribe_LiveProgress: an in-flight live transfer
// surfaces the source pod's progress-estimate annotation with the
// heuristic note.
func TestRenderMigrationDescribe_LiveProgress(t *testing.T) {
	m := &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "g"}},
		Status: migrationv1alpha1.SwiftMigrationStatus{
			Phase: migrationv1alpha1.SwiftMigrationPhaseStopAndCopy,
			Mode:  migrationv1alpha1.SwiftMigrationModeLive,
		},
	}
	srcPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "g-mig-abc123",
			Namespace:   "default",
			Annotations: map[string]string{migrationProgressEstimateAnnotation: "47"},
		},
	}
	var buf bytes.Buffer
	renderMigrationDescribe(&buf, m, srcPod)
	out := buf.String()
	if !strings.Contains(out, "Progress (estimate): 47%") {
		t.Errorf("output missing progress estimate\n---\n%s", out)
	}
	if !strings.Contains(out, "heuristic") {
		t.Errorf("output missing heuristic note\n---\n%s", out)
	}
}

func TestMigrationTimeoutPtr(t *testing.T) {
	if got := migrationTimeoutPtr(0); got != nil {
		t.Errorf("0 (flag default) must return nil so the CRD default applies; got %v", got)
	}
	if got := migrationTimeoutPtr(-5 * time.Second); got != nil {
		t.Errorf("negative must return nil; got %v", got)
	}
	got := migrationTimeoutPtr(10 * time.Minute)
	if got == nil || got.Duration != 10*time.Minute {
		t.Errorf("positive must return an explicit override; got %v", got)
	}
}
