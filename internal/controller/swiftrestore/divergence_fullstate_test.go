package swiftrestore

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default", CreationTimestamp: metav1.NewTime(at)}}
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
// "for a disk-consistent restore", which cannot restore in place either (#711).
func TestDiskDivergence_MemoryOnlyAdviceIsWhatCanBeDone(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default_snap1", "v51.1")
	snap.Spec.ResumeAfterSnapshot = true
	r, _ := newReconciler(t, snap)
	msg, err := r.diskDivergence(context.Background(), snap, inPlaceRestore(), makeSourceGuest("default", "g1", "ubuntu-noble"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"holds memory only", "can only go to a new SwiftGuest", "under a new name, while g1 still exists", snapshotv1alpha1.AnnotationAcceptDiskDivergence} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Use a csi-volume-snapshot for a disk-consistent restore") {
		t.Errorf("the old advice is back:\n%s", msg)
	}
}

// A csi-volume-snapshot restore onto the snapshot's own guest cannot work,
// with or without overwriteExisting, and deleting the guest first does not
// help: the restore copies its spec. Both refusals say so (#711).
func TestCSI_RestoreOntoTheSnapshotsOwnGuestSaysRestoreToANewName(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		snap := makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30)
		restore := makeRestore("r1", "default", "snap1", "g1", true)
		restore.Spec.TargetGuest.OverwriteExisting = overwrite
		r, c := newReconciler(t, restore, snap, runningGuest("g1"))
		reconcile(t, r, "r1", "default")

		want := ReasonTargetConflict
		if overwrite {
			want = ReasonOverwriteUnsupported
		}
		wantFailedWith(t, c, want)
		cond := findReady(get(t, c, "r1", "default"))
		if msg := msgOrEmpty(cond); !strings.Contains(msg, "restore to a new name while g1 still exists") || strings.Contains(msg, "delete it first") {
			t.Errorf("overwriteExisting=%v: %q", overwrite, msg)
		}
		var g client.Object = runningGuest("g1")
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(g), g); err != nil {
			t.Errorf("the guest must be left alone: %v", err)
		}
	}
}
