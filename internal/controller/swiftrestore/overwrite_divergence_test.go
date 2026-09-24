package swiftrestore

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	swiftguestctrl "github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

func wantFailedWith(t *testing.T, c client.Client, reason string) {
	t.Helper()
	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != reason {
		t.Errorf("Ready reason = %q (%s), want %s", reasonOrEmpty(cond), msgOrEmpty(cond), reason)
	}
}

func runningGuest(name string) *swiftv1alpha1.SwiftGuest {
	g := makeSourceGuest("default", name, "ubuntu-noble")
	g.Status.Conditions = []metav1.Condition{{Type: conditionGuestRunning, Status: metav1.ConditionTrue, Reason: "VmRunning"}}
	return g
}

// overwriteExisting over a guest the CSI restore did not create used to be a
// silent no-op: its root PVC and the guest both already existed, so nothing
// was created, the running guest satisfied Resuming, and the restore reported
// "restore complete" with nothing restored.
func TestCSI_OverwriteExistingGuestFailsInsteadOfReportingReady(t *testing.T) {
	snap := makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30)
	source := makeSourceGuest("default", "g1", "ubuntu-noble")
	restore := makeRestore("r1", "default", "snap1", "target", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	r, c := newReconciler(t, restore, snap, source, runningGuest("target"), makeBoundSourcePVC("default", "target", "sc"))

	for i := 0; i < 4; i++ {
		reconcile(t, r, "r1", "default")
	}
	wantFailedWith(t, c, ReasonOverwriteUnsupported)
}

// A re-run that finds the target this restore created is not a conflict.
func TestCSI_OwnTargetIsNotAConflict(t *testing.T) {
	snap := makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30)
	source := makeSourceGuest("default", "g1", "ubuntu-noble")
	own := makeSourceGuest("default", "target", "ubuntu-noble")
	own.Labels = map[string]string{swiftRestoreOwnerLabel: "r1"}
	restore := makeRestore("r1", "default", "snap1", "target", true)
	r, c := newReconciler(t, restore, snap, source, own)

	reconcile(t, r, "r1", "default")
	if got := get(t, c, "r1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring {
		t.Errorf("phase = %s, want Restoring", got.Status.Phase)
	}
}

// The same no-op on the memory path: a clone restore onto an existing,
// unrelated guest returned that guest unchanged, "resumed" it and went Ready.
func TestLocal_CloneOverwriteExistingGuestFailsInsteadOfReportingReady(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("r1", "default", "snap1", "other", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	restore.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{snapshotv1alpha1.RegenMACAddresses},
	}
	r, c := newReconciler(t, snap, restore, makeSourceGuest("default", "g1", "ubuntu-noble"), runningGuest("other"))
	r.CurrentHypervisorVersion = "v51.1"

	reconcile(t, r, "r1", "default")
	wantFailedWith(t, c, ReasonOverwriteUnsupported)
	var other swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "other", Namespace: "default"}, &other); err != nil {
		t.Fatal(err)
	}
	if other.Annotations[swiftguestctrl.AnnotationActiveRestore] != "" {
		t.Error("the unrelated guest was stamped for restore")
	}
}

// A memory snapshot holds no disk. When the guest kept running after the
// capture (resumeAfterSnapshot, the default), an in-place restore resumes the
// old RAM over the newer disk -- silent filesystem corruption. It must refuse,
// and leave the guest untouched, unless the operator explicitly accepts it.
func TestLocal_InPlaceRestoreRefusesADivergedDisk(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	snap.Spec.ResumeAfterSnapshot = true
	restore := makeRestore("r1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"}}
	r, c := newReconciler(t, snap, restore, makeSourceGuest("default", "g1", "ubuntu-noble"), pod)
	r.CurrentHypervisorVersion = "v51.1"

	reconcile(t, r, "r1", "default")
	wantFailedWith(t, c, ReasonDiskDiverged)
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &g); err != nil {
		t.Fatal(err)
	}
	if g.Annotations[swiftguestctrl.AnnotationActiveRestore] != "" {
		t.Error("a refused restore must not stamp the guest")
	}
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &p); err != nil {
		t.Errorf("a refused restore must not kill the running launcher: %v", err)
	}
}

