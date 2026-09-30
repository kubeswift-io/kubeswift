package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func testNamespace(terminating bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}
	if terminating {
		now := metav1.Now()
		ns.DeletionTimestamp = &now
		ns.Finalizers = []string{"kubernetes"}
	}
	return ns
}

// Lab validation of v0.15.0: deleting a test namespace that held guests
// logged 10 to 30 reconciler errors, each a create (launcher pod, RBAC,
// ConfigMap) the namespace refused because it was being deleted. A guest in
// such a namespace is not built; it is deleted next.
func TestReconcile_TerminatingNamespaceBuildsNothing(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		c := guestClientBuilder(kernelGuest(), testGuestClass(), readyKernel(), testNamespace(terminating)).Build()
		r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
		if _, _, err := reconcileGuest(t, r); err != nil {
			t.Fatalf("terminating=%v: reconcile: %v", terminating, err)
		}
		var cms corev1.ConfigMapList
		if err := c.List(context.Background(), &cms); err != nil {
			t.Fatal(err)
		}
		built := len(launcherPods(t, c)) + len(cms.Items)
		if terminating && built != 0 {
			t.Errorf("built %d pods and ConfigMaps in a namespace being deleted", built)
		}
		if !terminating && built == 0 {
			t.Fatal("control: nothing built in a live namespace, so the check above proves nothing")
		}
	}
}
