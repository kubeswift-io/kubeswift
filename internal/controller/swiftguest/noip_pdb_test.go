package swiftguest

import (
	"context"
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// A running guest that has not reported an IP -- and some never do (a static
// address, SR-IOV, a DHCP timeout) -- must still get its drain-protecting
// PDB. The controller used to return early while waiting for the IP, so such
// a guest never got one and was requeued every 5s forever.
func TestReconcile_RunningGuestWithoutIPStillGetsItsPDB(t *testing.T) {
	c := guestClientBuilder(kernelGuest(), testGuestClass(), readyKernel(), runningLauncher("worker-1")).Build()
	got, res, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		t.Fatalf("phase = %q, want Running", got.Status.Phase)
	}
	var pdbs policyv1.PodDisruptionBudgetList
	if err := c.List(context.Background(), &pdbs, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	if len(pdbs.Items) != 1 {
		t.Errorf("%d PDBs for a running guest with no IP yet, want 1", len(pdbs.Items))
	}
	if res.RequeueAfter <= 0 {
		t.Error("should still look for the IP again later")
	}
}