func TestLocal_InPlaceRestoreProceedsWhenDivergenceIsAccepted(t *testing.T) {
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	snap.Spec.ResumeAfterSnapshot = true
	restore := makeRestore("r1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	restore.Annotations = map[string]string{snapshotv1alpha1.AnnotationAcceptDiskDivergence: "true"}
	r, c := newReconciler(t, snap, restore, makeSourceGuest("default", "g1", "ubuntu-noble"))
	r.CurrentHypervisorVersion = "v51.1"

	reconcile(t, r, "r1", "default")
	if got := get(t, c, "r1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring {
		t.Errorf("phase = %s, want Restoring with the risk accepted", got.Status.Phase)
	}
}

// inPlaceRestoring is guest g1 stamped by in-place restore r1, now Restoring.
func inPlaceRestoring(t *testing.T, resume bool, pod *corev1.Pod) (*SwiftRestoreReconciler, client.Client) {
	t.Helper()
	snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
	restore := makeRestore("r1", "default", "snap1", "g1", resume)
	restore.Spec.TargetGuest.OverwriteExisting = true
	restore.Status.Phase = snapshotv1alpha1.SwiftRestorePhaseRestoring
	restore.Status.GuestRef = &snapshotv1alpha1.SwiftRestoreGuestRef{Name: "g1"}
	guest := runningGuest("g1")
	guest.Annotations = map[string]string{
		swiftguestctrl.AnnotationActiveRestore:       "r1",
		swiftguestctrl.AnnotationRestoreMode:         swiftguestctrl.RestoreModeInPlace,
		swiftguestctrl.AnnotationRestoreSnapshotPath: "/var/lib/kubeswift/snapshots/default-snap1",
		swiftguestctrl.AnnotationRestoreNodeName:     "worker-1",
	}
	objs := []client.Object{snap, restore, guest}
	if pod != nil {
		// The restore's own launcher, as the guest controller builds it and
		// the guest's status then names it.
		if pod.Labels == nil {
			pod.Labels = map[string]string{}
		}
		pod.Labels[swiftguestctrl.PodRoleLabel] = swiftguestctrl.PodRoleRestoreReceive
		if pod.UID == "" {
			pod.UID = "restore-launcher"
		}
		// Created after the restore started (the fake client stamps no
		// creation time; the restore's start is its first reconcile).
		if pod.CreationTimestamp.IsZero() {
			pod.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
		}
		guest.Status.PodRef = &corev1.ObjectReference{Name: pod.Name, UID: pod.UID}
		objs = append(objs, pod)
	}
	return newReconciler(t, objs...)
}

func guestAnnotations(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &g); err != nil {
		t.Fatal(err)
	}
	return g.Annotations
}

// resumeAfterRestore=false leaves the VM paused: once the launcher has loaded
// the snapshot the restore is done, and no resume is sent. The in-place path
// used to resume it regardless.
func TestLocal_NoResumeLeavesTheVMPaused(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"}}
	r, c := inPlaceRestoring(t, false, pod)

	for i := 0; i < 3; i++ {
		reconcile(t, r, "r1", "default")
	}
	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseReady {
		t.Fatalf("phase = %s, want Ready (paused)", got.Status.Phase)
	}
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &p); err != nil {
		t.Fatal(err)
	}
	if v := p.Annotations[annoActionID]; v != "" {
		t.Errorf("a resume action (%s) was sent with resumeAfterRestore=false", v)
	}
	if a := guestAnnotations(t, c); a[swiftguestctrl.AnnotationActiveRestore] != "" {
		t.Error("restore annotations should be cleared once the restore is done")
	}
}

