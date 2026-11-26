package podmode

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftsandbox"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

const (
	shim  = "ghcr.io/kubeswift-io/kubeswift/pod-shim:v0.27.0"
	pmNS  = "kubeswift-sandboxes"
	uid   = "0b0e4c4e-7a5b-4c2e-9d55-2f4a1c9e7d10"
	sbKey = SandboxPrefix + uid
)

func standIn(t *testing.T) *corev1.Pod {
	t.Helper()
	in := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-1", Namespace: "tenant", UID: uid},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(podmode.RuntimeClassName),
			Containers: []corev1.Container{{
				Name:  "sandbox",
				Image: "registry.k8s.io/agent-sandbox/sandboxd:v1.0.12",
				Ports: []corev1.ContainerPort{{ContainerPort: 8080}, {ContainerPort: 9090}},
			}},
		},
	}
	out, err := podmode.Rewrite(in, shim)
	if err != nil {
		t.Fatal(err)
	}
	out.Spec.NodeName = "node-a"
	return out
}

func setup(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *record.FakeRecorder) {
	t.Helper()
	base := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{podmode.NamespaceLabel: podmode.NamespaceLabelValue}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{corev1.LabelHostname: "host-a"}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(append(base, objs...)...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}, &corev1.Pod{}).Build()
	rec := record.NewFakeRecorder(20)
	return &Reconciler{Client: c, Scheme: scheme.Scheme, Recorder: rec, Namespace: pmNS,
		ShimImage: func() string { return shim }}, c, rec
}

func reconcileOnce(t *testing.T, r *Reconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLinkCreatesSandboxPoliciesAndTarget(t *testing.T) {
	r, c, _ := setup(t, standIn(t))
	reconcileOnce(t, r, "tenant", "sb-1")

	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Labels[podmode.LabelUID] != uid || len(pod.Finalizers) != 1 || pod.Finalizers[0] != podmode.Finalizer {
		t.Fatalf("labels %v finalizers %v", pod.Labels, pod.Finalizers)
	}
	var sb sandboxv1alpha1.SwiftSandbox
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: pmNS, Name: sbKey}, &sb); err != nil {
		t.Fatal(err)
	}
	if sb.Spec.Image != "registry.k8s.io/agent-sandbox/sandboxd:v1.0.12" || sb.Spec.NodeSelector[corev1.LabelHostname] != "host-a" ||
		sb.Spec.Network.Mode != sandboxv1alpha1.SandboxNetworkRestricted || len(sb.Spec.Network.Ports) != 2 {
		t.Errorf("sandbox spec %+v", sb.Spec)
	}
	if sb.Labels[podmode.LabelStandInNamespace] != "tenant" || sb.Annotations[podmode.AnnotationStandIn] != "tenant/sb-1" {
		t.Errorf("sandbox metadata %v %v", sb.Labels, sb.Annotations)
	}
	from := sb.Spec.Network.Ingress.From
	if len(from) != 1 || from[0].PodSelector.MatchLabels[podmode.LabelUID] != uid ||
		from[0].NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != "tenant" {
		t.Errorf("ingress %+v", from)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: NetworkPolicyPrefix + uid}, &np); err != nil {
		t.Fatal(err)
	}
	if len(np.OwnerReferences) != 1 || np.OwnerReferences[0].UID != uid {
		t.Errorf("policy owner %+v", np.OwnerReferences)
	}
	if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].Ports) != 2 ||
		np.Spec.Egress[0].To[0].PodSelector.MatchLabels[swiftsandbox.SandboxLabelKey] != sbKey {
		t.Errorf("policy egress %+v", np.Spec.Egress)
	}
	if _, ok := pod.Annotations[podmode.AnnotationTarget]; ok {
		t.Error("target set before the launcher runs")
	}

	launcher := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: sbKey + "-launcher", Namespace: pmNS, Labels: map[string]string{swiftsandbox.SandboxLabelKey: sbKey}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.2.3"},
	}
	if err := c.Create(context.Background(), launcher); err != nil {
		t.Fatal(err)
	}
	if got := r.mapPod(context.Background(), launcher); len(got) != 1 || got[0].Name != "sb-1" || got[0].Namespace != "tenant" {
		t.Fatalf("launcher maps to %v", got)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod)
	if pod.Annotations[podmode.AnnotationTarget] != "10.1.2.3" {
		t.Errorf("target %q", pod.Annotations[podmode.AnnotationTarget])
	}
}

