package swiftkernel

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

// v0141JobName is the name v0.14.1 gave the pull Job of kernel "k" on node
// "node-a": the kernel and node, nothing about the directory it pulled into.
const v0141JobName = "swiftkernel-pull-k-node-a"

// #658: a Job that pulled into another directory must not answer for the
// current one, so a change of directory has to change the name.
func TestPullJobName_ChangesWithTheDirectory(t *testing.T) {
	cur := pullJobName("k", "node-a", kernelv1alpha1.KernelLocalPath("default", "k"))
	old := pullJobName("k", "node-a", "/var/lib/kubeswift/kernels/default-k")
	if cur == old {
		t.Fatalf("the Jobs for two directories share the name %q", cur)
	}
	if cur == v0141JobName {
		t.Fatalf("the current Job has the v0.14.1 name %q", cur)
	}
	if pullJobName("k", "node-a", kernelv1alpha1.KernelLocalPath("default", "k")) != cur {
		t.Error("not deterministic")
	}
}

// The readable part of the name is ambiguous: '-' joins the kernel and node,
// and dots become '-'. The hash keeps these apart.
func TestPullJobName_DistinctWhereTheReadablePartIsNot(t *testing.T) {
	dir := func(name string) string { return kernelv1alpha1.KernelLocalPath("default", name) }
	pairs := [][2]string{
		{pullJobName("a-b", "c", dir("a-b")), pullJobName("a", "b-c", dir("a"))},
		{pullJobName("k", "n.1", dir("k")), pullJobName("k", "n-1", dir("k"))},
	}
	for _, p := range pairs {
		if p[0] == p[1] {
			t.Errorf("two kernel/node pairs share the Job name %q", p[0])
		}
	}
}

// The Job name becomes a label value on its pods, so it must fit in 63
// characters however long the kernel and node names are.
func TestPullJobName_FitsWithLongNames(t *testing.T) {
	longKernel := strings.Repeat("k", 200) + ".v1"
	longNode := strings.Repeat("n", 50) + ".eu-west-1.compute.internal"
	cases := []struct{ sk, node string }{
		{"k", "node-a"},
		{longKernel, "node-a"},
		{"k", longNode},
		{longKernel, longNode},
		{strings.Repeat("a", 27) + ".b", "node-a"},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		n := pullJobName(tc.sk, tc.node, kernelv1alpha1.KernelLocalPath("default", tc.sk))
		if len(n) > names.MaxJobName {
			t.Errorf("%q is %d characters, over %d", n, len(n), names.MaxJobName)
		}
		if errs := validation.IsDNS1123Subdomain(n); len(errs) != 0 {
			t.Errorf("%q is not a valid object name: %v", n, errs)
		}
		if errs := validation.IsValidLabelValue(n); len(errs) != 0 {
			t.Errorf("%q is not a valid label value: %v", n, errs)
		}
		if seen[n] {
			t.Errorf("%q was produced twice", n)
		}
		seen[n] = true
	}
	a := pullJobName(longKernel+"a", longNode, kernelv1alpha1.KernelLocalPath("default", longKernel+"a"))
	b := pullJobName(longKernel+"b", longNode, kernelv1alpha1.KernelLocalPath("default", longKernel+"b"))
	if a == b {
		t.Errorf("two long kernel names share the Job name %q", a)
	}
}

func TestCheckNodePullStatus_OnlyTheCurrentDirectorysJobCounts(t *testing.T) {
	sk := &kernelv1alpha1.SwiftKernel{ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "default"}}
	cases := []struct {
		name string
		job  string
		want kernelv1alpha1.SwiftKernelPhase
	}{
		{"a succeeded Job under the v0.14.1 name", v0141JobName, kernelv1alpha1.SwiftKernelPhasePending},
		{"a succeeded Job for the old directory", pullJobName("k", "node-a", "/var/lib/kubeswift/kernels/default-k"), kernelv1alpha1.SwiftKernelPhasePending},
		{"a succeeded Job for the current directory", pullJobName("k", "node-a", kernelv1alpha1.KernelLocalPath("default", "k")), kernelv1alpha1.SwiftKernelPhaseReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: tc.job, Namespace: "default"},
				Status:     batchv1.JobStatus{Succeeded: 1},
			}
			r := &SwiftKernelReconciler{Client: fake.NewClientBuilder().WithScheme(kscheme.Scheme).WithObjects(job).Build(), Scheme: kscheme.Scheme}
			phase, _, err := r.CheckNodePullStatus(context.Background(), sk, "node-a")
			if err != nil {
				t.Fatal(err)
			}
			if phase != tc.want {
				t.Errorf("phase %s, want %s", phase, tc.want)
			}
		})
	}
}

