package swiftguest

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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
	"github.com/kubeswift-io/kubeswift/internal/sharedbase"
)

// guestWithDisk is a shared-base guest whose disk was built on node, carrying
// the finalizer as every such guest does.
func guestWithDisk(node string) []client.Object {
	objs := sharedBaseFixtures()
	g := objs[0].(*swiftv1alpha1.SwiftGuest)
	g.Finalizers = []string{SharedBaseDiskFinalizer}
	g.Status.SharedBaseDisk = &swiftv1alpha1.SharedBaseDiskStatus{
		Node: node, BaseKey: sharedbase.BaseKey(sbImageUID, sbPVCUID), Created: true,
	}
	return objs
}

func labelledLauncher(node string) *corev1.Pod {
	p := runningLauncher(node)
	p.Labels = map[string]string{guestPodLabelKey: testGuestName}
	return p
}

func materialiseJobFixture(done bool) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: testGuestName + materialiseJobSuffix, Namespace: "ns"}}
	if done {
		j.Status.Succeeded = 1
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return j
}

func podOfJob(namespace, job, node string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: job + "-x", Namespace: namespace,
			Labels: map[string]string{"batch.kubernetes.io/job-name": job},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func releaseClient(objs ...client.Object) client.WithWatch {
	return guestClientBuilder(objs...).WithStatusSubresource(&batchv1.Job{}).Build()
}

// deleteGuest deletes the test guest; its finalizers keep it, deleting.
func deleteGuest(t *testing.T, c client.Client) {
	t.Helper()
	g := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: testGuestName}}
	if err := c.Delete(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}

// reconcileGone runs one Reconcile and returns the guest as stored, or nil once
// it is gone.
func reconcileGone(t *testing.T, r *SwiftGuestReconciler) (*swiftv1alpha1.SwiftGuest, ctrl.Result, error) {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: testGuestKey})
	var got swiftv1alpha1.SwiftGuest
	if getErr := r.Get(context.Background(), testGuestKey, &got); apierrors.IsNotFound(getErr) {
		return nil, res, err
	} else if getErr != nil {
		t.Fatal(getErr)
	}
	return &got, res, err
}

func releaseJobIn(t *testing.T, c client.Client, namespace string) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: releaseJobPrefix + string(sbGuestUID)}, &job)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &job
}

func completeJob(t *testing.T, c client.Client, job *batchv1.Job) {
	t.Helper()
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

// The finalizer must exist before anything of the guest's can exist on a node,
// or a guest deleted in between would leave its disk behind for good.
func TestSharedBaseFinalizer_AddedBeforeTheMaterialiseJob(t *testing.T) {
	sawJob := false
	c := guestClientBuilder(append(sharedBaseFixtures(), node("worker-1"))...).
		WithStatusSubresource(&batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if job, ok := obj.(*batchv1.Job); ok && strings.HasSuffix(job.Name, materialiseJobSuffix) {
				sawJob = true
				var g swiftv1alpha1.SwiftGuest
				if err := cl.Get(ctx, testGuestKey, &g); err != nil {
					return err
				}
				if !controllerutil.ContainsFinalizer(&g, SharedBaseDiskFinalizer) {
					t.Error("the materialise Job was created before the guest carried the finalizer")
				}
			}
			return cl.Create(ctx, obj, opts...)
		}}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if !sawJob {
		t.Fatal("no materialise Job was created; the test proved nothing")
	}
	if !controllerutil.ContainsFinalizer(got, SharedBaseDiskFinalizer) {
		t.Error("the guest does not carry the finalizer")
	}
}

