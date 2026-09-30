package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Lab validation of v0.15.0: a sandbox in a namespace being deleted kept
// trying to create its RBAC and launcher there, and each refusal was logged as
// a reconciler error. It is deleted with the namespace; nothing is built.
func TestSandboxReconcile_TerminatingNamespaceBuildsNothing(t *testing.T) {
	for _, terminating := range []bool{false, true} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		if terminating {
			now := metav1.Now()
			ns.DeletionTimestamp = &now
			ns.Finalizers = []string{"kubernetes"}
		}
		r, c := sandboxReconciler(ns, plainSandbox("registry.invalid/never/pulled:1"))
		res := reconcileSB(t, r, "sb")

		var rbs rbacv1.RoleBindingList
		if err := c.List(context.Background(), &rbs); err != nil {
			t.Fatal(err)
		}
		if terminating {
			if len(rbs.Items) != 0 {
				t.Errorf("created %d RoleBindings in a namespace being deleted", len(rbs.Items))
			}
			if res.RequeueAfter != 0 {
				t.Errorf("requeue = %v; the sandbox is deleted next, nothing to wait for", res.RequeueAfter)
			}
			assertNoLauncher(t, c, "sb")
		} else if len(rbs.Items) == 0 {
			t.Fatal("control: no RoleBinding in a live namespace, so the check above proves nothing")
		}
	}
}
