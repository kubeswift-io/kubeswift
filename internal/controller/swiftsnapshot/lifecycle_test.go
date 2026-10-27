package swiftsnapshot

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

// Snapshot node-cleanup lifecycle: a capture writes the guest's memory into a
// directory on the launcher's node, and only the snapshot's finalizer gets it
// removed. These tests hold the ordering that keeps every such directory
// owned: the finalizer and the node are persisted before the capture is sent,
// a deletion waits for a running capture, and a cleanup that cannot finish is
// retried or reported instead of holding the snapshot for good.

func newLifecycleReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) (*SwiftSnapshotReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(withTestNodes(objs)...).
		WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).
		WithInterceptorFuncs(funcs).
		Build()
	rec := record.NewFakeRecorder(20)
	return &SwiftSnapshotReconciler{Client: c, Scheme: scheme, ControllerNamespace: controllerNS, Recorder: rec}, c, rec
}

func podAction(t *testing.T, c client.Client, ns, name string) string {
	t.Helper()
	var p corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return p.Annotations[annoActionID]
}

func cleanupPods(t *testing.T, c client.Client) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(controllerNS), client.MatchingLabels{cleanupRoleLabel: cleanupRoleValue}); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

func drainEvents(rec *record.FakeRecorder) string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return strings.Join(out, "\n")
		}
	}
}

// The finalizer and the capture node are persisted before the launcher is
// told to capture: every write the controller makes to the snapshot that
// records the node lands before the action annotation does.
func TestCapture_OwnershipPersistedBeforeDispatch(t *testing.T) {
	var order []string
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			switch o := obj.(type) {
			case *corev1.Pod:
				// The only pod write before the capture runs is the action.
				order = append(order, "dispatch")
			case *snapshotv1alpha1.SwiftSnapshot:
				if hasFinalizer(o, HostPathFinalizer) {
					order = append(order, "finalizer")
				}
			}
			return cl.Patch(ctx, obj, p, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if s, ok := obj.(*snapshotv1alpha1.SwiftSnapshot); ok && s.Status.NodeName != "" {
				order = append(order, "node:"+string(s.Status.Phase))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	r, c, _ := newLifecycleReconciler(t, funcs, makeLocalSnap("snap1", "default", "g1"), makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))
	reconcile(t, r, "snap1", "default")

	got := strings.Join(order, ",")
	if !strings.HasPrefix(got, "finalizer,node:,dispatch") {
		t.Fatalf("write order = %s; the finalizer and node must be persisted before the capture is dispatched", got)
	}
	s := get(t, c, "snap1", "default")
	if !hasFinalizer(s, HostPathFinalizer) || s.Status.NodeName != "worker-1" || s.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Fatalf("finalizers=%v node=%q phase=%s", s.Finalizers, s.Status.NodeName, s.Status.Phase)
	}
}

// A conflict on the write that records the node leaves nothing on the node:
// the capture is not dispatched until a later pass records the node.
func TestCapture_StatusConflictBeforeDispatch_NoSideEffect(t *testing.T) {
	conflicts := 1
	funcs := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if s, ok := obj.(*snapshotv1alpha1.SwiftSnapshot); ok && s.Status.NodeName != "" && conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Resource: "swiftsnapshots"}, s.Name, nil)
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	r, c, _ := newLifecycleReconciler(t, funcs, makeLocalSnap("snap1", "default", "g1"), makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))
	reconcile(t, r, "snap1", "default")
	// The pass may still record the node (its progress write) but must not
	// have sent the capture: a capture is only sent after a write that
	// recorded its node succeeded in the same pass.
	if id := podAction(t, c, "default", "g1"); id != "" {
		t.Fatalf("capture %q dispatched after the write recording its node conflicted", id)
	}
	reconcile(t, r, "snap1", "default")
	s := get(t, c, "snap1", "default")
	if podAction(t, c, "default", "g1") == "" || s.Status.NodeName != "worker-1" || s.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Fatalf("second pass: node=%q phase=%s", s.Status.NodeName, s.Status.Phase)
	}
}

