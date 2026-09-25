package swiftsnapshot

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

const controllerNS = "kubeswift-system"

// newReconcilerIn builds a reconciler whose controller namespace is
// controllerNS and whose client refuses every create in each of terminating,
// the way the apiserver does for a namespace being deleted.
func newReconcilerIn(t *testing.T, terminating []string, objs ...client.Object) (*SwiftSnapshotReconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			for _, ns := range terminating {
				if obj.GetNamespace() == ns {
					return namespaceTerminatingErr(ns, obj)
				}
			}
			return cl.Create(ctx, obj, opts...)
		}}).
		Build()
	rec := record.NewFakeRecorder(10)
	return &SwiftSnapshotReconciler{Client: c, Scheme: scheme, ControllerNamespace: controllerNS, Recorder: rec}, c, rec
}

func namespaceTerminatingErr(namespace string, obj client.Object) error {
	gr := schema.GroupResource{Resource: "pods"}
	if _, ok := obj.(*batchv1.Job); ok {
		gr = schema.GroupResource{Group: "batch", Resource: "jobs"}
	}
	err := apierrors.NewForbidden(gr, obj.GetName(),
		errors.New("unable to create new content in namespace "+namespace+" because it is being terminated"))
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
		Type: corev1.NamespaceTerminatingCause, Message: "namespace " + namespace + " is being terminated",
	})
	return err
}

// deletingLocalSnap is a captured local snapshot being deleted.
func deletingLocalSnap(ns string) *snapshotv1alpha1.SwiftSnapshot {
	snap := makeLocalSnap("snap1", ns, "g1")
	now := metav1.Now()
	snap.DeletionTimestamp = &now
	snap.Finalizers = []string{HostPathFinalizer}
	snap.Status.NodeName = "worker-1"
	return snap
}

func getPod(t *testing.T, c client.Client, ns, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &pod)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &pod
}

func snapGone(t *testing.T, c client.Client, snap *snapshotv1alpha1.SwiftSnapshot) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKeyFromObject(snap), &snapshotv1alpha1.SwiftSnapshot{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return apierrors.IsNotFound(err)
}

