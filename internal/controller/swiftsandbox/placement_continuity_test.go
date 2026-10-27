package swiftsandbox

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// Placement target continuity across warm-pool checkouts. The targets are the
// nodes of the pool's running slots, warm or claimed; these tests hold that a
// checkout and its refill do not take a prepared node's Prepared state away
// unless the node really stops running a slot, and that a placement's status
// does not regress when it reconciles before its own last write is cached.

// claimedSlot is slot name on node after sandbox sb checked it out: the pool
// label stays, slot-state is claimed, the sandbox is its controller.
func claimedSlot(name, node, sb string) *corev1.Pod {
	p := poolSlot(name, node)
	isController := true
	p.Labels[SlotStateLabelKey] = slotStateClaimed
	p.Labels[SandboxLabelKey] = sb
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: sandboxv1alpha1.GroupName + "/v1alpha1", Kind: "SwiftSandbox", Name: sb, UID: "sb-uid", Controller: &isController}}
	return p
}

// reconcileStale runs one pass whose cached client serves the placement as it
// was (stale), the way the controller often sees it right after its own write.
// withReader gives the reconciler the uncached reader (the API server's view).
func (e *placementEnv) reconcileStale(stale *sandboxv1alpha1.SwiftArtifactPlacement, withReader bool) {
	e.t.Helper()
	cached := interceptor.NewClient(e.c.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if p, ok := obj.(*sandboxv1alpha1.SwiftArtifactPlacement); ok && key.Name == stale.Name {
			stale.DeepCopyInto(p)
			return nil
		}
		return cl.Get(ctx, key, obj, opts...)
	}})
	r := &ArtifactPlacementReconciler{Client: cached, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(50), resolver: e.res, now: func() time.Time { return e.now }}
	if withReader {
		r.APIReader = e.c
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "cache"}}); err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
}

func preparedNow(t *testing.T, e *placementEnv) *sandboxv1alpha1.SwiftArtifactPlacement {
	t.Helper()
	e.reconcile()
	e.finish("node-a", nil)
	e.reconcile()
	p := e.placement()
	if c := preparedCondition(p); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("setup: Prepared = %+v", c)
	}
	return p
}

// The flap seen on a cluster: right after recording node-a prepared (and
// deleting its finished prepare pod) the controller reconciled from a cached
// copy that still said Preparing, started another prepare pod and wrote
// Preparing over Prepared. Reading the placement uncached keeps it Prepared.
func TestPlacement_StaleCacheDoesNotUnprepare(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	e.reconcile()
	stale := e.placement() // Preparing, as the cache still has it
	e.finish("node-a", nil)
	e.reconcile() // node-a prepared; its pod is deleted

	e.reconcileStale(stale, true)
	if len(e.preparePods()) != 0 {
		t.Error("a pass from a stale copy started another prepare pod for a prepared node")
	}
	if c := preparedCondition(e.placement()); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Prepared = %+v after a stale pass, want True", c)
	}

	// A write made from an older read is refused, not merged over the newer
	// status: status.nodes is a list a merge patch replaces whole.
	r := &ArtifactPlacementReconciler{Client: e.c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(50)}
	old := stale.DeepCopy()
	orig := old.Status.DeepCopy()
	old.Status.Nodes = nil
	if _, err := r.finish(context.Background(), old, orig, time.Second); !apierrors.IsConflict(err) {
		t.Errorf("a status write from an outdated read: err=%v, want a conflict", err)
	}
	if c := preparedCondition(e.placement()); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Prepared = %+v after a refused stale write, want True", c)
	}
}

// A prepared node stays Prepared while its periodic recheck runs; only a
// recheck that fails unprepares it.
func TestPlacement_RecheckKeepsPrepared(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	preparedNow(t, e)
	e.now = e.now.Add(placementRecheck + time.Second)
	e.reconcile()
	if e.preparePods()["node-a"] == nil {
		t.Fatal("no recheck pod after placementRecheck")
	}
	e.reconcile() // the recheck pod is running
	if c := preparedCondition(e.placement()); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Prepared = %+v while a recheck runs, want True", c)
	}
	e.finish("node-a", nil)
	e.reconcile()
	if c := preparedCondition(e.placement()); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Prepared = %+v after a passing recheck", c)
	}
}