// A guest built before the finalizer existed, and stopped since, never passes
// through ensureSharedBaseDisk again. It still has a disk to release.
func TestSharedBaseFinalizer_AddedToAStoppedGuestThatHasADisk(t *testing.T) {
	objs := guestWithDisk("worker-1")
	g := objs[0].(*swiftv1alpha1.SwiftGuest)
	g.Finalizers = nil
	g.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	r := &SwiftGuestReconciler{Client: releaseClient(append(objs, node("worker-1"))...), Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(got, SharedBaseDiskFinalizer) {
		t.Error("a stopped guest with a disk was left without the finalizer that releases it")
	}
}

// THE lifecycle of a deletion: the VM is stopped, the disk is released on its
// node, and only then does the guest go.
func TestReconcile_SharedBaseGuest_Deletion(t *testing.T) {
	c := releaseClient(append(guestWithDisk("worker-1"),
		node("worker-1"), labelledLauncher("worker-1"),
		materialiseJobFixture(true),
		podOfJob("ns", testGuestName+materialiseJobSuffix, "worker-1", corev1.PodSucceeded))...)
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	// 1. The launcher is deleted, and nothing is released while it may still
	//    hold the disk open.
	got, _, err := reconcileGone(t, r)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if n := len(launchers(t, c)); n != 0 {
		t.Fatalf("%d launcher(s) left running for a deleted guest", n)
	}
	if releaseJobIn(t, c, "ns") != nil {
		t.Fatal("the disk was released in the same pass that stopped its launcher")
	}
	if cond := guestCondition(t, got, ConditionStorageReady); cond.Reason != reasonBaseDiskReleasing || !strings.Contains(cond.Message, "waiting for pod") {
		t.Errorf("StorageReady = %s %q; want it to say what the release waits for", cond.Reason, cond.Message)
	}

	// 2. The release Job, on the disk's node; the materialise Job is gone.
	got, _, err = reconcileGone(t, r)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	job := releaseJobIn(t, c, "ns")
	if job == nil {
		t.Fatal("no release Job")
	}
	if job.Spec.Template.Spec.NodeName != "worker-1" {
		t.Errorf("release Job on %q; the disk is on worker-1", job.Spec.Template.Spec.NodeName)
	}
	var mat batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: testGuestName + materialiseJobSuffix}, &mat); !apierrors.IsNotFound(err) {
		t.Errorf("the materialise Job was not deleted (err = %v)", err)
	}
	if got == nil || !controllerutil.ContainsFinalizer(got, SharedBaseDiskFinalizer) {
		t.Fatal("the guest let go before its disk was released")
	}
	if cond := guestCondition(t, got, ConditionStorageReady); !strings.Contains(cond.Message, "worker-1") || !strings.Contains(cond.Message, job.Name) {
		t.Errorf("StorageReady %q should name the node and the Job", cond.Message)
	}

	// 3. Still running: nothing changes, and no second Job.
	if _, _, err := reconcileGone(t, r); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Errorf("%d Jobs; want the one release Job", len(jobs.Items))
	}

	// 4. Released: the guest goes, and takes the Job with it.
	completeJob(t, c, job)
	got, _, err = reconcileGone(t, r)
	if err != nil {
		t.Fatalf("reconcile 4: %v", err)
	}
	if got != nil {
		t.Fatalf("the guest outlived the release; finalizers %v", got.Finalizers)
	}
	if releaseJobIn(t, c, "ns") != nil {
		t.Error("the finished release Job was left behind")
	}
	if n := len(launchers(t, c)); n != 0 {
		t.Errorf("a launcher was created for a guest being deleted (%d)", n)
	}
}

func TestReleaseJob_Shape(t *testing.T) {
	g := sharedBaseFixtures()[0].(*swiftv1alpha1.SwiftGuest)
	job := releaseJob(g, "worker-1", "ns")
	pod := job.Spec.Template.Spec

	if len(job.OwnerReferences) != 0 {
		t.Error("the release Job has an owner reference; a foreground deletion of the guest would collect it mid-release")
	}
	if _, ok := job.Spec.Template.Labels[guestPodLabelKey]; ok {
		t.Error("the release pod carries the guest label and would be mistaken for a launcher")
	}
	if pod.NodeName != "worker-1" {
		t.Errorf("NodeName = %q", pod.NodeName)
	}
	if len(pod.Tolerations) != 1 || pod.Tolerations[0].Operator != corev1.TolerationOpExists || pod.Tolerations[0].Key != "" {
		t.Errorf("tolerations %+v; a taint must not keep a deleted guest's disk allocated", pod.Tolerations)
	}
	if b := job.Spec.BackoffLimit; b == nil || *b < 1 {
		t.Error("a release is idempotent and should be retried in place")
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Error("no TTL: a Job the controller never deletes would stay forever")
	}
	if a := pod.AutomountServiceAccountToken; a == nil || *a {
		t.Error("the release pod needs no API access and must not get a token")
	}
	ctr := pod.Containers[0]
	want := []string{
		"--mode=release",
		"--root=" + sharedbase.StateRoot,
		"--pool=" + sharedbase.Pool,
		"--guest-key=" + sharedbase.GuestKey("ns", testGuestName, sbGuestUID),
		"--device=" + sharedbase.DeviceName(sbGuestUID),
	}
	if strings.Join(ctr.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args %v, want %v", ctr.Args, want)
	}
	if sc := ctr.SecurityContext; sc == nil || sc.Privileged == nil || !*sc.Privileged {
		t.Error("the release pod must be privileged to reach device-mapper")
	}
	mounts := map[string]string{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	if mounts["/dev"] == "" || mounts[sharedbase.StateRoot] == "" {
		t.Errorf("mounts %v; the command needs the host /dev and %s", mounts, sharedbase.StateRoot)
	}
}

// A failed release is said on the guest, with what to do, and not retried in a
// loop: the Job already retried in place.
func TestReleaseSharedBaseDisk_FailureIsSurfacedAndNotRetried(t *testing.T) {
	g := sharedBaseFixtures()[0].(*swiftv1alpha1.SwiftGuest)
	failed := releaseJob(g, "worker-1", "ns")
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	pod := podOfJob("ns", failed.Name, "worker-1", corev1.PodFailed)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Message: "pool kubeswift-pool is read-only (its metadata device is full, or it needs a check)",
	}}}}
	c := releaseClient(append(guestWithDisk("worker-1"), node("worker-1"), failed, pod)...)
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	got, _, err := reconcileGone(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !controllerutil.ContainsFinalizer(got, SharedBaseDiskFinalizer) {
		t.Fatal("the guest let go although its disk was not released")
	}
	cond := guestCondition(t, got, ConditionStorageReady)
	if cond.Reason != reasonBaseDiskReleaseFailed || !strings.Contains(cond.Message, "read-only") ||
		!strings.Contains(cond.Message, "Delete Job") || !strings.Contains(cond.Message, SharedBaseDiskFinalizer) {
		t.Errorf("StorageReady = %s %q; want the node's error and both ways forward", cond.Reason, cond.Message)
	}
	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Errorf("%d Jobs after a failure; a failed release must not be replaced automatically", len(jobs.Items))
	}
}

