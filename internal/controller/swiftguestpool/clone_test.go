package swiftguestpool

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func node(name string, ready bool, mut func(*corev1.Node)) *corev1.Node {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}}},
	}
	if mut != nil {
		mut(n)
	}
	return n
}

func poolReconciler(t *testing.T, objs ...client.Object) *SwiftGuestPoolReconciler {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	return &SwiftGuestPoolReconciler{Client: c}
}

func TestAssignCloneTargetNode_RoundRobin(t *testing.T) {
	r := poolReconciler(t,
		node("boba", true, nil),
		node("miles", true, nil),
		node("frida", true, func(n *corev1.Node) { // control-plane: excluded
			n.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
		}),
		node("down", false, nil), // not ready: excluded
		node("cordoned", true, func(n *corev1.Node) { n.Spec.Unschedulable = true }), // excluded
	)
	// schedulable workers sorted: [boba, miles]. Round-robin by index.
	want := []string{"boba", "miles", "boba", "miles"}
	for i, w := range want {
		spec := &swiftv1alpha1.SwiftGuestSpec{
			CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{
				SnapshotRef: corev1.LocalObjectReference{Name: "snap"},
			},
		}
		if err := r.assignCloneTargetNode(context.Background(), spec, i); err != nil {
			t.Fatalf("index %d: %v", i, err)
		}
		if spec.CloneFromSnapshot.TargetNode != w {
			t.Errorf("index %d: targetNode = %q, want %q", i, spec.CloneFromSnapshot.TargetNode, w)
		}
	}
}

func TestAssignCloneTargetNode_NonCloneNoop(t *testing.T) {
	r := poolReconciler(t, node("boba", true, nil))
	spec := &swiftv1alpha1.SwiftGuestSpec{ImageRef: &corev1.LocalObjectReference{Name: "img"}}
	if err := r.assignCloneTargetNode(context.Background(), spec, 0); err != nil {
		t.Fatalf("non-clone should be a no-op: %v", err)
	}
}

func TestAssignCloneTargetNode_NoSchedulableNodes(t *testing.T) {
	r := poolReconciler(t, node("frida", true, func(n *corev1.Node) {
		n.Labels = map[string]string{"node-role.kubernetes.io/control-plane": ""}
	}))
	spec := &swiftv1alpha1.SwiftGuestSpec{
		CloneFromSnapshot: &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}},
	}
	if err := r.assignCloneTargetNode(context.Background(), spec, 0); err != errNoSchedulableNodes {
		t.Fatalf("want errNoSchedulableNodes, got %v", err)
	}
}
