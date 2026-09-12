package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

func node(name string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
}

var cpTaint = corev1.Taint{
	Key:    "node-role.kubernetes.io/control-plane",
	Effect: corev1.TaintEffectNoSchedule,
}

// The reported escalation: spec.NodeName binds the pod directly, skipping the
// scheduler, so a control-plane NoSchedule taint did not stop a privileged
// launcher from landing there.
func TestCheckNodePlacement_RefusesUntoleratedControlPlane(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node("cp-1", cpTaint)).Build()
	guest := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "cp-1"}}
	err := checkNodePlacement(context.Background(), c, guest, &corev1.Pod{})
	if err == nil {
		t.Fatal("accepted an untolerated control-plane node")
	}
	if !strings.Contains(err.Error(), "control-plane") {
		t.Errorf("error should name the taint, got: %v", err)
	}
}

// A guest that legitimately targets a tainted node still can, via a toleration
// — exactly as an ordinary pod would.
func TestCheckNodePlacement_AllowsWithToleration(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node("cp-1", cpTaint)).Build()
	guest := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "cp-1"}}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Tolerations: []corev1.Toleration{{
		Key:      "node-role.kubernetes.io/control-plane",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}}}
	if err := checkNodePlacement(context.Background(), c, guest, pod); err != nil {
		t.Fatalf("rejected a tolerated placement: %v", err)
	}
}

func TestCheckNodePlacement_UntaintedAndUnpinned(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(node("worker-2")).Build()
	// Pinned to a clean node: fine.
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "worker-2"}}
	if err := checkNodePlacement(context.Background(), c, g, &corev1.Pod{}); err != nil {
		t.Errorf("clean node rejected: %v", err)
	}
	// Not pinned: the scheduler handles taints itself, so no check and no Node read.
	if err := checkNodePlacement(context.Background(), c, &swiftv1alpha1.SwiftGuest{}, &corev1.Pod{}); err != nil {
		t.Errorf("unpinned guest rejected: %v", err)
	}
}

func TestCheckNodePlacement_MissingNode(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "ghost"}}
	if err := checkNodePlacement(context.Background(), c, g, &corev1.Pod{}); err == nil {
		t.Fatal("accepted a nonexistent node")
	}
}

// PreferNoSchedule is a soft scheduler preference and never blocks placement,
// so enforcing it here would be stricter than Kubernetes itself.
func TestCheckNodePlacement_IgnoresPreferNoSchedule(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		node("worker-2", corev1.Taint{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}),
	).Build()
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "worker-2"}}
	if err := checkNodePlacement(context.Background(), c, g, &corev1.Pod{}); err != nil {
		t.Errorf("PreferNoSchedule should not block: %v", err)
	}
}

// The wildcard toleration (empty key + Exists) tolerates everything.
func TestTolerated_Wildcard(t *testing.T) {
	tols := []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	if !tolerated(cpTaint, tols) {
		t.Error("wildcard toleration should tolerate any taint")
	}
}

// TestCheckNodePlacementFor_RunsWithoutAPod is the #444 fix: the check has to be
// callable BEFORE the pod is built, because the root-disk clone Job pins to the
// same node and its pod would sit Pending forever, so reconcile never reached
// the old call site inside buildPod and the guest stalled with no reason set.
func TestCheckNodePlacementFor_RunsWithoutAPod(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule,
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(node).Build()
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "g1"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{NodeName: "cp-1"},
	}

	err := checkNodePlacementFor(context.Background(), c, guest, nil)
	if err == nil {
		t.Fatal("a guest pinned to a NoSchedule node was accepted; it would stall silently")
	}
	// The message must not send the operator to a field that does not exist.
	if strings.Contains(err.Error(), "spec.tolerations") {
		t.Errorf("error points at spec.tolerations, which SwiftGuest does not have: %v", err)
	}
	if !strings.Contains(err.Error(), "cp-1") {
		t.Errorf("error does not name the node: %v", err)
	}
}

// cordonedNode is what `kubectl cordon` leaves: spec.unschedulable, plus the
// taint the node lifecycle controller mirrors from it.
func cordonedNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.NodeSpec{
			Unschedulable: true,
			Taints:        []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}},
		},
	}
}

func pinnedGuest(nodeName string) *swiftv1alpha1.SwiftGuest {
	g := kernelGuest()
	g.Spec.NodeName = nodeName
	return g
}

// A cordon is routine and passes, so it reads as a cordon rather than a taint
// the guest "cannot tolerate". The taint trails spec.unschedulable in both
// directions, so either one alone counts.
func TestCheckNodePlacementFor_CordonReadsAsACordon(t *testing.T) {
	for name, spec := range map[string]corev1.NodeSpec{
		"cordoned": cordonedNode("worker-1").Spec,
		"spec.unschedulable, taint not yet added": {Unschedulable: true},
		"uncordoned, taint not yet cleared": {Taints: []corev1.Taint{{
			Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule,
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}, Spec: spec}).Build()
			err := checkNodePlacementFor(context.Background(), c, pinnedGuest("worker-1"), nil)
			if err == nil {
				t.Fatal("a new launcher was allowed onto a cordoned node")
			}
			if !strings.Contains(err.Error(), "cordoned") || strings.Contains(err.Error(), "cannot tolerate") {
				t.Errorf("should say the node is cordoned: %v", err)
			}
		})
	}
}

