package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// migratedGuest is g1 after a live migration: its launcher is the renamed
// destination pod g1-mig-abc, which status.podRef names and which still
// carries the completed migration's receive action.
func migratedGuest() (*swiftv1alpha1.SwiftGuest, *corev1.Pod) {
	g := makeGuest("default", "g1")
	g.Status.PodRef = &corev1.ObjectReference{Kind: "Pod", Name: "g1-mig-abc", Namespace: "default"}
	pod := makeLauncherPod("default", "g1", "worker-2")
	pod.Name = "g1-mig-abc"
	pod.Labels = map[string]string{launcherGuestLabel: "g1"}
	pod.Annotations = map[string]string{
		annoMigrationAction:                "receive",
		annoMigrationActionID:              "m1:recv:1",
		annoMigrationActionArgs:            "{}",
		"kubeswift.io/migration-status":    "running",
		"kubeswift.io/migration-status-id": "m1:recv:1",
	}
	return g, pod
}

func migration(phase migrationv1alpha1.SwiftMigrationPhase) *migrationv1alpha1.SwiftMigration {
	return &migrationv1alpha1.SwiftMigration{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec:       migrationv1alpha1.SwiftMigrationSpec{GuestRef: migrationv1alpha1.SwiftMigrationGuestRef{Name: "g1"}},
		Status:     migrationv1alpha1.SwiftMigrationStatus{Phase: phase},
	}
}

func podAnnotations(t *testing.T, c client.Client, name string) map[string]string {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &pod); err != nil {
		t.Fatal(err)
	}
	return pod.Annotations
}

// #824 + #823: a snapshot of a migrated guest finds its renamed launcher and
// clears the ended migration's leftover action, then captures.
func TestLocal_SnapshotOfAMigratedGuestCaptures(t *testing.T) {
	g, pod := migratedGuest()
	snap := makeLocalSnap("snap1", "default", "g1")
	r, c := newReconciler(t, snap, g, pod, migration(migrationv1alpha1.SwiftMigrationPhaseCompleted))
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Fatalf("phase = %s (%s), want Capturing", got.Status.Phase, findReady(got).Message)
	}
	a := podAnnotations(t, c, "g1-mig-abc")
	if a[annoActionID] != capturingActionID(snap) {
		t.Errorf("capture not written on the migrated launcher: %v", a)
	}
	if _, ok := a[annoMigrationAction]; ok {
		t.Errorf("the ended migration's action is still on the launcher: %v", a)
	}
}

// A migration still under way holds the launcher: the capture waits.
func TestLocal_CaptureWaitsForARunningMigration(t *testing.T) {
	g, pod := migratedGuest()
	snap := makeLocalSnap("snap1", "default", "g1")
	r, c := newReconciler(t, snap, g, pod, migration(migrationv1alpha1.SwiftMigrationPhaseStopAndCopy))
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending || !strings.Contains(findReady(got).Message, "SwiftMigration m1") {
		t.Errorf("phase = %s %q, want Pending waiting for SwiftMigration m1", got.Status.Phase, findReady(got).Message)
	}
	a := podAnnotations(t, c, "g1-mig-abc")
	if a[annoMigrationActionID] != "m1:recv:1" || a[annoActionID] != "" {
		t.Errorf("launcher annotations changed while the migration runs: %v", a)
	}
}

// #823: a terminal snapshot clears its own capture from the launcher, and
// only its own.
func TestTerminalSnapshotClearsItsCapture(t *testing.T) {
	for _, phase := range []snapshotv1alpha1.SwiftSnapshotPhase{snapshotv1alpha1.SwiftSnapshotPhaseReady, snapshotv1alpha1.SwiftSnapshotPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			snap := makeLocalSnap("snap1", "default", "g1")
			snap.Status.Phase = phase
			pod := makeLauncherPod("default", "g1", "worker-1")
			pod.Annotations = map[string]string{
				annoAction: verbCapture, annoActionID: capturingActionID(snap), annoActionArgs: "{}",
				annoStatus: "ready", annoStatusID: capturingActionID(snap),
			}
			other := makeLocalSnap("snap2", "default", "g1")
			other.UID = "0ther000-0000"
			other.Status.Phase = phase
			r, c := newReconciler(t, snap, other, makeGuest("default", "g1"), pod)

			reconcile(t, r, "snap2", "default")
			if podAnnotations(t, c, "g1")[annoActionID] != capturingActionID(snap) {
				t.Fatal("another snapshot cleared this snapshot's capture")
			}
			reconcile(t, r, "snap1", "default")
			a := podAnnotations(t, c, "g1")
			for _, k := range []string{annoAction, annoActionID, annoActionArgs} {
				if _, ok := a[k]; ok {
					t.Errorf("%s still set after the snapshot ended: %v", k, a)
				}
			}
			if a[annoStatus] != "ready" {
				t.Errorf("swiftletd's status was touched: %v", a)
			}
		})
	}
}

// The resume queued behind an abandoned capture stays until swiftletd has
// run it, so the guest is not left paused.
func TestFailedSnapshotKeepsItsResumeUntilItRan(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseFailed
	resumeID := capturingActionID(snap) + "-resume"
	pod := makeLauncherPod("default", "g1", "worker-1")
	pod.Annotations = map[string]string{annoAction: verbResume, annoActionID: resumeID, annoActionArgs: "{}",
		annoStatus: "running", annoStatusID: resumeID}
	r, c := newReconciler(t, snap, makeGuest("default", "g1"), pod)

	reconcile(t, r, "snap1", "default")
	if podAnnotations(t, c, "g1")[annoActionID] != resumeID {
		t.Fatal("the resume was cleared before it ran")
	}

	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Annotations[annoStatus] = "ready"
	if err := c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if reqs := r.podToSnapshots(context.Background(), &p); len(reqs) != 1 {
		t.Errorf("pod event mapped to %d requests, want the Failed snapshot with the queued resume", len(reqs))
	}
	reconcile(t, r, "snap1", "default")
	if _, ok := podAnnotations(t, c, "g1")[annoActionID]; ok {
		t.Error("the finished resume was not cleared")
	}
}

// A migrated launcher's events reach the snapshots of its guest.
func TestPodToSnapshots_MapsAMigratedLauncher(t *testing.T) {
	_, pod := migratedGuest()
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	r, _ := newReconciler(t, snap)
	if reqs := r.podToSnapshots(context.Background(), pod); len(reqs) != 1 || reqs[0].Name != "snap1" {
		t.Errorf("requests = %v, want snap1", reqs)
	}
}
