package swiftrestore

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// fullStateSnap is a full-state (includeDisk) oci capture of g1, taken at
// capturedAt, with resumeAfterSnapshot left at its default true: a full-state
// capture ignores it.
func fullStateSnap(capturedAt time.Time) *snapshotv1alpha1.SwiftSnapshot {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default_snap1", "v51.1")
	snap.Spec.Backend.Type = snapshotv1alpha1.SnapshotBackendOCI
	snap.Spec.IncludeDisk = true
	snap.Spec.ResumeAfterSnapshot = true
	snap.Status.CapturedAt = &metav1.Time{Time: capturedAt}
	return snap
}

func launcherCreatedAt(at time.Time) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default", OwnerReferences: guestControllerRef("g1"), CreationTimestamp: metav1.NewTime(at)}}
}

func inPlaceRestore() *snapshotv1alpha1.SwiftRestore {
	restore := makeRestore("r1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	return restore
}

// An in-place restore reopens the guest's live disk, not the one a full-state
// capture carries. A guest started again after the export has a disk that
// moved on, so the restore is refused like a memory-only one (#710).
func TestDiskDivergence_FullStateGuestStartedAgainIsRefused(t *testing.T) {
	captured := time.Now().Add(-time.Hour)
	snap := fullStateSnap(captured)
	r, _ := newReconciler(t, snap, launcherCreatedAt(captured.Add(10*time.Minute)))
	msg, err := r.diskDivergence(context.Background(), snap, inPlaceRestore(), makeSourceGuest("default", "g1", "ubuntu-noble"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"relaunched from its disk since the capture", "carries the disk it was captured with", "spec.cloneFromSnapshot"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "resumeAfterSnapshot") {
		t.Errorf("a full-state capture never resumes the guest; the message must not say it did:\n%s", msg)
	}
}

// A full-state export stops the guest. Left stopped, its disk is the one the
// capture froze, and the restore may go ahead, whatever resumeAfterSnapshot says.
func TestDiskDivergence_FullStateGuestLeftStoppedProceeds(t *testing.T) {
	snap := fullStateSnap(time.Now().Add(-time.Hour))
	r, _ := newReconciler(t, snap)
	msg, err := r.diskDivergence(context.Background(), snap, inPlaceRestore(), makeSourceGuest("default", "g1", "ubuntu-noble"))
	if err != nil || msg != "" {
		t.Errorf("msg=%q err=%v, want no refusal", msg, err)
	}
}

// The memory-only refusal used to send the operator to a csi-volume-snapshot
// "for a disk-consistent restore"; a csi-volume-snapshot now restores in place
// too (#711), and the advice says how.
func TestDiskDivergence_MemoryOnlyAdviceIsWhatCanBeDone(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default_snap1", "v51.1")
	snap.Spec.ResumeAfterSnapshot = true
	r, _ := newReconciler(t, snap)
	msg, err := r.diskDivergence(context.Background(), snap, inPlaceRestore(), makeSourceGuest("default", "g1", "ubuntu-noble"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"holds memory only", "restore a csi-volume-snapshot of g1", "in place with targetGuest.overwriteExisting=true", snapshotv1alpha1.AnnotationAcceptDiskDivergence} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Use a csi-volume-snapshot for a disk-consistent restore") {
		t.Errorf("the old advice is back:\n%s", msg)
	}
}