func succeed(t *testing.T, c client.Client, pod *corev1.Pod) {
	t.Helper()
	if pod == nil {
		t.Fatal("no cleanup pod to succeed")
	}
	pod.Status.Phase = corev1.PodSucceeded
	if err := c.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupPod_RunsInTheControllerNamespace(t *testing.T) {
	snap := deletingLocalSnap("team-a")
	r, c, _ := newReconcilerIn(t, nil, snap)

	if done, err := r.handleDeletion(context.Background(), snap); err != nil || done {
		t.Fatalf("first pass should create the cleanup pod; done=%v err=%v", done, err)
	}
	if getPod(t, c, "team-a", cleanupPodName(snap)) != nil || getPod(t, c, "team-a", legacyCleanupPodName(snap)) != nil {
		t.Error("the cleanup pod was created in the snapshot's namespace")
	}
	pod := getPod(t, c, controllerNS, cleanupPodName(snap))
	if pod == nil {
		t.Fatal("no cleanup pod in the controller's namespace")
	}
	if pod.Spec.NodeName != "worker-1" {
		t.Errorf("node = %q, want worker-1", pod.Spec.NodeName)
	}
	want := map[string]string{
		cleanupRoleLabel:       cleanupRoleValue,
		snapshotNameLabel:      "snap1",
		snapshotNamespaceLabel: "team-a",
		snapshotUIDLabel:       fakeUID,
	}
	for k, v := range want {
		if pod.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if len(pod.OwnerReferences) != 0 {
		t.Errorf("an owner reference cannot cross namespaces; got %v", pod.OwnerReferences)
	}
	if args := strings.Join(pod.Spec.Containers[0].Args, " "); args != HostPathBaseMount+"/team-a_snap1" {
		t.Errorf("args = %q", args)
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.Privileged != nil && *sc.Privileged {
		t.Errorf("the cleanup pod needs a hostPath mount, not privilege; got %+v", sc)
	}
	if !hasFin(t, c, snap, HostPathFinalizer) {
		t.Error("finalizer must stay while the cleanup pod runs")
	}
}

func TestCleanupPod_SucceededRemovesTheFinalizerAndThePod(t *testing.T) {
	snap := deletingLocalSnap("team-a")
	r, c, _ := newReconcilerIn(t, nil, snap)
	ctx := context.Background()

	if _, err := r.handleDeletion(ctx, snap); err != nil {
		t.Fatal(err)
	}
	succeed(t, c, getPod(t, c, controllerNS, cleanupPodName(snap)))
	if done, err := r.handleDeletion(ctx, snap); err != nil || !done {
		t.Fatalf("a succeeded cleanup should finish the deletion; done=%v err=%v", done, err)
	}
	if !snapGone(t, c, snap) {
		t.Error("the finalizer was not removed")
	}
	if getPod(t, c, controllerNS, cleanupPodName(snap)) != nil {
		t.Error("the succeeded pod was left in the controller's namespace; nothing else deletes it")
	}
}

// Deleting the namespace is how the lab deleted its snapshots, and a namespace
// being deleted refuses new pods: the snapshot held the namespace forever.
func TestCleanupPod_TerminatingNamespaceStillCleansUp(t *testing.T) {
	snap := deletingLocalSnap("team-a")
	r, c, _ := newReconcilerIn(t, []string{"team-a"}, snap)
	ctx := context.Background()

	if _, err := r.handleDeletion(ctx, snap); err != nil {
		t.Fatalf("a terminating snapshot namespace must not stop the cleanup: %v", err)
	}
	pod := getPod(t, c, controllerNS, cleanupPodName(snap))
	if pod == nil {
		t.Fatal("no cleanup pod in the controller's namespace")
	}
	succeed(t, c, pod)
	if done, err := r.handleDeletion(ctx, snap); err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if !snapGone(t, c, snap) {
		t.Error("the snapshot still holds its namespace")
	}
}

// A cleanup pod the previous version ran in the snapshot's namespace and that
// finished did the work: the deletion completes without a second pod.
func TestCleanupPod_LegacySucceededPodIsHonoured(t *testing.T) {
	snap := deletingLocalSnap("team-a")
	legacy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: legacyCleanupPodName(snap), Namespace: "team-a",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(snap, swiftSnapshotGVK)},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	r, c, _ := newReconcilerIn(t, nil, snap, legacy)

	if done, err := r.handleDeletion(context.Background(), snap); err != nil || !done {
		t.Fatalf("a succeeded legacy pod should finish the deletion; done=%v err=%v", done, err)
	}
	if !snapGone(t, c, snap) {
		t.Error("the finalizer was not removed")
	}
	if getPod(t, c, "team-a", legacy.Name) != nil {
		t.Error("the legacy pod was not deleted")
	}
	if getPod(t, c, controllerNS, cleanupPodName(snap)) != nil {
		t.Error("a second cleanup pod was created for work already done")
	}
}

// An unfinished legacy pod is replaced, not raced; a pod of that name the
// controller did not make for this snapshot is neither honoured nor deleted.
func TestCleanupPod_LegacyUnfinishedOrForeignPod(t *testing.T) {
	for _, tc := range []struct {
		name        string
		owned       bool
		phase       corev1.PodPhase
		wantDeleted bool
	}{
		{"pending legacy pod", true, corev1.PodPending, true},
		{"failed legacy pod", true, corev1.PodFailed, true},
		{"succeeded pod not owned by the snapshot", false, corev1.PodSucceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := deletingLocalSnap("team-a")
			legacy := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: legacyCleanupPodName(snap), Namespace: "team-a"},
				Status:     corev1.PodStatus{Phase: tc.phase},
			}
			if tc.owned {
				legacy.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(snap, swiftSnapshotGVK)}
			}
			r, c, _ := newReconcilerIn(t, nil, snap, legacy)

			if done, err := r.handleDeletion(context.Background(), snap); err != nil || done {
				t.Fatalf("done=%v err=%v; want a new cleanup pod", done, err)
			}
			if getPod(t, c, controllerNS, cleanupPodName(snap)) == nil {
				t.Error("no cleanup pod in the controller's namespace")
			}
			if deleted := getPod(t, c, "team-a", legacy.Name) == nil; deleted != tc.wantDeleted {
				t.Errorf("legacy pod deleted = %v, want %v", deleted, tc.wantDeleted)
			}
		})
	}
}