// The upgrade from v0.14.1: the kernel is Ready on the strength of a Job that
// pulled into <namespace>-<name>. The next reconcile must pull into the
// current directory, report the node Pulling until that pull succeeds, and
// remove the old Job.
func TestReconcile_RepullsAKernelPulledIntoAnOldDirectory(t *testing.T) {
	ctx := context.Background()
	sk := &kernelv1alpha1.SwiftKernel{
		ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "default", UID: "sk-uid"},
		Spec:       kernelv1alpha1.SwiftKernelSpec{OCIRef: kernelv1alpha1.OCIRef{Image: "ghcr.io/example/kernel:1"}},
		Status: kernelv1alpha1.SwiftKernelStatus{
			Phase:        kernelv1alpha1.SwiftKernelPhaseReady,
			NodeStatuses: []kernelv1alpha1.NodeKernelStatus{{NodeName: "node-a", Phase: kernelv1alpha1.SwiftKernelPhaseReady}},
		},
	}
	other := &kernelv1alpha1.SwiftKernel{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "other-uid"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubeswift.io/kernel-node": "true"}}}
	ownedJob := func(owner *kernelv1alpha1.SwiftKernel, name, nodeName string, succeeded int32) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, schema.GroupVersionKind{
					Group: kernelv1alpha1.GroupName, Version: kernelv1alpha1.Version, Kind: "SwiftKernel",
				})},
			},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				NodeSelector: map[string]string{corev1.LabelHostname: nodeName},
			}}},
			Status: batchv1.JobStatus{Succeeded: succeeded},
		}
	}
	oldJob := ownedJob(sk, v0141JobName, "node-a", 1)
	// Left alone: another kernel's Job, and this kernel's Job on a node that is
	// no longer a kernel node.
	otherJob := ownedJob(other, "swiftkernel-pull-other-node-a", "node-a", 1)
	goneNodeJob := ownedJob(sk, "swiftkernel-pull-k-node-gone", "node-gone", 1)

	c := fake.NewClientBuilder().WithScheme(kscheme.Scheme).
		WithObjects(sk, other, node, oldJob, otherJob, goneNodeJob).
		WithStatusSubresource(&kernelv1alpha1.SwiftKernel{}).
		Build()
	r := &SwiftKernelReconciler{Client: c, Scheme: kscheme.Scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "k"}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var got kernelv1alpha1.SwiftKernel
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != kernelv1alpha1.SwiftKernelPhasePulling {
		t.Errorf("kernel phase %s while the pull into the current directory has not run, want Pulling", got.Status.Phase)
	}
	if len(got.Status.NodeStatuses) != 1 || got.Status.NodeStatuses[0].Phase != kernelv1alpha1.SwiftKernelPhasePulling {
		t.Errorf("node statuses %+v, want node-a Pulling", got.Status.NodeStatuses)
	}

	destDir := kernelv1alpha1.KernelLocalPath("default", "k")
	var newJob batchv1.Job
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: pullJobName("k", "node-a", destDir)}, &newJob); err != nil {
		t.Fatalf("no pull Job for the current directory: %v", err)
	}
	if script := strings.Join(newJob.Spec.Template.Spec.Containers[0].Command, " "); !strings.Contains(script, destDir) {
		t.Errorf("the new Job does not pull into %s: %s", destDir, script)
	}
	if newJob.Labels[pullKernelLabel] != "k" {
		t.Errorf("the new Job's labels %v do not name the kernel", newJob.Labels)
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(oldJob), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("the v0.14.1 Job was not deleted (err=%v)", err)
	}
	for _, j := range []*batchv1.Job{otherJob, goneNodeJob} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(j), &batchv1.Job{}); err != nil {
			t.Errorf("Job %s should have been left alone: %v", j.Name, err)
		}
	}

	newJob.Status.Succeeded = 1
	if err := c.Status().Update(ctx, &newJob); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != kernelv1alpha1.SwiftKernelPhaseReady {
		t.Errorf("kernel phase %s after the current directory's pull succeeded, want Ready", got.Status.Phase)
	}
}
