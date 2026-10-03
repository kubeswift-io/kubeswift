package swiftkernel

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

const testControllerNS = "kubeswift-system"

func kernelNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"kubeswift.io/kernel-node": "true"}}}
}

// deletingKernel is a SwiftKernel being deleted, pulled to the given nodes.
func deletingKernel(nodes ...string) *kernelv1alpha1.SwiftKernel {
	now := metav1.Now()
	sk := &kernelv1alpha1.SwiftKernel{
		ObjectMeta: metav1.ObjectMeta{
			Name: "k", Namespace: "team-a", UID: "sk-uid",
			Finalizers: []string{NodeCleanupFinalizer}, DeletionTimestamp: &now,
		},
		Spec: kernelv1alpha1.SwiftKernelSpec{OCIRef: kernelv1alpha1.OCIRef{Image: "registry.example.com/kernels/k:1"}},
	}
	for _, n := range nodes {
		sk.Status.NodeStatuses = append(sk.Status.NodeStatuses, kernelv1alpha1.NodeKernelStatus{NodeName: n, Phase: kernelv1alpha1.SwiftKernelPhaseReady})
	}
	return sk
}

func newCleanupReconciler(objs ...client.Object) (*SwiftKernelReconciler, client.Client, *record.FakeRecorder) {
	c := fake.NewClientBuilder().WithScheme(kscheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&kernelv1alpha1.SwiftKernel{}).Build()
	rec := record.NewFakeRecorder(10)
	return &SwiftKernelReconciler{Client: c, Scheme: kscheme.Scheme, ControllerNamespace: testControllerNS, Recorder: rec}, c, rec
}

func reconcileKernel(t *testing.T, r *SwiftKernelReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: "k"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func cleanupPods(t *testing.T, c client.Client) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(testControllerNS), client.MatchingLabels{cleanupRoleLabel: cleanupRoleValue}); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

func setPodPhase(t *testing.T, c client.Client, p *corev1.Pod, phase corev1.PodPhase) {
	t.Helper()
	p.Status.Phase = phase
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func kernelGone(t *testing.T, c client.Client) bool {
	t.Helper()
	var sk kernelv1alpha1.SwiftKernel
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "k"}, &sk)
	return client.IgnoreNotFound(err) == nil && err != nil
}

// A live kernel gets the finalizer, a Failed one too: it may have files on the
// nodes it did pull to.
func TestReconcile_AddsNodeCleanupFinalizer(t *testing.T) {
	for _, phase := range []kernelv1alpha1.SwiftKernelPhase{"", kernelv1alpha1.SwiftKernelPhaseFailed} {
		sk := &kernelv1alpha1.SwiftKernel{
			ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "team-a"},
			Spec:       kernelv1alpha1.SwiftKernelSpec{OCIRef: kernelv1alpha1.OCIRef{Image: "registry.example.com/kernels/k:1"}},
			Status:     kernelv1alpha1.SwiftKernelStatus{Phase: phase},
		}
		r, c, _ := newCleanupReconciler(sk, kernelNode("node-a"))
		reconcileKernel(t, r)
		var got kernelv1alpha1.SwiftKernel
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "k"}, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Finalizers) != 1 || got.Finalizers[0] != NodeCleanupFinalizer {
			t.Errorf("phase %q: finalizers = %v, want [%s]", phase, got.Finalizers, NodeCleanupFinalizer)
		}
	}
}

