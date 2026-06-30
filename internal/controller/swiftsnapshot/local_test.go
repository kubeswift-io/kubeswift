package swiftsnapshot

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// fakeUID is a deterministic UID for test fixtures. The action-id
// helper takes the first 8 chars, so the expected action-id for any
// makeLocalSnap("name", ...) is "name-deadbeef".
const fakeUID = "deadbeef-1234-5678-9abc-def012345678"

// makeLocalSnap returns a Phase-2 local-backend SwiftSnapshot with a
// single explicit hostPath under the operator-controlled prefix.
func makeLocalSnap(name, ns, guestName string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       fakeUID,
		},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: guestName},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{
					HostPath: HostPathBaseDir + ns + "-" + name,
				},
			},
			IncludeMemory:       true,
			ResumeAfterSnapshot: true,
		},
	}
}

// makeLauncherPod fabricates a launcher pod with the same name as the
// guest, scheduled on `node`, in PodRunning phase by default. Tests
// override Status.Phase as needed.
func makeLauncherPod(ns, name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestLocal_Pending_AdvancesToCapturing_AndWritesActionAnnotations(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Fatalf("phase = %s, want Capturing", got.Status.Phase)
	}
	if got.Status.NodeName != "boba" {
		t.Errorf("status.nodeName = %q, want boba", got.Status.NodeName)
	}
	if got.Status.SnapshotDirVersion != SnapshotDirVersionV1 {
		t.Errorf("status.snapshotDirVersion = %q, want %q", got.Status.SnapshotDirVersion, SnapshotDirVersionV1)
	}

	// The action annotations must be set on the launcher pod.
	var p corev1.Pod
	if err := c.Get(t.Context(), client.ObjectKey{Name: "g1", Namespace: "default"}, &p); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if p.Annotations[annoAction] != verbCapture {
		t.Errorf("pod annotation %s = %q, want %q", annoAction, p.Annotations[annoAction], verbCapture)
	}
	wantID := "snap1-deadbeef"
	if p.Annotations[annoActionID] != wantID {
		t.Errorf("pod annotation %s = %q, want %q", annoActionID, p.Annotations[annoActionID], wantID)
	}
	// Args must be valid JSON containing the expected destination URL.
	var args captureArgs
	if err := json.Unmarshal([]byte(p.Annotations[annoActionArgs]), &args); err != nil {
		t.Fatalf("parse action args: %v (raw=%s)", err, p.Annotations[annoActionArgs])
	}
	wantURL := "file://" + HostPathBaseDir + "default-snap1/"
	if args.DestinationURL != wantURL {
		t.Errorf("destination_url = %q, want %q", args.DestinationURL, wantURL)
	}
	if !args.ResumeAfterSnapshot {
		t.Errorf("resume_after_snapshot = false, want true (per spec)")
	}
}

func TestLocal_Pending_GuestNotFound_StaysPending(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "missing")
	r, c := newReconciler(t, snap)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending {
		t.Errorf("phase = %s, want Pending", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonGuestNotFound {
		t.Errorf("Ready reason = %q, want GuestNotFound", reasonOrEmpty(cond))
	}
}

func TestLocal_Pending_PodNotYetPresent_StaysPending(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	guest := makeGuest("default", "g1")

	r, c := newReconciler(t, snap, guest)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending {
		t.Errorf("phase = %s, want Pending", got.Status.Phase)
	}
}

func TestLocal_Pending_PodNotRunning_StaysPending(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	pod.Status.Phase = corev1.PodPending

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending {
		t.Errorf("phase = %s, want Pending", got.Status.Phase)
	}
}

func TestLocal_Capturing_StatusIDMismatch_Requeues(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	// Annotations carry a stale status-id from a prior run.
	pod.Annotations = map[string]string{
		annoStatusID: "snap1-OLD",
		annoStatus:   "ready",
	}

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Errorf("phase = %s, want Capturing (stale status-id should not finalize)", got.Status.Phase)
	}
}

func TestLocal_Capturing_StatusReady_FinalizesWithPauseWindow(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	snap.Status.NodeName = "boba"
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	pod.Annotations = map[string]string{
		annoStatusID:      "snap1-deadbeef",
		annoStatus:        "ready",
		annoStatusDetail:  "captured to file:///x (1234ms pause window, resumed)",
		annoPauseWindowMs: "1234",
	}

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady {
		t.Errorf("phase = %s, want Ready", got.Status.Phase)
	}
	if got.Status.ObservedPauseWindowMs != 1234 {
		t.Errorf("observedPauseWindowMs = %d, want 1234", got.Status.ObservedPauseWindowMs)
	}
	if got.Status.MemorySnapshot == nil || got.Status.MemorySnapshot.Handle == "" {
		t.Errorf("memorySnapshot.handle empty, want %q", snap.Spec.Backend.Local.HostPath)
	}
	if got.Status.CapturedAt == nil {
		t.Errorf("capturedAt nil, want set")
	}
}