// A guest whose materialise pod never bound to a node has nothing on any node.
func TestReleaseSharedBaseDisk_NeverPlacedLetsGo(t *testing.T) {
	objs := sharedBaseFixtures()
	objs[0].(*swiftv1alpha1.SwiftGuest).Finalizers = []string{SharedBaseDiskFinalizer}
	// The real client refuses a Get with no name, where the fake answers
	// NotFound — which would let this pass through the node-gone path by
	// accident, while a real guest retried forever.
	c := guestClientBuilder(append(objs, materialiseJobFixture(false))...).
		WithStatusSubresource(&batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "" {
				return errors.New("resource name may not be empty")
			}
			return cl.Get(ctx, key, obj, opts...)
		}}).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	got, _, err := reconcileGone(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("a guest that never reached a node is held (finalizers %v)", got.Finalizers)
	}
	if releaseJobIn(t, c, "ns") != nil {
		t.Error("a release Job was run for a guest that never reached a node")
	}
}

// The node is recorded on the pass after the materialise pod binds. A guest
// deleted in between still has a disk there, and the pod is how to find it.
func TestReleaseSharedBaseDisk_FindsAnUnrecordedDiskThroughTheMaterialisePod(t *testing.T) {
	objs := sharedBaseFixtures()
	objs[0].(*swiftv1alpha1.SwiftGuest).Finalizers = []string{SharedBaseDiskFinalizer}
	matPod := podOfJob("ns", testGuestName+materialiseJobSuffix, "worker-2", corev1.PodRunning)
	c := releaseClient(append(objs, node("worker-2"), materialiseJobFixture(false), matPod)...)
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	got, _, err := reconcileGone(t, r)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if sb := got.Status.SharedBaseDisk; sb == nil || sb.Node != "worker-2" {
		t.Fatalf("status.sharedBaseDisk = %+v; the node the materialise pod ran on must be recorded before the pod goes", sb)
	}
	if releaseJobIn(t, c, "ns") != nil {
		t.Fatal("released while the materialise pod could still be writing the disk")
	}

	// The Job's deletion takes its pod.
	if err := c.Delete(context.Background(), matPod); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcileGone(t, r); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	job := releaseJobIn(t, c, "ns")
	if job == nil || job.Spec.Template.Spec.NodeName != "worker-2" {
		t.Fatalf("release Job %+v; want one on worker-2", job)
	}
}

// A node that has left the cluster took its pool with it. Holding the guest for
// it would hold it forever.
func TestReleaseSharedBaseDisk_NodeGoneLetsGo(t *testing.T) {
	c := releaseClient(guestWithDisk("worker-9")...) // no Node object
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	got, _, err := reconcileGone(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("the guest is held for a node that no longer exists (finalizers %v)", got.Finalizers)
	}
	if releaseJobIn(t, c, "ns") != nil {
		t.Error("a release Job was created for a node that does not exist")
	}
}