// Deleting a kernel used to leave its files on every node it was pulled to.
// Now one pod per node removes /var/lib/kubeswift/kernels/<ns>/<name>, and the
// kernel goes once all have succeeded.
func TestReconcile_DeletionCleansEveryPulledNode(t *testing.T) {
	r, c, rec := newCleanupReconciler(deletingKernel("node-a", "node-b"), kernelNode("node-a"), kernelNode("node-b"))
	if res := reconcileKernel(t, r); res.RequeueAfter == 0 {
		t.Error("cleanup in flight must requeue")
	}
	pods := cleanupPods(t, c)
	if len(pods) != 2 {
		t.Fatalf("cleanup pods = %d, want 2", len(pods))
	}
	nodes := map[string]bool{}
	for _, p := range pods {
		nodes[p.Spec.NodeName] = true
		ct := p.Spec.Containers[0]
		if strings.Join(ct.Command, " ") != "rm -rf --" || len(ct.Args) != 1 || ct.Args[0] != "/kernels/team-a/k" {
			t.Errorf("pod %s runs %v %v, want rm -rf -- /kernels/team-a/k", p.Name, ct.Command, ct.Args)
		}
		if hp := p.Spec.Volumes[0].HostPath; hp == nil || hp.Path != kernelHostBasePath {
			t.Errorf("pod %s mounts %+v, want the kernels base %s", p.Name, hp, kernelHostBasePath)
		}
		if p.Namespace != testControllerNS {
			t.Errorf("pod %s in %s, want the controller's namespace", p.Name, p.Namespace)
		}
	}
	if !nodes["node-a"] || !nodes["node-b"] {
		t.Errorf("cleanup pods on %v, want node-a and node-b", nodes)
	}

	// One node done: the kernel waits for the other, and keeps both pods.
	setPodPhase(t, c, &pods[0], corev1.PodSucceeded)
	reconcileKernel(t, r)
	if kernelGone(t, c) || len(cleanupPods(t, c)) != 2 {
		t.Fatal("kernel released, or a pod deleted, before every node was clean")
	}
	setPodPhase(t, c, &pods[1], corev1.PodSucceeded)
	reconcileKernel(t, r)
	if !kernelGone(t, c) {
		t.Error("kernel still held after every node was cleaned")
	}
	if n := len(cleanupPods(t, c)); n != 0 {
		t.Errorf("%d cleanup pods left behind", n)
	}
	if len(rec.Events) != 0 {
		t.Errorf("unexpected event: %s", <-rec.Events)
	}
}

// A node that fails its cleanup, or whose pod cannot run, must not hold the
// kernel (and a namespace being deleted) for good: it is given up with a
// Warning event naming what was left. A node that no longer exists has
// nothing to clean.
func TestReconcile_DeletionGivesUpOnANodeItCannotClean(t *testing.T) {
	sk := deletingKernel("node-a", "node-b", "node-gone")
	r, c, rec := newCleanupReconciler(sk, kernelNode("node-a"), kernelNode("node-b"))
	reconcileKernel(t, r)
	pods := cleanupPods(t, c)
	if len(pods) != 2 {
		t.Fatalf("cleanup pods = %d, want 2 (none for a node that no longer exists)", len(pods))
	}
	for i := range pods {
		switch pods[i].Spec.NodeName {
		case "node-a":
			setPodPhase(t, c, &pods[i], corev1.PodFailed)
		case "node-b":
			// Stuck Pending past the limit, as on a node that is down.
			pods[i].CreationTimestamp = metav1.NewTime(time.Now().Add(-cleanupPendingLimit - time.Minute))
			if err := c.Update(context.Background(), &pods[i]); err != nil {
				t.Fatal(err)
			}
			setPodPhase(t, c, &pods[i], corev1.PodPending)
		}
	}
	reconcileKernel(t, r)
	if !kernelGone(t, c) {
		t.Fatal("kernel still held by nodes it cannot clean")
	}
	select {
	case ev := <-rec.Events:
		for _, want := range []string{ReasonNodeCleanupSkipped, "/var/lib/kubeswift/kernels/team-a/k", "node-a", "node-b"} {
			if !strings.Contains(ev, want) {
				t.Errorf("event %q does not mention %q", ev, want)
			}
		}
		if strings.Contains(ev, "node-gone") {
			t.Errorf("event names a node that no longer exists: %q", ev)
		}
	default:
		t.Error("no Warning event for the nodes left uncleaned")
	}
}

