package swiftkernel

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	kscheme "github.com/kubeswift-io/kubeswift/internal/scheme"
)

// Lab validation of v0.15.0: a namespaced SwiftKernel in a namespace being
// deleted tried to create its pull Jobs there, and each refusal was logged as
// a reconciler error. It is deleted with the namespace; nothing is pulled.
func TestReconcile_TerminatingNamespacePullsNothing(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}
		if terminating {
			now := metav1.Now()
			ns.DeletionTimestamp = &now
			ns.Finalizers = []string{"kubernetes"}
		}
		sk := &kernelv1alpha1.SwiftKernel{
			ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "ns"},
			Spec:       kernelv1alpha1.SwiftKernelSpec{OCIRef: kernelv1alpha1.OCIRef{Image: "ghcr.io/example/kernel:1"}},
		}
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubeswift.io/kernel-node": "true"}}}
		c := fake.NewClientBuilder().WithScheme(kscheme.Scheme).WithObjects(ns, sk, node).WithStatusSubresource(sk).Build()
		r := &SwiftKernelReconciler{Client: c, Scheme: kscheme.Scheme}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "k"}}); err != nil {
			t.Fatalf("terminating=%v: reconcile: %v", terminating, err)
		}
		var jobs batchv1.JobList
		if err := c.List(context.Background(), &jobs); err != nil {
			t.Fatal(err)
		}
		if terminating && len(jobs.Items) != 0 {
			t.Errorf("created %d pull Jobs for a kernel in a namespace being deleted", len(jobs.Items))
		}
		if !terminating && len(jobs.Items) == 0 {
			t.Fatal("control: no pull Job in a live namespace, so the check above proves nothing")
		}
	}
}