// A cordoned node that also carries a lasting taint must name the taint: an
// uncordon alone would not let the guest start.
func TestCheckNodePlacementFor_LastingTaintOutranksACordon(t *testing.T) {
	n := cordonedNode("cp-1")
	n.Spec.Taints = append(n.Spec.Taints, cpTaint)
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(n).Build()
	err := checkNodePlacementFor(context.Background(), c, pinnedGuest("cp-1"), nil)
	if err == nil || !strings.Contains(err.Error(), "node-role.kubernetes.io/control-plane") {
		t.Errorf("want the control-plane taint named, got: %v", err)
	}
}

// The reported bug: a cordon marked every running guest pinned to the node
// Failed while its VM kept running, froze its status and counted a VM failure.
// A SwiftGuestPool deletes Failed guests to replace them, so a pooled VM was
// killed outright.
func TestReconcile_CordonLeavesARunningPinnedGuestRunning(t *testing.T) {
	c := guestClientBuilder(pinnedGuest("worker-1"), testGuestClass(), readyKernel(),
		cordonedNode("worker-1"), runningLauncher("worker-1")).Build()
	got, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseRunning {
		t.Errorf("phase = %q, want Running: the VM is up", got.Status.Phase)
	}
	if got.Status.NodeName != "worker-1" {
		t.Errorf("status.nodeName = %q: the running launcher is not being reported", got.Status.NodeName)
	}
	if n := len(launcherPods(t, c)); n != 1 {
		t.Errorf("%d launcher pods, want the running one left alone", n)
	}
}

// With no launcher yet, a cordoned pin holds the guest: no new launcher on a
// cordoned node, but no failure either, because a cordon passes.
func TestReconcile_CordonHoldsANewPinnedLauncher(t *testing.T) {
	c := guestClientBuilder(pinnedGuest("worker-1"), testGuestClass(), readyKernel(), cordonedNode("worker-1")).Build()
	got, res, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Fatalf("created %d launcher pods on a cordoned node", n)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	cond := guestCondition(t, got, ConditionPodScheduled)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "cordoned") {
		t.Errorf("PodScheduled = %s %q; want False saying the node is cordoned", cond.Status, cond.Message)
	}
	if res.RequeueAfter <= 0 {
		t.Error("no requeue: nothing watches Nodes, so the guest would not start after the uncordon")
	}
}

// ...and starts the guest once the node is uncordoned.
func TestReconcile_PinnedGuestStartsOnceUncordoned(t *testing.T) {
	c := guestClientBuilder(pinnedGuest("worker-1"), testGuestClass(), readyKernel(), cordonedNode("worker-1")).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme}
	if _, _, err := reconcileGuest(t, r); err != nil {
		t.Fatalf("held pass: %v", err)
	}
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "worker-1"}, &n); err != nil {
		t.Fatal(err)
	}
	n.Spec.Unschedulable = false
	n.Spec.Taints = nil
	if err := c.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	got, _, err := reconcileGuest(t, r)
	if err != nil {
		t.Fatalf("pass after uncordon: %v", err)
	}
	if pods := launcherPods(t, c); len(pods) != 1 {
		t.Fatalf("after uncordon: %d launcher pods, want 1", len(pods))
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhaseScheduling {
		t.Errorf("after uncordon: phase = %q, want Scheduling", got.Status.Phase)
	}
}

// #444's case still holds: a disk-boot guest pinned to a node it cannot use
// used to clone its root disk first, with a Job pinned to that node that never
// ran. It must wait, name the taint, and clone nothing.
func TestReconcile_TaintedPinnedNodeHoldsWithoutCloningTheRootDisk(t *testing.T) {
	c := guestClientBuilder(asDiskBoot(pinnedGuest("cp-1")), testGuestClass(), readyImage(), preparedPVC(),
		node("cp-1", cpTaint)).Build()
	got, res, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.Status.Phase != swiftv1alpha1.SwiftGuestPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
	cond := guestCondition(t, got, ConditionPodScheduled)
	if cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, "node-role.kubernetes.io/control-plane") {
		t.Errorf("PodScheduled = %s %q; want False naming the taint", cond.Status, cond.Message)
	}
	if res.RequeueAfter <= 0 {
		t.Error("no requeue: removing the taint would not start the guest")
	}
	var clone corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: RootDiskCloneName(testGuestName)}, &clone); !apierrors.IsNotFound(err) {
		t.Errorf("root disk clone for a guest whose node cannot take it: err = %v, want NotFound", err)
	}
}

// The creation-time check is the security boundary: spec.nodeName skips the
// scheduler, and the launcher is privileged. A launcher present when the
// reconcile starts can be gone by the time it creates one (a drain evicting it
// from a cordoned node is exactly that), and the replacement must still be
// checked.
func TestReconcile_LauncherGoneMidReconcileIsNotRecreatedOnACordonedNode(t *testing.T) {
	served := false
	c := guestClientBuilder(pinnedGuest("worker-1"), testGuestClass(), readyKernel(), cordonedNode("worker-1")).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if pod, ok := obj.(*corev1.Pod); ok && key == testGuestKey && !served {
					served = true
					runningLauncher("worker-1").DeepCopyInto(pod)
					return nil
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()
	got, _, err := reconcileGuest(t, &SwiftGuestReconciler{Client: c, Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n := len(launcherPods(t, c)); n != 0 {
		t.Fatalf("created %d launcher pods on a cordoned node after the running one went away", n)
	}
	if got.Status.Phase == swiftv1alpha1.SwiftGuestPhaseFailed {
		t.Error("a cordon failed the guest")
	}
}

func TestCheckNodePlacementFor_UntaintedNodeIsFine(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}}).Build()
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "g1"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{NodeName: "worker-2"},
	}
	if err := checkNodePlacementFor(context.Background(), c, guest, nil); err != nil {
		t.Errorf("rejected an untainted node: %v", err)
	}
}