func TestUnscheduledStandInWaits(t *testing.T) {
	p := standIn(t)
	p.Spec.NodeName = ""
	r, c, _ := setup(t, p)
	reconcileOnce(t, r, "tenant", "sb-1")
	var list sandboxv1alpha1.SwiftSandboxList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Errorf("%d sandboxes before scheduling", len(list.Items))
	}
}

func TestNamespaceNotOptedIn(t *testing.T) {
	p := standIn(t)
	p.Namespace = "other"
	r, c, rec := setup(t, p, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}})
	reconcileOnce(t, r, "other", "sb-1")
	var list sandboxv1alpha1.SwiftSandboxList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Errorf("%d sandboxes for a namespace not opted in", len(list.Items))
	}
	if ev := <-rec.Events; !strings.Contains(ev, "NamespaceNotOptedIn") {
		t.Errorf("event %q", ev)
	}
}

func TestReleaseWaitsForSandbox(t *testing.T) {
	r, c, _ := setup(t, standIn(t))
	reconcileOnce(t, r, "tenant", "sb-1")

	// The SwiftSandbox holds a finalizer of its own, as the real one does.
	var sb sandboxv1alpha1.SwiftSandbox
	key := types.NamespacedName{Namespace: pmNS, Name: sbKey}
	_ = c.Get(context.Background(), key, &sb)
	sb.Finalizers = []string{"sandbox.kubeswift.io/cleanup"}
	if err := c.Update(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod)
	if err := c.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	if res := reconcileOnce(t, r, "tenant", "sb-1"); res.RequeueAfter == 0 {
		t.Error("released before the SwiftSandbox was gone")
	}
	_ = c.Get(context.Background(), key, &sb)
	if sb.DeletionTimestamp == nil {
		t.Fatal("SwiftSandbox not deleted")
	}
	sb.Finalizers = nil
	if err := c.Update(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("stand-in still there (finalizers %v): %v", pod.Finalizers, err)
	}
}

func TestTerminalStandInReleases(t *testing.T) {
	p := standIn(t)
	r, c, _ := setup(t, p)
	reconcileOnce(t, r, "tenant", "sb-1")
	var pod corev1.Pod
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod)
	pod.Status.Phase = corev1.PodFailed
	if err := c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	// Deletes the SwiftSandbox, then (next pass) drops the finalizer.
	reconcileOnce(t, r, "tenant", "sb-1")
	reconcileOnce(t, r, "tenant", "sb-1")
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod)
	if len(pod.Finalizers) != 0 {
		t.Errorf("finalizers %v on a Failed stand-in whose sandbox is gone", pod.Finalizers)
	}
}

func TestTamperedStandInDeleted(t *testing.T) {
	p := standIn(t)
	p.Spec.Containers[0].Image = "docker.io/library/busybox:1.37"
	r, c, rec := setup(t, p)
	reconcileOnce(t, r, "tenant", "sb-1")
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("tampered stand-in not deleted: %v", err)
	}
	if ev := <-rec.Events; !strings.Contains(ev, "StandInTampered") {
		t.Errorf("event %q", ev)
	}
}

