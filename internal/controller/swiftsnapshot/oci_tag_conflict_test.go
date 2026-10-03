package swiftsnapshot

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

var tagTestT0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// taggedSnap is an oci snapshot pushing to zot.svc:5000/vm-snapshots:nightly,
// created at t0+offset.
func taggedSnap(name string, offset time.Duration, phase snapshotv1alpha1.SwiftSnapshotPhase) *snapshotv1alpha1.SwiftSnapshot {
	s := ociSnap(func(o *snapshotv1alpha1.OCIBackend) { o.Tag = "nightly" })
	s.Name = name
	s.CreationTimestamp = metav1.NewTime(tagTestT0.Add(offset))
	s.Status.Phase = phase
	return s
}

func pendingResult(t *testing.T, snap *snapshotv1alpha1.SwiftSnapshot, others ...*snapshotv1alpha1.SwiftSnapshot) snapshotv1alpha1.SwiftSnapshotStatus {
	t.Helper()
	objs := []client.Object{snap}
	for _, o := range others {
		objs = append(objs, o)
	}
	r, _ := newReconciler(t, objs...)
	var status snapshotv1alpha1.SwiftSnapshotStatus
	if _, _, err := r.handlePending(context.Background(), snap, &status); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	return status
}

func refusedTagInUse(status snapshotv1alpha1.SwiftSnapshotStatus) (bool, string) {
	for _, c := range status.Conditions {
		if c.Type == "Ready" && c.Reason == ReasonTagInUse {
			return status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseFailed, c.Message
		}
	}
	return false, ""
}

// Two snapshots on one repository:tag: some registries drop the first one's
// manifest as soon as the second pushes (#705). Exactly one may proceed, and
// it is decided before the guest is touched.
func TestHandlePending_OCITagInUse(t *testing.T) {
	cases := []struct {
		name    string
		self    *snapshotv1alpha1.SwiftSnapshot
		other   *snapshotv1alpha1.SwiftSnapshot
		refused bool
	}{
		{"an older snapshot still pending wins", taggedSnap("b", time.Minute, ""), taggedSnap("a", 0, snapshotv1alpha1.SwiftSnapshotPhasePending), true},
		{"a newer snapshot that has started wins", taggedSnap("b", 0, ""), taggedSnap("a", time.Minute, snapshotv1alpha1.SwiftSnapshotPhaseCapturing), true},
		{"a Ready snapshot wins", taggedSnap("b", time.Minute, ""), taggedSnap("a", 0, snapshotv1alpha1.SwiftSnapshotPhaseReady), true},
		{"a newer snapshot still pending loses", taggedSnap("b", 0, ""), taggedSnap("a", time.Minute, ""), false},
		{"same second: the smaller name wins", taggedSnap("b", 0, ""), taggedSnap("a", 0, ""), true},
		{"same second: the larger name loses", taggedSnap("a", 0, ""), taggedSnap("b", 0, ""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status := pendingResult(t, c.self, c.other)
			refused, msg := refusedTagInUse(status)
			if refused != c.refused {
				t.Fatalf("refused=%v (phase %q), want %v", refused, status.Phase, c.refused)
			}
			if refused && (!strings.Contains(msg, "zot.svc:5000/vm-snapshots:nightly") || !strings.Contains(msg, "SwiftSnapshot "+c.other.Name)) {
				t.Errorf("message should name the reference and the other snapshot: %q", msg)
			}
		})
	}
}

// Default tags are per snapshot, so they never conflict.
func TestHandlePending_DefaultTagsDoNotConflict(t *testing.T) {
	self := ociSnap(nil)
	self.Name = "b"
	other := ociSnap(nil)
	other.Name = "a"
	other.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseReady
	if refused, _ := refusedTagInUse(pendingResult(t, self, other)); refused {
		t.Error("default tags <namespace>-<name> must not conflict")
	}
}
