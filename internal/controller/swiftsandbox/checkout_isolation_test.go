package swiftsandbox

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

// The slot's deny-ingress NetworkPolicy selected SandboxLabelKey=<slot>,
// and checkout rewrites that label to the claiming sandbox's name -- so every
// checked-out workload ran with no ingress isolation. The policy must still
// select the pod after a claim, and go to the sandbox along with the pod.
func TestCheckout_SlotStaysIsolatedAndItsObjectsMoveToTheSandbox(t *testing.T) {
	ctx := context.Background()
	pool := &sandboxv1alpha1.SwiftSandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "pool-uid"},
		Spec:       sandboxv1alpha1.SwiftSandboxPoolSpec{Image: "busybox:1"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(pool).Build()
	pr := &SwiftSandboxPoolReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}
	if err := pr.createWarmSlot(ctx, pool, defaultKernelProfile, resolvedImage{RootfsPath: "/cache/x.ext4"}, ""); err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace("ns")); err != nil || len(pods.Items) != 1 {
		t.Fatalf("want one slot pod, got %d (err=%v)", len(pods.Items), err)
	}
	slot := pods.Items[0]
	slot.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: launcherName, Ready: true}}}
	if err := c.Status().Update(ctx, &slot); err != nil {
		t.Fatal(err)
	}

	sb := &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb", Namespace: "ns", UID: "sb-uid"},
		Spec:       sandboxv1alpha1.SwiftSandboxSpec{Image: "busybox:1", PoolRef: &corev1.LocalObjectReference{Name: "p"}},
	}
	if err := c.Create(ctx, sb); err != nil {
		t.Fatal(err)
	}
	r := &SwiftSandboxReconciler{Client: c, Scheme: scheme.Scheme}
	claimed, err := r.tryClaimWarmSlot(ctx, sb)
	if err != nil || claimed == nil {
		t.Fatalf("claim: slot=%v err=%v", claimed, err)
	}
	if err := r.adoptSlotObjects(ctx, sb, claimed); err != nil {
		t.Fatal(err)
	}

	var nps networkingv1.NetworkPolicyList
	if err := c.List(ctx, &nps, client.InNamespace("ns")); err != nil || len(nps.Items) != 1 {
		t.Fatalf("want the slot's NetworkPolicy, got %d (err=%v)", len(nps.Items), err)
	}
	np := nps.Items[0]
	sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Matches(labels.Set(claimed.Labels)) {
		t.Errorf("after checkout the deny-ingress policy (%v) no longer selects the pod %v", np.Spec.PodSelector.MatchLabels, claimed.Labels)
	}
	if !metav1.IsControlledBy(&np, sb) {
		t.Errorf("NetworkPolicy owner = %v, want the claiming sandbox", np.OwnerReferences)
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: claimed.Name + intentConfigMapSuffix}, &cm); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&cm, sb) {
		t.Errorf("intent ConfigMap owner = %v, want the claiming sandbox", cm.OwnerReferences)
	}
}