// A namespace being deleted refuses new pods, and deletes its kernels: their
// cleanup still runs, in the controller's namespace.
func TestReconcile_DeletionInATerminatingNamespace(t *testing.T) {
	now := metav1.Now()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"}}}
	r, c, _ := newCleanupReconciler(ns, deletingKernel("node-a"), kernelNode("node-a"))
	reconcileKernel(t, r)
	if n := len(cleanupPods(t, c)); n != 1 {
		t.Errorf("cleanup pods = %d, want 1", n)
	}
}

// A kernel deleted while its pods ran (its finalizer removed by hand) leaves
// pods nothing owns; the next reconcile of its name deletes them, and only
// them.
func TestReconcile_DeletesOrphanCleanupPods(t *testing.T) {
	orphan := buildCleanupPod(deletingKernel(), client.ObjectKey{Namespace: testControllerNS, Name: "orphan"}, "node-a", "team-a/k")
	live := &kernelv1alpha1.SwiftKernel{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-a", UID: "other-uid"}}
	livePod := buildCleanupPod(live, client.ObjectKey{Namespace: testControllerNS, Name: "live"}, "node-a", "team-a/other")
	r, c, _ := newCleanupReconciler(orphan, live, livePod)
	reconcileKernel(t, r) // kernel "k" does not exist
	pods := cleanupPods(t, c)
	if len(pods) != 1 || pods[0].Name != "live" {
		t.Errorf("cleanup pods left = %v, want only the live kernel's", pods)
	}
}

// The directory removed is always exactly <namespace>/<name> below the base.
func TestKernelRelPath(t *testing.T) {
	ok := &kernelv1alpha1.SwiftKernel{ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "team-a"}}
	if rel, good := kernelRelPath(ok); !good || rel != "team-a/k" {
		t.Errorf("kernelRelPath = %q, %v", rel, good)
	}
	for _, bad := range []kernelv1alpha1.SwiftKernel{
		{ObjectMeta: metav1.ObjectMeta{Name: "", Namespace: "team-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "..", Namespace: "team-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: ".."}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a/b", Namespace: "team-a"}},
	} {
		if rel, good := kernelRelPath(&bad); good {
			t.Errorf("namespace %q name %q: accepted %q", bad.Namespace, bad.Name, rel)
		}
	}
}

// launcherMounting is a launcher pod in team-a that mounts dir, as a
// kernel-boot guest's or a sandbox's does.
func launcherMounting(name, dir string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name:         "kernel-artifacts",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: dir}},
		}}},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// Like a PVC in use: a running launcher mounts the kernel and its hypervisor
// reads the kernel again when the guest reboots, so the files stay until the
// last such pod is gone. A pod mounting another kernel does not count.
func TestReconcile_DeletionWaitsWhileTheKernelIsInUse(t *testing.T) {
	launcher := launcherMounting("guest-a", "/var/lib/kubeswift/kernels/team-a/k/", corev1.PodRunning)
	other := launcherMounting("guest-b", "/var/lib/kubeswift/kernels/team-a/other", corev1.PodRunning)
	r, c, rec := newCleanupReconciler(deletingKernel("node-a"), kernelNode("node-a"), launcher, other)
	if res := reconcileKernel(t, r); res.RequeueAfter != inUseRequeue {
		t.Errorf("requeue = %s, want %s while in use", res.RequeueAfter, inUseRequeue)
	}
	if n := len(cleanupPods(t, c)); n != 0 || kernelGone(t, c) {
		t.Fatalf("cleaned (pods=%d, gone=%v) while a running guest uses the kernel", n, kernelGone(t, c))
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, ReasonKernelInUse) || !strings.Contains(ev, "guest-a") || strings.Contains(ev, "guest-b") {
			t.Errorf("event = %q, want %s naming guest-a only", ev, ReasonKernelInUse)
		}
	default:
		t.Error("no event saying why the deletion waits")
	}

	setPodPhase(t, c, launcher, corev1.PodSucceeded)
	reconcileKernel(t, r)
	if n := len(cleanupPods(t, c)); n != 1 {
		t.Errorf("cleanup pods = %d after the guest stopped, want 1", n)
	}
}
