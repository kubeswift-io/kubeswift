package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// diskOn is a guest whose shared-base root disk lives on node. A kernel-boot
// guest keeps these tests about PLACEMENT alone, with no disk path involved —
// which is exactly the scope of the pin.
func diskOn(node string) *swiftv1alpha1.SwiftGuest {
	g := kernelGuest()
	g.Status.SharedBaseDisk = &swiftv1alpha1.SharedBaseDiskStatus{Node: node, BaseKey: "uid-img/uid-pvc"}
	return g
}

func TestPinnedNode(t *testing.T) {
	gpuOn := func(g *swiftv1alpha1.SwiftGuest, n string) *swiftv1alpha1.SwiftGuest {
		g.Status.GPU = &swiftv1alpha1.GPUStatus{NodeName: n}
		return g
	}
	specOn := func(g *swiftv1alpha1.SwiftGuest, n string) *swiftv1alpha1.SwiftGuest {
		g.Spec.NodeName = n
		return g
	}
	for _, tc := range []struct {
		name       string
		guest      *swiftv1alpha1.SwiftGuest
		wantNode   string
		wantSource string
		wantErr    string
	}{
		// Unchanged for every guest without a shared-base disk.
		{"unpinned", kernelGuest(), "", "", ""},
		{"spec only", specOn(kernelGuest(), "a"), "a", "spec.nodeName", ""},
		// The disk wins.
		{"disk only", diskOn("a"), "a", "status.sharedBaseDisk.node", ""},
		{"disk and spec agree", specOn(diskOn("a"), "a"), "a", "status.sharedBaseDisk.node", ""},
		{"disk and GPU agree", gpuOn(diskOn("a"), "a"), "a", "status.sharedBaseDisk.node", ""},
		// Disagreement is an error, never a silent choice.
		{"spec disagrees", specOn(diskOn("a"), "b"), "", "", "has no copy of it"},
		{"GPU disagrees", gpuOn(diskOn("a"), "b"), "", "", "only run where"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node, source, err := pinnedNode(tc.guest)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("got node %q, want an error: disagreeing pins must never be resolved silently", node)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q should contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if node != tc.wantNode || source != tc.wantSource {
				t.Errorf("got (%q, %q), want (%q, %q)", node, source, tc.wantNode, tc.wantSource)
			}
		})
	}
}

// The property that matters: a shared-base guest's launcher is NEVER left for
// the scheduler to place, because on any other node it cannot start. Including
// when the pins disagree — the placement check should have held the guest
// before a pod was built, but if this is ever reached, the disk's node is the
// only safe answer.
func TestApplyNodeName_NeverLeavesASharedBaseGuestUnpinned(t *testing.T) {
	for name, g := range map[string]*swiftv1alpha1.SwiftGuest{
		"disk only": diskOn("a"),
		"conflicting spec.nodeName": func() *swiftv1alpha1.SwiftGuest {
			g := diskOn("a")
			g.Spec.NodeName = "b"
			return g
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			pod := &corev1.Pod{}
			applyNodeName(pod, g)
			if pod.Spec.NodeName != "a" {
				t.Fatalf("pod.spec.nodeName = %q, want %q (the disk's node)", pod.Spec.NodeName, "a")
			}
		})
	}
}

// Pinned by a disk skips the scheduler, so a schedulerName would be inert
// configuration that reads as if it did something.
func TestApplySchedulerName_SkippedWhenPinnedByADisk(t *testing.T) {
	g := diskOn("a")
	g.Spec.SchedulerName = "least-allocated"
	pod := &corev1.Pod{}
	applySchedulerName(pod, g)
	if pod.Spec.SchedulerName != "" {
		t.Errorf("schedulerName = %q on a disk-pinned launcher", pod.Spec.SchedulerName)
	}
}

func placementClient(t *testing.T, objs ...*corev1.Node) *fake.ClientBuilder {
	t.Helper()
	b := fake.NewClientBuilder().WithScheme(scheme.Scheme)
	for _, n := range objs {
		b = b.WithObjects(n)
	}
	return b
}

// A missing disk node is not a typo to correct. The message must say the disk
// cannot move, and must not suggest re-pinning.
func TestCheckNodePlacementFor_MissingDiskNodeSaysTheDiskCannotMove(t *testing.T) {
	c := placementClient(t).Build()
	err := checkNodePlacementFor(context.Background(), c, diskOn("gone"), nil)
	if err == nil {
		t.Fatal("a guest whose disk node does not exist was allowed to place")
	}
	for _, want := range []string{"gone", "cannot move", "recreate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q: %v", want, err)
		}
	}
}

// Re-pinning cannot help a guest whose disk is on the tainted node, so the
// advice must not offer it — only removing the taint works.
func TestCheckNodePlacementFor_TaintedDiskNodeDoesNotSuggestRepinning(t *testing.T) {
	c := placementClient(t, node("a", corev1.Taint{Key: "maintenance", Effect: corev1.TaintEffectNoSchedule})).Build()
	err := checkNodePlacementFor(context.Background(), c, diskOn("a"), nil)
	if err == nil {
		t.Fatal("a tainted disk node was allowed")
	}
	if strings.Contains(err.Error(), "pin the guest to an untainted node") {
		t.Errorf("recommends re-pinning, which cannot help a node-local disk: %v", err)
	}
	if !strings.Contains(err.Error(), "remove the taint") {
		t.Errorf("should say to remove the taint: %v", err)
	}
}

// End to end through Reconcile: with no spec.nodeName at all, the launcher is
// created PINNED to the disk's node rather than handed to the scheduler.
func TestReconcile_SharedBaseGuestIsPinnedToItsDiskNode(t *testing.T) {
	c := guestClientBuilder(diskOn("worker-1"), testGuestClass(), readyKernel(), node("worker-1")).Build()
	if _, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := launcherPods(t, c)
	if len(pods) != 1 {
		t.Fatalf("got %d launcher pods, want 1", len(pods))
	}
	if pods[0].Spec.NodeName != "worker-1" {
		t.Fatalf("launcher placed on %q; a shared-base guest must run on its disk's node %q",
			pods[0].Spec.NodeName, "worker-1")
	}
}

// If the disk's node is gone, the guest waits. It does not start anywhere else.
func TestReconcile_SharedBaseGuestWaitsWhenItsDiskNodeIsGone(t *testing.T) {
	c := guestClientBuilder(diskOn("gone"), testGuestClass(), readyKernel(), node("worker-2")).Build()
	got, res, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Fatalf("created %d launcher pods while the disk's node is gone — that is an empty disk elsewhere", n)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	cond := guestCondition(t, got, ConditionPodScheduled)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "cannot move") {
		t.Errorf("PodScheduled = %s %q; want False explaining the disk cannot move", cond.Status, cond.Message)
	}
	if res.RequeueAfter <= 0 {
		t.Error("no requeue: a node that comes back would never restart the guest")
	}
}

// A spec.nodeName that disagrees with the disk holds the guest with the reason —
// visibly, in status — rather than obeying either pin.
func TestReconcile_ConflictingNodeNameHoldsTheGuest(t *testing.T) {
	g := diskOn("worker-1")
	g.Spec.NodeName = "worker-2"
	c := guestClientBuilder(g, testGuestClass(), readyKernel(), node("worker-1"), node("worker-2")).Build()
	got, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Fatalf("created %d launcher pods despite disagreeing pins", n)
	}
	cond := guestCondition(t, got, ConditionPodScheduled)
	if !strings.Contains(cond.Message, "worker-1") || !strings.Contains(cond.Message, "worker-2") {
		t.Errorf("PodScheduled should name both nodes: %q", cond.Message)
	}
}