// A one-slot pool: the slot on node-a is checked out and its refill lands on
// node-a too. node-a stays a target throughout (the claimed slot runs there),
// so the placement never leaves Prepared.
func TestPlacement_OneSlotCheckout_SameNode(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	preparedNow(t, e)

	// Claim: relabel s1, owner the sandbox; the refill is created unscheduled.
	if err := e.c.Delete(context.Background(), poolSlot("s1", "node-a")); err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{claimedSlot("s1", "node-a", "sb1"), poolSlot("s2", "")} {
		if err := e.c.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	e.reconcile()
	assertTargets(t, e, true, "node-a")

	// The refill is scheduled on node-a; the workload ends.
	setNode(t, e, "s2", "node-a")
	finishPod(t, e, "s1")
	e.reconcile()
	assertTargets(t, e, true, "node-a")
	if len(e.preparePods()) != 0 {
		t.Error("node-a was prepared again although it never stopped running a slot")
	}
}

// The refill lands on node-b: node-a (the running checkout) and node-b (the
// next checkout) are both targets until the checkout ends, then only node-b.
// The placement reports Preparing while node-b is unconfirmed, and node-a's
// prepared state is kept meanwhile.
func TestPlacement_OneSlotCheckout_OtherNode(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	preparedNow(t, e)
	if err := e.c.Delete(context.Background(), poolSlot("s1", "node-a")); err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{claimedSlot("s1", "node-a", "sb1"), poolSlot("s2", "node-b")} {
		if err := e.c.Create(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	e.reconcile()
	p := e.placement()
	if p.Status.TargetNodes != 2 || !nodeStatus(p, "node-a").Prepared || nodeStatus(p, "node-b").Prepared {
		t.Fatalf("targets=%d nodes=%+v; want node-a prepared, node-b preparing", p.Status.TargetNodes, p.Status.Nodes)
	}
	e.finish("node-b", nil)
	e.reconcile()
	assertTargets(t, e, true, "node-a", "node-b")

	finishPod(t, e, "s1")
	e.reconcile()
	assertTargets(t, e, true, "node-b")
}

// Scaled to zero with a checkout still running: its node stays a target while
// the workload runs (it is a pool workload on that node), and the placement
// has no target once it ends. Nothing keeps a node after the pool's last pod.
func TestPlacement_ScaleToZero_EndsWithTheLastCheckout(t *testing.T) {
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"))
	preparedNow(t, e)
	if err := e.c.Delete(context.Background(), poolSlot("s1", "node-a")); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(context.Background(), claimedSlot("s1", "node-a", "sb1")); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	assertTargets(t, e, true, "node-a")
	finishPod(t, e, "s1")
	e.reconcile()
	p := e.placement()
	if p.Status.TargetNodes != 0 || len(p.Status.Nodes) != 0 {
		t.Fatalf("targets=%d nodes=%v after the last pool pod ended", p.Status.TargetNodes, p.Status.Nodes)
	}
	if c := preparedCondition(p); c == nil || c.Reason != sandboxv1alpha1.PlacementReasonNoTargetNodes {
		t.Errorf("Prepared = %+v, want NoTargetNodes", c)
	}
}

// Only the pool's own slots count: a pod carrying the pool label that the pool
// does not control (nor a sandbox that claimed it) adds no node, nor does a
// claimed slot whose controller is not the sandbox its label names.
func TestPlacement_OnlyPoolSlotsAreTargets(t *testing.T) {
	stray := poolSlot("stray", "node-x")
	stray.OwnerReferences = nil
	forged := claimedSlot("forged", "node-y", "sb1")
	forged.Labels[SandboxLabelKey] = "someone-else"
	other := poolSlot("other", "node-z")
	other.OwnerReferences[0].Name = "another-pool"
	e := newPlacementEnv(t, testPlacement(plDigestA), poolSlot("s1", "node-a"), claimedSlot("s2", "node-b", "sb2"), stray, forged, other)
	e.reconcile()
	p := e.placement()
	got := map[string]bool{}
	for _, n := range p.Status.Nodes {
		got[n.Name] = true
	}
	if len(got) != 2 || !got["node-a"] || !got["node-b"] {
		t.Fatalf("targets %v, want node-a (warm) and node-b (claimed) only", got)
	}
}

// Two placements on one pool see the same targets.
func TestPlacement_MultiplePlacementsAgree(t *testing.T) {
	second := testPlacement(plDigestB)
	second.Name = "cache2"
	e := newPlacementEnv(t, testPlacement(plDigestA), second, poolSlot("s1", "node-a"), claimedSlot("s2", "node-b", "sb2"))
	e.reconcile()
	r := &ArtifactPlacementReconciler{Client: e.c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(50), resolver: e.res, now: func() time.Time { return e.now }}
	a, err := r.targetNodes(context.Background(), testPlacement(plDigestA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.targetNodes(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || len(b) != 2 || a["node-a"] == nil || b["node-b"] == nil {
		t.Fatalf("targets differ: %v vs %v", keys(a), keys(b))
	}
}

func assertTargets(t *testing.T, e *placementEnv, prepared bool, nodes ...string) {
	t.Helper()
	p := e.placement()
	if int(p.Status.TargetNodes) != len(nodes) {
		t.Fatalf("targets=%d (%+v), want %v", p.Status.TargetNodes, p.Status.Nodes, nodes)
	}
	for _, n := range nodes {
		if s := nodeStatus(p, n); s == nil || s.Prepared != prepared {
			t.Fatalf("node %s status %+v, want prepared=%v", n, s, prepared)
		}
	}
	c := preparedCondition(p)
	if prepared && (c == nil || c.Status != metav1.ConditionTrue) {
		t.Fatalf("Prepared = %+v, want True", c)
	}
}

func setNode(t *testing.T, e *placementEnv, pod, node string) {
	t.Helper()
	var p corev1.Pod
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: pod}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.NodeName = node
	if err := e.c.Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
}

func finishPod(t *testing.T, e *placementEnv, pod string) {
	t.Helper()
	var p corev1.Pod
	if err := e.c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: pod}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = corev1.PodSucceeded
	if err := e.c.Status().Update(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
}