// refuseJobsIn fails Job creation in namespace the way the apiserver does for a
// namespace being deleted.
func refuseJobsIn(namespace string) interceptor.Funcs {
	return interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*batchv1.Job); ok && obj.GetNamespace() == namespace {
			err := apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, obj.GetName(),
				errors.New("unable to create new content in namespace "+namespace+" because it is being terminated"))
			err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{
				Type: corev1.NamespaceTerminatingCause, Message: "namespace " + namespace + " is being terminated",
			})
			return err
		}
		return cl.Create(ctx, obj, opts...)
	}}
}

// Deleting a namespace is an ordinary way to delete its guests, and a namespace
// being deleted accepts no new objects. The release must still run.
func TestReleaseSharedBaseDisk_NamespaceBeingDeletedRunsTheJobInTheControllerNamespace(t *testing.T) {
	t.Setenv(controllerNamespaceEnv, "kubeswift-system")
	c := guestClientBuilder(append(guestWithDisk("worker-1"), node("worker-1"))...).
		WithStatusSubresource(&batchv1.Job{}).WithInterceptorFuncs(refuseJobsIn("ns")).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	got, _, err := reconcileGone(t, r)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	job := releaseJobIn(t, c, "kubeswift-system")
	if job == nil {
		t.Fatal("no release Job in the controller's namespace")
	}
	if cond := guestCondition(t, got, ConditionStorageReady); !strings.Contains(cond.Message, "kubeswift-system/"+job.Name) {
		t.Errorf("StorageReady %q should say where the Job is", cond.Message)
	}

	completeJob(t, c, job)
	got, _, err = reconcileGone(t, r)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if got != nil {
		t.Fatal("the guest outlived a release that ran in the controller's namespace")
	}
	if releaseJobIn(t, c, "kubeswift-system") != nil {
		t.Error("the finished release Job was left in the controller's namespace")
	}
}

func TestReleaseSharedBaseDisk_NamespaceBeingDeletedWithNowhereElseIsAnError(t *testing.T) {
	t.Setenv(controllerNamespaceEnv, "")
	c := guestClientBuilder(append(guestWithDisk("worker-1"), node("worker-1"))...).
		WithStatusSubresource(&batchv1.Job{}).WithInterceptorFuncs(refuseJobsIn("ns")).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	got, _, err := reconcileGone(t, r)
	if err == nil || !strings.Contains(err.Error(), controllerNamespaceEnv) {
		t.Fatalf("err = %v; want one naming %s", err, controllerNamespaceEnv)
	}
	if got == nil || !controllerutil.ContainsFinalizer(got, SharedBaseDiskFinalizer) {
		t.Fatal("the guest let go without its disk being released")
	}
}

// The garbage collector will not stop a deleting guest's launcher while any
// finalizer holds the guest, so the controller does — for every guest, not
// only shared-base ones.
func TestReconcileDeletion_StopsTheVMOfAnyGuestAndStartsNoNewOne(t *testing.T) {
	g := kernelGuest()
	g.Finalizers = []string{"example.com/hold"}
	c := releaseClient(g, testGuestClass(), readyKernel(), labelledLauncher("worker-1"))
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	deleteGuest(t, c)

	for i := 0; i < 2; i++ {
		got, _, err := reconcileGone(t, r)
		if err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
		if got == nil {
			t.Fatal("the other finalizer should still hold the guest")
		}
		if n := len(launcherPods(t, c)); n != 0 {
			t.Fatalf("reconcile %d: %d launcher pod(s) for a guest being deleted", i+1, n)
		}
	}
}

// A shared-base guest held for placement returns before ensureSharedBaseDisk.
// StorageReady must still describe its disk, not the PVC it does not have.
func TestReconcile_SharedBaseGuestHeldForPlacementKeepsItsStorageMessage(t *testing.T) {
	objs := guestWithDisk("worker-1")
	g := objs[0].(*swiftv1alpha1.SwiftGuest)
	g.Spec.NodeName = "worker-2"
	const diskMsg = "shared-base root disk ks-g-x on node worker-1"
	SetStorageReadyCondition(&g.Status, true, "", diskMsg)
	r := &SwiftGuestReconciler{Client: releaseClient(append(objs, node("worker-1"), node("worker-2"))...), Scheme: scheme.Scheme}

	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if cond := guestCondition(t, got, ConditionPodScheduled); !strings.Contains(cond.Message, "has no copy of it") {
		t.Fatalf("PodScheduled %q; the guest should be held for placement", cond.Message)
	}
	if cond := guestCondition(t, got, ConditionStorageReady); cond.Message != diskMsg {
		t.Errorf("StorageReady = %q; want the shared-base disk's message kept, not PVC wording", cond.Message)
	}
}
