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