func TestCleanupPod_NoControllerNamespaceFallsBackToTheSnapshotNamespace(t *testing.T) {
	snap := deletingLocalSnap("team-a")
	r, c, _ := newReconcilerIn(t, nil, snap)
	r.ControllerNamespace = ""

	if _, err := r.handleDeletion(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	if getPod(t, c, "team-a", cleanupPodName(snap)) == nil {
		t.Error("no cleanup pod in the snapshot's namespace")
	}

	// With nowhere else to go, a terminating namespace is an error that says
	// what is missing.
	snap2 := deletingLocalSnap("team-b")
	r2, _, _ := newReconcilerIn(t, []string{"team-b"}, snap2)
	r2.ControllerNamespace = ""
	if _, err := r2.handleDeletion(context.Background(), snap2); err == nil || !strings.Contains(err.Error(), "POD_NAMESPACE") {
		t.Errorf("err = %v; want one naming POD_NAMESPACE", err)
	}
}

// The capture copy of an s3 snapshot is cleaned from the controller's
// namespace too; the purge Job, which needs the tenant's credentials, cannot
// run anywhere but the terminating namespace, so the finalizer goes with a
// warning naming what was left.
func TestS3Deletion_TerminatingNamespaceSkipsThePurgeWithAWarning(t *testing.T) {
	snap := s3SnapReady("snap1", "team-a")
	snap.Status.NodeName = "worker-1"
	r, c, rec := newReconcilerIn(t, []string{"team-a"}, snap)
	r.SnapshotS3Image = "img"
	ctx := context.Background()

	if done, err := r.handleS3Deletion(ctx, snap); err != nil || done {
		t.Fatalf("first pass should start the node cleanup; done=%v err=%v", done, err)
	}
	pod := getPod(t, c, controllerNS, cleanupPodName(snap))
	if pod == nil {
		t.Fatal("capture-copy cleanup pod not created in the controller's namespace")
	}
	succeed(t, c, pod)
	if done, err := r.handleS3Deletion(ctx, snap); err != nil || !done {
		t.Fatalf("a refused purge Job must not hold the namespace; done=%v err=%v", done, err)
	}
	if hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("finalizer should be dropped")
	}
	select {
	case ev := <-rec.Events:
		if !strings.HasPrefix(ev, corev1.EventTypeWarning+" "+ReasonPurgeSkipped) || !strings.Contains(ev, s3Location(snap)) {
			t.Errorf("event = %q; want a %s warning naming %s", ev, ReasonPurgeSkipped, s3Location(snap))
		}
	default:
		t.Error("no warning event")
	}
}

func TestOCIDeletion_TerminatingNamespaceSkipsThePurgeWithAWarning(t *testing.T) {
	snap := ociSnapPushed()
	r, c, rec := newReconcilerIn(t, []string{snap.Namespace}, snap)
	r.SnapshotORASImage = "img"

	if done, err := r.handleOCIDeletion(context.Background(), snap); err != nil || !done {
		t.Fatalf("a refused delete Job must not hold the namespace; done=%v err=%v", done, err)
	}
	if hasFin(t, c, snap, OCIArtifactFinalizer) {
		t.Error("finalizer should be dropped")
	}
	select {
	case ev := <-rec.Events:
		for _, ref := range []string{snap.Status.OCI.Reference, snap.Status.OCI.Disk.Reference, snap.Status.OCI.DataDisks[0].Reference} {
			if !strings.Contains(ev, ref) {
				t.Errorf("event %q does not name %s", ev, ref)
			}
		}
	default:
		t.Error("no warning event")
	}
}

