package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func TestPathSubdir_HappyPath(t *testing.T) {
	got := pathSubdir(HostPathBaseDir + "default-snap1")
	if got != "default-snap1" {
		t.Errorf("got %q, want default-snap1", got)
	}
}

func TestPathSubdir_TrailingSlashTolerated(t *testing.T) {
	got := pathSubdir(HostPathBaseDir + "default-snap1/")
	if got != "default-snap1" {
		t.Errorf("got %q, want default-snap1 (trailing slash)", got)
	}
}

func TestPathSubdir_RejectsParentTraversal(t *testing.T) {
	if got := pathSubdir(HostPathBaseDir + ".."); got != "" {
		t.Errorf("expected empty (refusal); got %q", got)
	}
}

func TestPathSubdir_RejectsEmptyTail(t *testing.T) {
	if got := pathSubdir(HostPathBaseDir); got != "" {
		t.Errorf("expected empty for bare base; got %q", got)
	}
}

func TestPathSubdir_RejectsNestedPath(t *testing.T) {
	// We only support one level under HostPathBaseDir; nested paths
	// (the operator manually crafted a hostPath like
	// /var/lib/kubeswift/snapshots/sub/dir) get rejected — the
	// finalizer would otherwise need to mkdir or rmdir intermediate
	// levels which is out of Phase 2 scope.
	if got := pathSubdir(HostPathBaseDir + "sub/dir"); got != "" {
		t.Errorf("expected empty (nested); got %q", got)
	}
}

func TestPathSubdir_RejectsWrongPrefix(t *testing.T) {
	if got := pathSubdir("/tmp/snap"); got != "" {
		t.Errorf("expected empty (wrong prefix); got %q", got)
	}
}

func TestEnsureFinalizer_LocalBackend_Adds(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	r, c := newReconciler(t, snap)
	if err := r.ensureFinalizer(context.Background(), snap); err != nil {
		t.Fatalf("ensureFinalizer: %v", err)
	}
	got := getSnap(t, c, "snap1", "default")
	if !hasFinalizer(got, HostPathFinalizer) {
		t.Errorf("finalizer not added; got %v", got.Finalizers)
	}
}

func TestEnsureFinalizer_CSIBackend_NoOp(t *testing.T) {
	snap := makeSwiftSnapshot("snap1", "default", "g1", "")
	r, c := newReconciler(t, snap)
	if err := r.ensureFinalizer(context.Background(), snap); err != nil {
		t.Fatalf("ensureFinalizer: %v", err)
	}
	got := getSnap(t, c, "snap1", "default")
	if hasFinalizer(got, HostPathFinalizer) {
		t.Errorf("CSI snapshot should not get hostPath finalizer; got %v", got.Finalizers)
	}
}

func TestEnsureFinalizer_Idempotent(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Finalizers = []string{HostPathFinalizer}
	r, c := newReconciler(t, snap)
	if err := r.ensureFinalizer(context.Background(), snap); err != nil {
		t.Fatalf("ensureFinalizer: %v", err)
	}
	got := getSnap(t, c, "snap1", "default")
	count := 0
	for _, f := range got.Finalizers {
		if f == HostPathFinalizer {
			count++
		}
	}
	if count != 1 {
		t.Errorf("finalizer count = %d, want 1 (idempotent)", count)
	}
}

func TestHandleDeletion_NoHostPathFinalizer_DoneImmediately(t *testing.T) {
	// SwiftSnapshot mid-deletion but without HostPathFinalizer — e.g.
	// CSI snapshot that only has unrelated finalizers, or a local
	// snapshot that was never marked Ready (so we never added our
	// finalizer). handleDeletion is a no-op: the apiserver GCs the
	// resource once the unrelated finalizers go away.
	snap := makeLocalSnap("snap1", "default", "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	// Need at least one finalizer for the fake client to accept the
	// object as "deleting" rather than "should already be GC'd".
	snap.Finalizers = []string{"some.other/finalizer"}
	r, _ := newReconciler(t, snap)
	done, err := r.handleDeletion(context.Background(), snap)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	if !done {
		t.Errorf("expected done=true (HostPathFinalizer not present)")
	}
}

func TestHandleDeletion_NoNodeName_DropsFinalizer(t *testing.T) {
	// Failed before status.NodeName was recorded: no node to schedule
	// the cleanup pod on. Drop the finalizer rather than block deletion
	// indefinitely; orphan cleanup is operator-driven (out of scope).
	snap := makeLocalSnap("snap1", "default", "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	snap.Finalizers = []string{HostPathFinalizer}
	r, c := newReconciler(t, snap)

	done, err := r.handleDeletion(context.Background(), snap)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	if !done {
		t.Errorf("expected done=true (no node = drop finalizer)")
	}
	// Fake client GCs the object the moment the last finalizer is
	// removed (DeletionTimestamp + no finalizers = GC'd in real
	// kube too). NotFound here is the success signal.
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: "snap1", Namespace: "default"}, &snapshotv1alpha1.SwiftSnapshot{}); err == nil {
		t.Errorf("expected SwiftSnapshot to be GC'd (finalizer removed)")
	}
}