func TestOrphanAndStaleSandboxesDeleted(t *testing.T) {
	orphan := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{
		Name: SandboxPrefix + "old", Namespace: pmNS,
		Labels:      map[string]string{podmode.LabelUID: "old", podmode.LabelStandInNamespace: "tenant"},
		Annotations: map[string]string{podmode.AnnotationStandIn: "tenant/sb-1"},
	}}
	unrelated := &sandboxv1alpha1.SwiftSandbox{ObjectMeta: metav1.ObjectMeta{
		Name: SandboxPrefix + "keep", Namespace: pmNS,
		Labels:      map[string]string{podmode.LabelUID: "keep", podmode.LabelStandInNamespace: "tenant"},
		Annotations: map[string]string{podmode.AnnotationStandIn: "tenant/sb-2"},
	}}
	// Stand-in gone: its SwiftSandbox goes, its neighbour's stays.
	r, c, _ := setup(t, orphan, unrelated)
	reconcileOnce(t, r, "tenant", "sb-1")
	var list sandboxv1alpha1.SwiftSandboxList
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 1 || list.Items[0].Name != unrelated.Name {
		t.Fatalf("left %v", list.Items)
	}

	// A new Pod of the same name: the earlier UID's SwiftSandbox goes.
	r, c, _ = setup(t, standIn(t), orphan.DeepCopy())
	reconcileOnce(t, r, "tenant", "sb-1")
	list = sandboxv1alpha1.SwiftSandboxList{}
	_ = c.List(context.Background(), &list)
	if len(list.Items) != 1 || list.Items[0].Name != sbKey {
		t.Errorf("left %v", list.Items)
	}
	if got := r.mapSandbox(context.Background(), &list.Items[0]); len(got) != 1 || got[0].Name != "sb-1" {
		t.Errorf("sandbox maps to %v", got)
	}
}

func TestEgressPolicyWithoutPortsDeniesAll(t *testing.T) {
	np := EgressPolicyFor(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "tenant", UID: uid}},
		&podmode.Workload{}, pmNS, nil)
	if len(np.Spec.Egress) != 0 || len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("policy %+v", np.Spec)
	}
	// With the relay, the only egress is to the relay's pods and port.
	relay := &RelayPeer{Namespace: "kubeswift-system", Labels: map[string]string{"app": "cm"}, Port: 9443}
	np = EgressPolicyFor(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "tenant", UID: uid}},
		&podmode.Workload{}, pmNS, relay)
	if len(np.Spec.Egress) != 1 || np.Spec.Egress[0].Ports[0].Port.IntVal != 9443 ||
		np.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != "kubeswift-system" ||
		np.Spec.Egress[0].To[0].PodSelector.MatchLabels["app"] != "cm" {
		t.Errorf("relay policy %+v", np.Spec.Egress)
	}
	sb, err := SandboxFor(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "tenant", UID: uid}},
		&podmode.Workload{Image: "i", CPU: 1, Memory: "512Mi"}, nil, pmNS, "h")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Spec.Network.Ingress != nil {
		t.Error("ingress without ports (the SwiftSandbox webhook refuses it)")
	}
}

func TestSplitStandIn(t *testing.T) {
	for in, ok := range map[string]bool{"a/b": true, "/b": false, "a/": false, "ab": false, "": false} {
		if _, _, got := splitStandIn(in); got != ok {
			t.Errorf("splitStandIn(%q) ok=%v", in, got)
		}
	}
}

func TestLinkWritesRelay(t *testing.T) {
	r, c, _ := setup(t, standIn(t))
	r.Relay = func(context.Context) (*podmode.Relay, error) {
		return &podmode.Relay{Addr: "10.96.0.9:443", ServerName: "kubeswift-webhook-service.kubeswift-system.svc", CA: "PEM"}, nil
	}
	r.RelayPeer = &RelayPeer{Namespace: "kubeswift-system", Labels: map[string]string{"app": "cm"}, Port: 9443}
	reconcileOnce(t, r, "tenant", "sb-1")
	reconcileOnce(t, r, "tenant", "sb-1")

	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: "sb-1"}, &pod); err != nil {
		t.Fatal(err)
	}
	if got := pod.Annotations[podmode.AnnotationRelay]; got != `{"addr":"10.96.0.9:443","serverName":"kubeswift-webhook-service.kubeswift-system.svc","ca":"PEM"}` {
		t.Errorf("relay annotation %q", got)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: NetworkPolicyPrefix + uid}, &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Egress) != 2 {
		t.Errorf("egress rules %d, want relay + launcher", len(np.Spec.Egress))
	}

	// A policy from before the relay (PR 1) is brought up to date.
	np.Spec.Egress = np.Spec.Egress[1:]
	if err := c.Update(context.Background(), &np); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r, "tenant", "sb-1")
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(&np), &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Egress) != 2 {
		t.Errorf("egress rules after update %d", len(np.Spec.Egress))
	}
}