// A purge Job that is merely running in a live namespace is waited for, as
// before: only a refused creation drops the finalizer.
func TestS3Deletion_LiveNamespaceStillWaitsForThePurge(t *testing.T) {
	snap := s3SnapReady("snap1", "team-a")
	r, c, rec := newReconcilerIn(t, nil, snap, deleteJobWith(snap, ""))
	r.SnapshotS3Image = "img"
	if done, err := r.handleS3Deletion(context.Background(), snap); err != nil || done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if !hasFin(t, c, snap, S3ObjectFinalizer) {
		t.Error("finalizer must stay while the purge runs")
	}
	if len(rec.Events) != 0 {
		t.Errorf("unexpected event %q", <-rec.Events)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: s3DeleteJobName(snap)}, &job); err != nil {
		t.Errorf("purge Job gone: %v", err)
	}
}

// Nothing owns a cleanup pod, so one left when a finalizer is removed by hand
// is deleted once its snapshot is gone; a live snapshot's pod is kept.
func TestReconcile_GoneSnapshotDeletesItsCleanupPod(t *testing.T) {
	live := deletingLocalSnap("team-a")
	live.Name, live.UID = "live", "live-uid"
	gone := deletingLocalSnap("team-a")
	gone.UID = "gone-uid"
	podFor := func(snap *snapshotv1alpha1.SwiftSnapshot) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: cleanupPodName(snap), Namespace: controllerNS,
			Labels: map[string]string{
				cleanupRoleLabel: cleanupRoleValue, snapshotNameLabel: snap.Name,
				snapshotNamespaceLabel: snap.Namespace, snapshotUIDLabel: string(snap.UID),
			},
		}}
	}
	r, c, _ := newReconcilerIn(t, nil, live, podFor(live), podFor(gone))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gone)}); err != nil {
		t.Fatal(err)
	}
	if getPod(t, c, controllerNS, cleanupPodName(gone)) != nil {
		t.Error("the gone snapshot's cleanup pod was left behind")
	}
	if getPod(t, c, controllerNS, cleanupPodName(live)) == nil {
		t.Error("a live snapshot's cleanup pod was deleted")
	}

	// The Pod watch maps a cleanup pod back to its snapshot, so a pod left
	// while the controller was down reaches the same path.
	reqs := r.podToSnapshots(context.Background(), podFor(gone))
	if len(reqs) != 1 || reqs[0].NamespacedName != client.ObjectKeyFromObject(gone) {
		t.Errorf("cleanup pod maps to %v, want %s", reqs, client.ObjectKeyFromObject(gone))
	}
}

// Same-named snapshots of different namespaces meet in the controller's
// namespace; a name at the API's limit must still give a valid pod name.
func TestCleanupPodName_UniqueAndBounded(t *testing.T) {
	a, b := makeLocalSnap("db", "team-a", "g"), makeLocalSnap("db", "team-b", "g")
	if cleanupPodName(a) == cleanupPodName(b) {
		t.Error("same-named snapshots of two namespaces share a cleanup pod name")
	}
	if cleanupPodName(a) != cleanupPodName(a.DeepCopy()) {
		t.Error("the name is not stable")
	}
	long := makeLocalSnap(strings.Repeat("s", 253), "team-a", "g")
	name := cleanupPodName(long)
	if errs := validation.IsDNS1123Label(name); len(errs) != 0 {
		t.Errorf("cleanup pod name %q (%d chars): %v", name, len(name), errs)
	}
}
