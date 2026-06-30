package swiftsnapshotschedule

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// readyChild builds a Ready snapshot owned (by label) by "nightly", captured at
// the given time.
func readyChild(name string, capturedAt time.Time) *snapshotv1alpha1.SwiftSnapshot {
	ct := metav1.NewTime(capturedAt)
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns",
			Labels:            map[string]string{snapshotv1alpha1.ScheduleLabel: "nightly"},
			CreationTimestamp: ct,
		},
		Spec:   snapshotv1alpha1.SwiftSnapshotSpec{GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"}},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{Phase: snapshotv1alpha1.SwiftSnapshotPhaseReady, CapturedAt: &ct},
	}
}

func keepLastSched(n int32) *snapshotv1alpha1.SwiftSnapshotSchedule {
	return schedule(func(s *snapshotv1alpha1.SwiftSnapshotSchedule) {
		s.Spec.Retention = &snapshotv1alpha1.SnapshotScheduleRetention{KeepLast: &n}
	})
}

func snapNames(t *testing.T, c client.Client) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, s := range listSnaps(t, c) {
		if s.DeletionTimestamp == nil {
			out[s.Name] = true
		}
	}
	return out
}

func TestPruneKeepN_DeletesOldestBeyondBudget(t *testing.T) {
	s := keepLastSched(2)
	c1 := readyChild("s1", baseTime.Add(-3*time.Minute))
	c2 := readyChild("s2", baseTime.Add(-2*time.Minute))
	c3 := readyChild("s3", baseTime.Add(-1*time.Minute))
	c4 := readyChild("s4", baseTime) // newest
	r, c := newSched(t, baseTime, s, c1, c2, c3, c4)

	if err := r.pruneKeepN(context.Background(), s, []snapshotv1alpha1.SwiftSnapshot{*c1, *c2, *c3, *c4}); err != nil {
		t.Fatal(err)
	}
	got := snapNames(t, c)
	// Keep the 2 newest (s3, s4); prune s1, s2.
	if !got["s3"] || !got["s4"] {
		t.Errorf("newest 2 must survive; got %v", got)
	}
	if got["s1"] || got["s2"] {
		t.Errorf("oldest 2 must be pruned; got %v", got)
	}
}

func TestPruneKeepN_UnderBudget_NoOp(t *testing.T) {
	s := keepLastSched(5)
	c1 := readyChild("s1", baseTime.Add(-time.Minute))
	c2 := readyChild("s2", baseTime)
	r, c := newSched(t, baseTime, s, c1, c2)
	if err := r.pruneKeepN(context.Background(), s, []snapshotv1alpha1.SwiftSnapshot{*c1, *c2}); err != nil {
		t.Fatal(err)
	}
	if n := len(snapNames(t, c)); n != 2 {
		t.Errorf("under budget must prune nothing; got %d", n)
	}
}

func TestPruneKeepN_NoRetention_NoOp(t *testing.T) {
	s := schedule(nil) // no spec.retention
	c1 := readyChild("s1", baseTime.Add(-time.Minute))
	c2 := readyChild("s2", baseTime)
	r, c := newSched(t, baseTime, s, c1, c2)
	if err := r.pruneKeepN(context.Background(), s, []snapshotv1alpha1.SwiftSnapshot{*c1, *c2}); err != nil {
		t.Fatal(err)
	}
	if n := len(snapNames(t, c)); n != 2 {
		t.Errorf("no retention must prune nothing; got %d", n)
	}
}

func TestPruneKeepN_NonReadyNotCounted(t *testing.T) {
	// keepLast=1; two Ready + one Capturing. Only the 2 Ready count; prune the
	// older Ready, keep the newer Ready and leave the Capturing one alone.
	s := keepLastSched(1)
	rOld := readyChild("ready-old", baseTime.Add(-2*time.Minute))
	rNew := readyChild("ready-new", baseTime)
	capturing := readyChild("capturing", baseTime.Add(-time.Minute))
	capturing.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	r, c := newSched(t, baseTime, s, rOld, rNew, capturing)
	if err := r.pruneKeepN(context.Background(), s, []snapshotv1alpha1.SwiftSnapshot{*rOld, *rNew, *capturing}); err != nil {
		t.Fatal(err)
	}
	got := snapNames(t, c)
	if got["ready-old"] {
		t.Error("older Ready snapshot should be pruned")
	}
	if !got["ready-new"] || !got["capturing"] {
		t.Errorf("newest Ready + the in-flight capture must survive; got %v", got)
	}
}

func TestPruneKeepN_SkipsReferenced(t *testing.T) {
	// keepLast=1; the oldest (prune candidate) is referenced by a clone guest →
	// skipped (not deleted); the next-oldest unreferenced one is still pruned.
	s := keepLastSched(1)
	cOld := readyChild("ref-old", baseTime.Add(-2*time.Minute)) // referenced, beyond budget
	cMid := readyChild("plain-mid", baseTime.Add(-time.Minute)) // unreferenced, beyond budget
	cNew := readyChild("keep-new", baseTime)                    // newest, kept
	cloneGuest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "clone-x", Namespace: "ns"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "ref-old"}},
		},
	}
	r, c := newSched(t, baseTime, s, cOld, cMid, cNew, cloneGuest)
	if err := r.pruneKeepN(context.Background(), s, []snapshotv1alpha1.SwiftSnapshot{*cOld, *cMid, *cNew}); err != nil {
		t.Fatal(err)
	}
	got := snapNames(t, c)
	if !got["ref-old"] {
		t.Error("a referenced snapshot must be skipped by keep-N, not deleted")
	}
	if got["plain-mid"] {
		t.Error("an unreferenced over-budget snapshot must still be pruned")
	}
	if !got["keep-new"] {
		t.Error("the newest snapshot must be kept")
	}
}