// The controller dies after dispatching and before writing Capturing (here:
// that write fails). The snapshot still names the node and holds the
// finalizer; deleting it waits for the capture to end, then cleans that node.
func TestCapture_LostCapturingWrite_DeletionWaitsThenCleans(t *testing.T) {
	funcs := interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if s, ok := obj.(*snapshotv1alpha1.SwiftSnapshot); ok && s.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
				return apierrors.NewServiceUnavailable("controller restarted")
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		},
	}
	r, c, rec := newLifecycleReconciler(t, funcs, makeLocalSnap("snap1", "default", "g1"), makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))
	_, _ = r.Reconcile(context.Background(), reqFor("snap1", "default"))

	s := get(t, c, "snap1", "default")
	if podAction(t, c, "default", "g1") != "snap1-deadbeef" {
		t.Fatal("capture was not dispatched")
	}
	if s.Status.NodeName != "worker-1" || !hasFinalizer(s, HostPathFinalizer) {
		t.Fatalf("after a lost Capturing write: node=%q finalizers=%v; the capture on worker-1 would be untracked", s.Status.NodeName, s.Finalizers)
	}

	if err := c.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "snap1", "default")
	if pods := cleanupPods(t, c); len(pods) != 0 {
		t.Fatalf("cleanup started while the launcher may still be writing the capture: %d pods", len(pods))
	}

	// The launcher reports the capture finished.
	var p corev1.Pod
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "g1"}, &p)
	p.Annotations[annoStatusID] = "snap1-deadbeef"
	p.Annotations[annoStatus] = "ready"
	if err := c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "snap1", "default")
	pods := cleanupPods(t, c)
	if len(pods) != 1 || pods[0].Spec.NodeName != "worker-1" || pods[0].Spec.Containers[0].Args[0] != HostPathBaseMount+"/default_snap1" {
		t.Fatalf("want one cleanup pod for default_snap1 on worker-1, got %+v", pods)
	}
	_ = drainEvents(rec)
}

// A snapshot an earlier version began has a capture node but no finalizer
// unless it reached Ready. It gains one, so deleting it cleans the node.
func TestLegacyFailedCapture_GetsFinalizerAndIsCleaned(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseFailed
	snap.Status.NodeName = "worker-2"
	snap.Status.MemorySnapshot = &snapshotv1alpha1.MemorySnapshotRef{Handle: HostPathBaseDir + "default_snap1"}
	r, c, _ := newLifecycleReconciler(t, interceptor.Funcs{}, snap)
	reconcile(t, r, "snap1", "default")
	s := get(t, c, "snap1", "default")
	if !hasFinalizer(s, HostPathFinalizer) {
		t.Fatal("a failed capture with a node got no cleanup finalizer")
	}
	if err := c.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "snap1", "default")
	if pods := cleanupPods(t, c); len(pods) != 1 || pods[0].Spec.NodeName != "worker-2" {
		t.Fatalf("want a cleanup pod on worker-2, got %d", len(pods))
	}
}

// A snapshot that never got a node (the guest was never found) needs no
// cleanup and no finalizer.
func TestPendingWithoutNode_NoFinalizer(t *testing.T) {
	r, c, _ := newLifecycleReconciler(t, interceptor.Funcs{}, makeLocalSnap("snap1", "default", "missing"))
	reconcile(t, r, "snap1", "default")
	if s := get(t, c, "snap1", "default"); len(s.Finalizers) != 0 {
		t.Fatalf("finalizers = %v on a snapshot that captured nothing", s.Finalizers)
	}
}

func deletingSnapOn(node string) *snapshotv1alpha1.SwiftSnapshot {
	s := makeLocalSnap("snap1", "default", "g1")
	s.Finalizers = []string{HostPathFinalizer}
	now := metav1.Now()
	s.DeletionTimestamp = &now
	s.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseReady
	s.Status.NodeName = node
	s.Status.MemorySnapshot = &snapshotv1alpha1.MemorySnapshotRef{Handle: HostPathBaseDir + "default_snap1"}
	return s
}

func failedCleanupPod(snap *snapshotv1alpha1.SwiftSnapshot, node string, ran bool, finished time.Time) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: cleanupPodName(snap), Namespace: controllerNS,
			Labels: map[string]string{cleanupRoleLabel: cleanupRoleValue, snapshotUIDLabel: string(snap.UID)}},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
	if ran {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "rm", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 1, StartedAt: metav1.NewTime(finished.Add(-time.Second)), FinishedAt: metav1.NewTime(finished), Message: "rm: Device or resource busy"}}}}
	} else {
		// The kubelet's admission rejection, as on the stuck lab pods.
		p.Status.Reason = "Evicted"
		p.Status.Message = "Pod was rejected: The node had condition: [DiskPressure]."
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "rm", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 137, Reason: "ContainerStatusUnknown"}}}}
	}
	return p
}

// A cleanup pod the kubelet rejected never ran: it is replaced at once, with
// a Warning event, instead of holding the snapshot in Terminating.
func TestCleanupPod_NeverRan_IsReplaced(t *testing.T) {
	snap := deletingSnapOn("worker-1")
	r, c, rec := newLifecycleReconciler(t, interceptor.Funcs{}, snap, failedCleanupPod(snap, "worker-1", false, time.Time{}))
	reconcile(t, r, "snap1", "default")
	if pods := cleanupPods(t, c); len(pods) != 0 {
		t.Fatalf("the rejected pod was not removed: %d pods", len(pods))
	}
	if ev := drainEvents(rec); !strings.Contains(ev, ReasonNodeCleanupRetry) || !strings.Contains(ev, "did not run") || !strings.Contains(ev, "DiskPressure") {
		t.Errorf("events = %q", ev)
	}
	reconcile(t, r, "snap1", "default")
	if pods := cleanupPods(t, c); len(pods) != 1 || pods[0].Status.Phase != "" {
		t.Fatalf("a new cleanup pod should have been created, got %d", len(pods))
	}
}

