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
		WithObjects(node("frida", cpTaint)).Build()
	guest := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "frida"}}
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
		WithObjects(node("frida", cpTaint)).Build()
	guest := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "frida"}}
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
		WithObjects(node("miles")).Build()
	// Pinned to a clean node: fine.
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "miles"}}
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
		node("miles", corev1.Taint{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule}),
	).Build()
	g := &swiftv1alpha1.SwiftGuest{Spec: swiftv1alpha1.SwiftGuestSpec{NodeName: "miles"}}
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
