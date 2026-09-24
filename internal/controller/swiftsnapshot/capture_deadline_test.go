package swiftsnapshot

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// Sending the capture records when it started; the deadline runs from there.
func TestLocal_SendingTheCaptureRecordsItsStart(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	r, c := newReconciler(t, snap, makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))
	reconcile(t, r, "snap1", "default")
	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing || got.Status.CaptureStartedAt == nil {
		t.Fatalf("phase=%s captureStartedAt=%v; want Capturing with a start time", got.Status.Phase, got.Status.CaptureStartedAt)
	}
}

// A snapshot that waited in Pending longer than the capture deadline (the
// guest took a while to come up) used to fail on its first Capturing poll --
// after the capture had been sent and the guest paused. Only time since the
// capture was sent counts.
func TestLocal_TimeSpentPendingDoesNotCountAgainstTheCapture(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour)) // deadline is 600s
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	justNow := metav1.Now()
	snap.Status.CaptureStartedAt = &justNow
	r, c := newReconciler(t, snap, makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))

	reconcile(t, r, "snap1", "default")
	if got := get(t, c, "snap1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Errorf("phase = %s, want still Capturing: the capture itself has only just started", got.Status.Phase)
	}
}

// A full-state capture that does run past its deadline must not leave the
// guest paused with nobody to export and terminate it: a resume is queued.
func TestLocal_AbandonedFullStateCaptureQueuesAResume(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Spec.IncludeDisk = true
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	long := metav1.NewTime(time.Now().Add(-time.Hour))
	snap.Status.CaptureStartedAt = &long
	r, c := newReconciler(t, snap, makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))

	reconcile(t, r, "snap1", "default")
	if got := get(t, c, "snap1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Fatalf("phase = %s, want Failed past the deadline", got.Status.Phase)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[annoAction] != verbResume {
		t.Errorf("launcher action = %q, want a queued resume so the guest is not left paused", pod.Annotations[annoAction])
	}
}