// A cleanup pod whose rm failed stays for cleanupRetryBackoff (the error is
// readable on it), then is replaced, with a Warning event naming the error.
func TestCleanupPod_RmFailed_RetriedAfterBackoff(t *testing.T) {
	snap := deletingSnapOn("worker-1")
	r, c, _ := newLifecycleReconciler(t, interceptor.Funcs{}, snap, failedCleanupPod(snap, "worker-1", true, time.Now()))
	reconcile(t, r, "snap1", "default")
	if pods := cleanupPods(t, c); len(pods) != 1 || pods[0].Status.Phase != corev1.PodFailed {
		t.Fatal("a pod that just failed must stay for the backoff")
	}

	snap = deletingSnapOn("worker-1")
	r, c, rec := newLifecycleReconciler(t, interceptor.Funcs{}, snap, failedCleanupPod(snap, "worker-1", true, time.Now().Add(-2*cleanupRetryBackoff)))
	reconcile(t, r, "snap1", "default")
	if pods := cleanupPods(t, c); len(pods) != 0 {
		t.Fatal("a pod failed longer than the backoff ago must be replaced")
	}
	if ev := drainEvents(rec); !strings.Contains(ev, "exit code 1") || !strings.Contains(ev, "busy") {
		t.Errorf("events = %q", ev)
	}
}

// A cleanup pod deleted while it ran is created again.
func TestCleanupPod_DeletedMidRun_IsRecreated(t *testing.T) {
	snap := deletingSnapOn("worker-1")
	r, c, _ := newLifecycleReconciler(t, interceptor.Funcs{}, snap)
	reconcile(t, r, "snap1", "default")
	pods := cleanupPods(t, c)
	if len(pods) != 1 {
		t.Fatal("no cleanup pod")
	}
	if err := c.Delete(context.Background(), &pods[0]); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "snap1", "default")
	if len(cleanupPods(t, c)) != 1 {
		t.Fatal("the deleted cleanup pod was not recreated")
	}
}

// Cleanup completes once the pod succeeds, and running it again (the
// directory already gone; the pod's rm -rf succeeds on a missing path) is
// harmless: the finalizer goes and nothing is left.
func TestCleanup_SucceedsAndIsIdempotent(t *testing.T) {
	snap := deletingSnapOn("worker-1")
	r, c, _ := newLifecycleReconciler(t, interceptor.Funcs{}, snap)
	reconcile(t, r, "snap1", "default")
	pods := cleanupPods(t, c)
	pods[0].Status.Phase = corev1.PodSucceeded
	if err := c.Status().Update(context.Background(), &pods[0]); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "snap1", "default")
	var s snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &s); !apierrors.IsNotFound(err) {
		t.Fatalf("snapshot still present after cleanup: finalizers=%v err=%v", s.Finalizers, err)
	}
	if len(cleanupPods(t, c)) != 0 {
		t.Fatal("cleanup pod left behind")
	}
}

// A capture node no longer in the cluster does not hold the deletion; a
// Warning names the directory in case the node returns.
func TestCleanup_NodeGone_WarnsAndCompletes(t *testing.T) {
	snap := deletingSnapOn("gone-node")
	r, c, rec := newLifecycleReconciler(t, interceptor.Funcs{}, snap)
	reconcile(t, r, "snap1", "default")
	var s snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &s); !apierrors.IsNotFound(err) {
		t.Fatalf("deletion held for a node that is not in the cluster: %v", err)
	}
	if ev := drainEvents(rec); !strings.Contains(ev, ReasonNodeCleanupSkipped) || !strings.Contains(ev, "default_snap1") {
		t.Errorf("events = %q", ev)
	}
}

// A capture recorded on one node whose guest has since moved: the old node
// stays on the cleanup list when the capture is sent on the new one.
func TestCapture_GuestMovedSinceRecordedNode_KeepsOldNode(t *testing.T) {
	snap := makeLocalSnap("snap1", "default", "g1")
	snap.Finalizers = []string{HostPathFinalizer}
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhasePending
	snap.Status.NodeName = "worker-2"
	snap.Status.MemorySnapshot = &snapshotv1alpha1.MemorySnapshotRef{Handle: HostPathBaseDir + "default_snap1"}
	r, c, _ := newLifecycleReconciler(t, interceptor.Funcs{}, snap, makeGuest("default", "g1"), makeLauncherPod("default", "g1", "worker-1"))
	reconcile(t, r, "snap1", "default")
	s := get(t, c, "snap1", "default")
	if s.Status.NodeName != "worker-1" || len(s.Status.NodeCopies) != 1 || s.Status.NodeCopies[0] != "worker-2" {
		t.Fatalf("node=%q nodeCopies=%v; worker-2 may hold a capture and must stay on the cleanup list", s.Status.NodeName, s.Status.NodeCopies)
	}
}

func reqFor(name, ns string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Name: name, Namespace: ns}}
}
