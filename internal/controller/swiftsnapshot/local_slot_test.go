package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func slotOf(t *testing.T, c client.Client) map[string]string {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(t.Context(), client.ObjectKey{Name: "g1", Namespace: "default"}, &p); err != nil {
		t.Fatal(err)
	}
	return p.Annotations
}

func readyCond(snap *snapshotv1alpha1.SwiftSnapshot) (string, string) {
	if c := apimeta.FindStatusCondition(snap.Status.Conditions, "Ready"); c != nil {
		return c.Reason, c.Message
	}
	return "", ""
}

// Two snapshots of one guest started together (#735): the second waits for
// the first instead of replacing its capture, and goes once the first has
// read its result.
func TestLocal_SecondCaptureWaitsForTheFirst(t *testing.T) {
	a := makeLocalSnap("a", "default", "g1")
	b := makeLocalSnap("b", "default", "g1")
	b.UID = "cafebabe-1234-5678-9abc-def012345678"
	r, c := newReconciler(t, a, b, makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))

	reconcile(t, r, "a", "default")
	reconcile(t, r, "b", "default")
	if got := get(t, c, "a", "default"); got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Fatalf("a: phase %s, want Capturing", got.Status.Phase)
	}
	gotB := get(t, c, "b", "default")
	reason, msg := readyCond(gotB)
	if gotB.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending || reason != ReasonCaptureInProgress || !strings.Contains(msg, "SwiftSnapshot a ") {
		t.Fatalf("b: %s %s %q, want Pending/%s naming a", gotB.Status.Phase, reason, msg, ReasonCaptureInProgress)
	}
	if gotB.Status.CaptureStartedAt != nil || gotB.Status.NodeName != "" {
		t.Errorf("b recorded a capture it has not sent: startedAt=%v node=%q", gotB.Status.CaptureStartedAt, gotB.Status.NodeName)
	}
	if id := slotOf(t, c)[annoActionID]; id != "a-deadbeef" {
		t.Fatalf("slot = %q, want a's capture still in it", id)
	}

	// The launcher reports a's capture; a reads it and goes Ready.
	pod := &corev1.Pod{}
	_ = c.Get(t.Context(), client.ObjectKey{Name: "g1", Namespace: "default"}, pod)
	pod.Annotations[annoStatusID], pod.Annotations[annoStatus] = "a-deadbeef", "ready"
	if err := c.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "b", "default")
	if got := get(t, c, "b", "default"); got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending {
		t.Fatalf("b went while a had not yet read its result: %s", got.Status.Phase)
	}
	reconcile(t, r, "a", "default")
	if got := get(t, c, "a", "default"); got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady {
		t.Fatalf("a: phase %s, want Ready", got.Status.Phase)
	}
	reconcile(t, r, "b", "default")
	gotB = get(t, c, "b", "default")
	if gotB.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing || gotB.Status.CaptureStartedAt == nil {
		t.Fatalf("b: phase %s startedAt %v, want Capturing from now", gotB.Status.Phase, gotB.Status.CaptureStartedAt)
	}
	if id := slotOf(t, c)[annoActionID]; id != "b-cafebabe" {
		t.Errorf("slot = %q, want b's capture", id)
	}
}

// An action the launcher has not finished, such as a restore's resume, holds
// the slot; a finished one, or this snapshot's own, does not.
func TestLocal_CaptureWaitsForAnUnfinishedAction(t *testing.T) {
	cases := []struct {
		name string
		slot map[string]string
		wait bool
	}{
		{"unfinished resume", map[string]string{annoActionID: "r1-resume"}, true},
		{"resume running", map[string]string{annoActionID: "r1-resume", annoStatusID: "r1-resume", annoStatus: "running"}, true},
		{"resume finished", map[string]string{annoActionID: "r1-resume", annoStatusID: "r1-resume", annoStatus: "ready"}, false},
		{"resume rejected", map[string]string{annoActionID: "r1-resume", annoStatusID: "r1-resume", annoStatus: "rejected"}, false},
		{"its own capture", map[string]string{annoActionID: "snap1-deadbeef"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := makeLauncherPod("default", "g1", "worker-1")
			pod.Annotations = tc.slot
			r, c := newReconciler(t, makeLocalSnap("snap1", "default", "g1"), makeGuest("default", "g1"), pod)
			reconcile(t, r, "snap1", "default")
			got := get(t, c, "snap1", "default")
			reason, msg := readyCond(got)
			waited := got.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhasePending && reason == ReasonCaptureInProgress
			if waited != tc.wait {
				t.Fatalf("phase %s %s %q; want waiting=%v", got.Status.Phase, reason, msg, tc.wait)
			}
			if tc.wait && !strings.Contains(msg, "r1-resume") {
				t.Errorf("message %q does not name the action", msg)
			}
		})
	}
}

// A capture decided on a stale read of the pod (the cache had not yet shown
// another capture's write) must not land: its patch conflicts.
func TestLocal_CaptureOnAStaleReadDoesNotReplaceTheSlot(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	pod := makeLauncherPod("default", "g1", "worker-1")
	sch := testScheme(t)
	base := fake.NewClientBuilder().WithScheme(sch).WithObjects(snap, makeGuest("default", "g1"), pod).
		WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).Build()
	var stale corev1.Pod
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(pod), &stale); err != nil {
		t.Fatal(err)
	}
	// Another capture's write the cache has not shown yet.
	other := stale.DeepCopy()
	other.Annotations = map[string]string{annoAction: verbCapture, annoActionID: "other-12345678"}
	if err := base.Update(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if p, ok := obj.(*corev1.Pod); ok && key.Name == "g1" {
			stale.DeepCopyInto(p)
			return nil
		}
		return cl.Get(ctx, key, obj, opts...)
	}})
	r := &SwiftSnapshotReconciler{Client: c, Scheme: sch}
	reconcile(t, r, "snap1", "default")

	if got := get(t, base, "snap1", "default"); got.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Fatal("a capture decided on a stale read was sent")
	}
	if id := slotOf(t, base)[annoActionID]; id != "other-12345678" {
		t.Errorf("slot = %q, want the other capture kept", id)
	}
}