// A failed in-place restore must hand the guest back: its restore annotations
// route every launcher to the snapshot, so leaving them on retried the failed
// restore on every relaunch and the guest never booted from its disk again.
func TestLocal_FailedInPlaceRestoreReleasesTheGuest(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	r, c := inPlaceRestoring(t, true, pod)
	var restore snapshotv1alpha1.SwiftRestore
	if err := c.Get(context.Background(), client.ObjectKey{Name: "r1", Namespace: "default"}, &restore); err != nil {
		t.Fatal(err)
	}
	restore.Status.Phase = snapshotv1alpha1.SwiftRestorePhaseResuming
	if err := c.Status().Update(context.Background(), &restore); err != nil {
		t.Fatal(err)
	}

	reconcile(t, r, "r1", "default")
	if got := get(t, c, "r1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Fatalf("phase = %s, want Failed (launcher died before resume)", got.Status.Phase)
	}
	if a := guestAnnotations(t, c); a[swiftguestctrl.AnnotationActiveRestore] != "" || a[swiftguestctrl.AnnotationRestoreMode] != "" {
		t.Errorf("failed in-place restore left the guest stamped: %v", a)
	}
}

// Annotations naming a different restore (or a cloneFromSnapshot snapshot)
// are not this restore's to remove.
func TestLocal_FailedRestoreLeavesOtherRestoresAnnotations(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	r, c := inPlaceRestoring(t, true, pod)
	ctx := context.Background()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, client.ObjectKey{Name: "g1", Namespace: "default"}, &g); err != nil {
		t.Fatal(err)
	}
	g.Annotations[swiftguestctrl.AnnotationActiveRestore] = "someone-else"
	if err := c.Update(ctx, &g); err != nil {
		t.Fatal(err)
	}
	var restore snapshotv1alpha1.SwiftRestore
	if err := c.Get(ctx, client.ObjectKey{Name: "r1", Namespace: "default"}, &restore); err != nil {
		t.Fatal(err)
	}
	restore.Status.Phase = snapshotv1alpha1.SwiftRestorePhaseResuming
	if err := c.Status().Update(ctx, &restore); err != nil {
		t.Fatal(err)
	}

	reconcile(t, r, "r1", "default")
	if a := guestAnnotations(t, c); a[swiftguestctrl.AnnotationActiveRestore] != "someone-else" {
		t.Errorf("another restore's annotations were removed: %v", a)
	}
}

// resumeAfterSnapshot=false keeps the captured VM paused, but a launcher
// started after the capture booted from the disk anyway (the guest controller
// replaces a killed launcher), so that disk has moved on too. The launcher
// that was captured, started before the capture, is fine.
func TestLocal_InPlaceRestoreRefusesAGuestRelaunchedSinceTheCapture(t *testing.T) {
	captured := metav1.Now()
	for _, tc := range []struct {
		name       string
		podCreated metav1.Time
		wantPhase  snapshotv1alpha1.SwiftRestorePhase
	}{
		{"relaunched after the capture", metav1.NewTime(captured.Add(time.Minute)), snapshotv1alpha1.SwiftRestorePhaseFailed},
		{"the captured launcher", metav1.NewTime(captured.Add(-time.Hour)), snapshotv1alpha1.SwiftRestorePhaseRestoring},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := makeLocalSnapWithBackend("snap1", "default", "g1", "/var/lib/kubeswift/snapshots/default-snap1", "v51.1")
			snap.Spec.ResumeAfterSnapshot = false
			snap.Status.CapturedAt = &captured
			restore := makeRestore("r1", "default", "snap1", "g1", true)
			restore.Spec.TargetGuest.OverwriteExisting = true
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default", CreationTimestamp: tc.podCreated}}
			r, c := newReconciler(t, snap, restore, makeSourceGuest("default", "g1", "ubuntu-noble"), pod)
			r.CurrentHypervisorVersion = "v51.1"

			reconcile(t, r, "r1", "default")
			got := get(t, c, "r1", "default")
			if got.Status.Phase != tc.wantPhase {
				t.Errorf("phase = %s (%s), want %s", got.Status.Phase, msgOrEmpty(findReady(got)), tc.wantPhase)
			}
		})
	}
}