func TestLocal_Capturing_StatusFailed_TransitionsToFailed(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	pod.Annotations = map[string]string{
		annoStatusID:     "snap1-deadbeef",
		annoStatus:       "failed",
		annoStatusDetail: "snapshot: VM not in Paused state",
	}

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Errorf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonSnapshotFailed {
		t.Errorf("Ready reason = %q, want SnapshotFailed", reasonOrEmpty(cond))
	}
}

func TestLocal_Capturing_StatusRejected_TransitionsToFailed(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	pod.Annotations = map[string]string{
		annoStatusID:     "snap1-deadbeef",
		annoStatus:       "rejected",
		annoStatusDetail: "rejected: action snap0-9 already in flight",
	}

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Errorf("phase = %s, want Failed (rejected = hard fail; operator can retry by reapplying)", got.Status.Phase)
	}
}

func TestLocal_Capturing_PodPhaseFailed_Surfaces_Failed(t *testing.T) {
	// Pod-phase observation. The Pod watcher in SetupWithManager makes
	// this observation prompt; the controller-side transition itself
	// works on any reconcile pass that sees PodFailed/PodSucceeded.
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	pod.Status.Phase = corev1.PodFailed

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Errorf("phase = %s, want Failed", got.Status.Phase)
	}
}

func TestLocal_Capturing_DeadlineExceeded_TransitionsToFailed(t *testing.T) {
	// Wall-clock safety net: a SwiftSnapshot stuck in Capturing past
	// its deadline force-fails even when the launcher pod is otherwise
	// alive (e.g. swiftletd hung on a kernel I/O wait).
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	// Pretend the SwiftSnapshot is older than the default deadline.
	snap.CreationTimestamp = metav1.Time{
		Time: metav1.Now().Add(-(time.Duration(DefaultCaptureDeadlineSeconds) + 60) * time.Second),
	}
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	pod.Annotations = map[string]string{
		annoStatusID: "snap1-deadbeef",
		annoStatus:   "running",
	}

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Fatalf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || !strings.Contains(cond.Message, "deadline") {
		t.Errorf("Ready message %q, want substring 'deadline'", cond.Message)
	}
}

func TestLocal_Capturing_AnnotationOverride_ExtendsDeadline(t *testing.T) {
	// Operator override via kubeswift.io/snapshot-deadline-seconds:
	// extending the deadline keeps a long-running snapshot in Capturing.
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	snap.Annotations = map[string]string{CaptureDeadlineAnnotation: "86400"} // 1 day
	// Snapshot is older than the default deadline but within the override.
	snap.CreationTimestamp = metav1.Time{
		Time: metav1.Now().Add(-(time.Duration(DefaultCaptureDeadlineSeconds) + 60) * time.Second),
	}
	guest := makeGuest("default", "g1")
	pod := makeLauncherPod("default", "g1", "boba")
	pod.Annotations = map[string]string{
		annoStatusID: "snap1-deadbeef",
		annoStatus:   "running",
	}

	r, c := newReconciler(t, snap, guest, pod)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Errorf("phase = %s, want Capturing (deadline override should keep us running)", got.Status.Phase)
	}
}

func TestLocal_PodWatcher_MapsCapturingSnapshotsOnly(t *testing.T) {
	// podToSnapshots must enqueue only Capturing snapshots that match
	// the pod's namespace and guest-name. Pending or terminal phases
	// are filtered out so we don't waste reconcile budget on snapshots
	// that don't observe Pod state.
	pod := makeLauncherPod("default", "g1", "boba")

	cap := makeLocalSnap("cap-snap", "default", "g1")
	cap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	pending := makeLocalSnap("pending-snap", "default", "g1")
	pending.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhasePending
	ready := makeLocalSnap("ready-snap", "default", "g1")
	ready.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseReady
	otherGuest := makeLocalSnap("other-snap", "default", "g2")
	otherGuest.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	otherNS := makeLocalSnap("ns-snap", "other", "g1")
	otherNS.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing

	r, _ := newReconciler(t, pod, cap, pending, ready, otherGuest, otherNS)

	reqs := r.podToSnapshots(t.Context(), pod)
	if len(reqs) != 1 {
		t.Fatalf("got %d reqs, want 1", len(reqs))
	}
	if reqs[0].Name != "cap-snap" {
		t.Errorf("req name = %q, want cap-snap", reqs[0].Name)
	}
}