func TestHandleDeletion_CreatesCleanupPod(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	snap.Finalizers = []string{HostPathFinalizer}
	snap.Status.NodeName = "boba"
	r, c := newReconciler(t, snap)

	done, err := r.handleDeletion(context.Background(), snap)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	if done {
		t.Errorf("expected done=false (pod just created, requeue)")
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: cleanupPodName(snap), Namespace: "default"}, &pod); err != nil {
		t.Fatalf("cleanup pod not created: %v", err)
	}
	if pod.Spec.NodeName != "boba" {
		t.Errorf("cleanup pod node = %q, want boba", pod.Spec.NodeName)
	}
	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "default-snap1") {
		t.Errorf("cleanup args missing subdir name: %q", args)
	}
	// Defense check: must not rm the parent.
	if strings.Contains(args, "rm -rf "+HostPathBaseMount+" ") || strings.HasSuffix(args, "rm -rf "+HostPathBaseMount) {
		t.Errorf("cleanup args targets parent mount, not subdir: %q", args)
	}
}

// TestHandleLocalDeletion_Retain drops the finalizer WITHOUT running a cleanup
// pod when deletionPolicy: Retain.
func TestHandleLocalDeletion_Retain(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	snap.Finalizers = []string{HostPathFinalizer}
	snap.Status.NodeName = "boba"
	snap.Spec.DeletionPolicy = snapshotv1alpha1.SnapshotDeletionPolicyRetain
	r, c := newReconciler(t, snap)

	done, err := r.handleDeletion(context.Background(), snap)
	if err != nil || !done {
		t.Fatalf("Retain should drop the finalizer immediately; done=%v err=%v", done, err)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: cleanupPodName(snap), Namespace: "default"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("Retain must NOT create a cleanup pod; err=%v", err)
	}
	// Dropping the last finalizer on a deletion-stamped object lets it GC —
	// the snapshot is now gone, proving the finalizer was removed without a purge.
	var got snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &got); !apierrors.IsNotFound(err) {
		t.Errorf("Retain should drop the finalizer and let the snapshot GC; get err=%v", err)
	}
}

func TestHandleDeletion_PodSucceeded_RemovesFinalizer(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	snap.Finalizers = []string{HostPathFinalizer}
	snap.Status.NodeName = "boba"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cleanupPodName(snap),
			Namespace: "default",
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	r, c := newReconciler(t, snap, pod)

	done, err := r.handleDeletion(context.Background(), snap)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	if !done {
		t.Errorf("expected done=true (pod succeeded)")
	}
	// Same GC-on-finalizer-removal semantics as
	// TestHandleDeletion_NoNodeName_DropsFinalizer.
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: "snap1", Namespace: "default"}, &snapshotv1alpha1.SwiftSnapshot{}); err == nil {
		t.Errorf("expected SwiftSnapshot to be GC'd after successful cleanup")
	}
}

func TestHandleDeletion_PodFailed_RetainsFinalizer(t *testing.T) {
	// Failed cleanup: leave the finalizer so the operator sees the
	// pod via `kubectl describe`. Don't auto-retry (would loop on a
	// permanent failure like missing host directory).
	snap := makeLocalSnap("snap1", "default", "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	snap.Finalizers = []string{HostPathFinalizer}
	snap.Status.NodeName = "boba"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cleanupPodName(snap),
			Namespace: "default",
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	r, c := newReconciler(t, snap, pod)

	done, err := r.handleDeletion(context.Background(), snap)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	if done {
		t.Errorf("expected done=false (pod failed; finalizer retained)")
	}
	got := getSnap(t, c, "snap1", "default")
	if !hasFinalizer(got, HostPathFinalizer) {
		t.Errorf("finalizer should be retained on cleanup failure")
	}
}

func TestHandleDeletion_MalformedHostPath_DropsFinalizer(t *testing.T) {
	// Belt-and-suspenders: if a SwiftSnapshot somehow has a hostPath
	// outside the base prefix (operator hand-edit, etcd corruption,
	// pre-Phase-2 resource), refuse to construct a cleanup command
	// and just drop the finalizer. Operator can clean up by hand.
	snap := makeLocalSnap("snap1", "default", "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	snap.Finalizers = []string{HostPathFinalizer}
	snap.Status.NodeName = "boba"
	snap.Spec.Backend.Local.HostPath = "/tmp/something-weird"
	r, c := newReconciler(t, snap)

	done, err := r.handleDeletion(context.Background(), snap)
	if err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	if !done {
		t.Errorf("expected done=true (malformed hostPath = drop finalizer)")
	}
	// Fake client GCs the object the moment the last finalizer is
	// removed (DeletionTimestamp + no finalizers = GC'd in real
	// kube too). NotFound here is the success signal.
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: "snap1", Namespace: "default"}, &snapshotv1alpha1.SwiftSnapshot{}); err == nil {
		t.Errorf("expected SwiftSnapshot to be GC'd (finalizer removed)")
	}
}

// getSnap is a localized helper for cleanup tests so we don't fight
// the existing controller_test.go's `get` (which returns the typed
// SwiftSnapshot via the same fake client used by other tests).
func getSnap(t *testing.T, c client.Client, name, ns string) *snapshotv1alpha1.SwiftSnapshot {
	t.Helper()
	var s snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(),
		client.ObjectKey{Name: name, Namespace: ns}, &s); err != nil {
		t.Fatalf("get SwiftSnapshot: %v", err)
	}
	return &s
}