// Restored in place, the guest keeps its old launcher's status until the new
// launcher reports, and that says GuestRunning=True: the VM the snapshot left
// paused. The restore must not take it for its own launcher's: it used to
// resume the old VM (its dying swiftletd still polls the pod by name) and
// report Ready in seconds, while the new launcher, its intent rewritten
// without the restore, booted the guest cold. The e2e found the sentinel
// gone.
func TestLocal_InPlaceRestoreWaitsForItsOwnLauncher(t *testing.T) {
	ctx := context.Background()
	started := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	for _, tc := range []struct {
		name string
		pod  corev1.Pod
		ref  types.UID // the pod the guest's status describes
	}{
		{
			name: "status still describes the replaced launcher",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default", UID: "new",
				Labels:            map[string]string{swiftguestctrl.PodRoleLabel: swiftguestctrl.PodRoleRestoreReceive},
				CreationTimestamp: metav1.NewTime(started.Add(time.Second))}},
			ref: "old",
		},
		{
			name: "the name still resolves to the replaced launcher",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default", UID: "old",
				CreationTimestamp: metav1.NewTime(started.Add(-time.Hour))}},
			ref: "old",
		},
		{
			name: "an earlier restore's launcher",
			pod: corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default", UID: "earlier",
				Labels:            map[string]string{swiftguestctrl.PodRoleLabel: swiftguestctrl.PodRoleRestoreReceive},
				CreationTimestamp: metav1.NewTime(started.Add(-time.Hour))}},
			ref: "earlier",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := tc.pod
			r, c := inPlaceRestoring(t, true, nil)
			if err := c.Create(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			var g swiftv1alpha1.SwiftGuest
			if err := c.Get(ctx, client.ObjectKey{Name: "g1", Namespace: "default"}, &g); err != nil {
				t.Fatal(err)
			}
			g.Status.PodRef = &corev1.ObjectReference{Name: "g1", UID: tc.ref}
			if err := c.Update(ctx, &g); err != nil {
				t.Fatal(err)
			}
			var restore snapshotv1alpha1.SwiftRestore
			if err := c.Get(ctx, client.ObjectKey{Name: "r1", Namespace: "default"}, &restore); err != nil {
				t.Fatal(err)
			}
			restore.Status.StartedAt = &started
			if err := c.Status().Update(ctx, &restore); err != nil {
				t.Fatal(err)
			}

			for i := 0; i < 3; i++ {
				reconcile(t, r, "r1", "default")
			}
			if got := get(t, c, "r1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring {
				t.Fatalf("phase = %s, want Restoring until its own launcher reports", got.Status.Phase)
			}
			var p corev1.Pod
			if err := c.Get(ctx, client.ObjectKey{Name: "g1", Namespace: "default"}, &p); err != nil {
				t.Fatal(err)
			}
			if v := p.Annotations[annoActionID]; v != "" {
				t.Errorf("a resume action (%s) was sent to a launcher this restore did not start", v)
			}
		})
	}

	// Once the guest's status names the restore's launcher, the restore
	// resumes that launcher.
	t.Run("its own launcher reported", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default", UID: "new",
			CreationTimestamp: metav1.NewTime(started.Add(time.Second))}}
		r, c := inPlaceRestoring(t, true, pod)
		var restore snapshotv1alpha1.SwiftRestore
		if err := c.Get(ctx, client.ObjectKey{Name: "r1", Namespace: "default"}, &restore); err != nil {
			t.Fatal(err)
		}
		restore.Status.StartedAt = &started
		if err := c.Status().Update(ctx, &restore); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			reconcile(t, r, "r1", "default")
		}
		var p corev1.Pod
		if err := c.Get(ctx, client.ObjectKey{Name: "g1", Namespace: "default"}, &p); err != nil {
			t.Fatal(err)
		}
		if p.Annotations[annoAction] != verbResume {
			t.Errorf("the restore's own launcher got no resume action: %v", p.Annotations)
		}
	})
}
