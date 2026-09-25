package swiftguest

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func kernelInPhase(phase kernelv1alpha1.SwiftKernelPhase) *kernelv1alpha1.SwiftKernel {
	k := readyKernel()
	k.Status.Phase = phase
	return k
}

// Every kernel re-pulls once after an upgrade from v0.14.1 (#658), and a
// kernel pulls whenever a node is newly labeled. A running guest resolved in
// that window was marked Failed while its VM ran, and a SwiftGuestPool deleted
// it to replace it.
func TestReconcile_PullingKernelLeavesARunningGuestRunning(t *testing.T) {
	g := kernelGuest()
	g.Status.Phase = swiftv1alpha1.SwiftGuestPhaseRunning
	c := guestClientBuilder(g, testGuestClass(), kernelInPhase(kernelv1alpha1.SwiftKernelPhasePulling),
		runningLauncher("worker-1")).Build()
	got, res, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		t.Errorf("phase = %q, want Running: the VM is up", got.Status.Phase)
	}
	cond := guestCondition(t, got, ConditionResolved)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "SwiftKernel not Ready") {
		t.Errorf("Resolved = %s %q; want False saying the kernel is not Ready", cond.Status, cond.Message)
	}
	if res.RequeueAfter <= 0 {
		t.Error("no requeue: nothing watches SwiftKernel")
	}
	if n := len(launcherPods(t, c)); n != 1 {
		t.Errorf("%d launcher pods, want the running one left alone", n)
	}
}

// A new guest waits for a pulling kernel, Pending rather than Failed, and
// starts once the kernel is Ready.
func TestReconcile_NewGuestWaitsForAPullingKernel(t *testing.T) {
	c := guestClientBuilder(kernelGuest(), testGuestClass(), kernelInPhase(kernelv1alpha1.SwiftKernelPhasePulling)).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	got, res, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Fatalf("created %d launcher pods before the kernel was on the nodes", n)
	}
	if res.RequeueAfter <= 0 {
		t.Error("no requeue: nothing watches SwiftKernel, so the guest would not start")
	}

	var k kernelv1alpha1.SwiftKernel
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "k"}, &k); err != nil {
		t.Fatal(err)
	}
	k.Status.Phase = kernelv1alpha1.SwiftKernelPhaseReady
	if err := c.Update(context.Background(), &k); err != nil {
		t.Fatal(err)
	}
	got, _, err = reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("reconcile after the pull: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 1 {
		t.Fatalf("after the pull: %d launcher pods, want 1", n)
	}
	if got.Status.Phase == swiftv1alpha1.SwiftGuestPhaseFailed || got.Status.Phase == swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("after the pull: phase = %q", got.Status.Phase)
	}
}

// A Failed kernel is final, and so is the guest.
func TestReconcile_FailedKernelFailsTheGuest(t *testing.T) {
	c := guestClientBuilder(kernelGuest(), testGuestClass(), kernelInPhase(kernelv1alpha1.SwiftKernelPhaseFailed)).Build()
	got, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
}
