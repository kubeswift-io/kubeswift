package swiftsandbox

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

var httpApp = sandboxv1alpha1.SandboxPort{Name: "http-app", Port: 3000}

func exposedSandbox() *sandboxv1alpha1.SwiftSandbox {
	sb := egSandbox("")
	sb.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{httpApp}
	sb.Spec.PodMetadata = &sandboxv1alpha1.SandboxPodMetadata{
		Labels:      map[string]string{"app": "hello"},
		Annotations: map[string]string{"example.com/owner": "team-a"},
	}
	return sb
}

// A declared port is a named containerPort on the launcher, a DNAT to the
// same guest port in the intent, and the pod carries the sandbox's metadata
// without losing KubeSwift's own label.
func TestBuildPod_ExposesPortsAndCarriesPodMetadata(t *testing.T) {
	sb := exposedSandbox()
	pod := buildPod(sb, "sandbox")
	want := []corev1.ContainerPort{{Name: "http-app", ContainerPort: 3000, Protocol: corev1.ProtocolTCP}}
	if got := pod.Spec.Containers[0].Ports; !reflect.DeepEqual(got, want) {
		t.Errorf("launcher ports = %+v, want %+v", got, want)
	}
	if pod.Labels["app"] != "hello" || pod.Labels[SandboxLabelKey] != sb.Name || pod.Annotations["example.com/owner"] != "team-a" {
		t.Errorf("pod metadata = labels %v annotations %v", pod.Labels, pod.Annotations)
	}
	intent := buildIntent(sb, "sandbox", "/cache/x.ext4", "", execSpec{Argv: []string{"/srv"}}, false)
	if len(intent.Ports) != 1 || intent.Ports[0].Port != 3000 || intent.Ports[0].TargetPort != 3000 || intent.Ports[0].Protocol != "tcp" {
		t.Errorf("intent ports = %+v, want 3000 -> 3000/tcp", intent.Ports)
	}

	sb.Spec.Network.Mode = sandboxv1alpha1.SandboxNetworkNone
	if p := buildPod(sb, "sandbox"); len(p.Spec.Containers[0].Ports) != 0 {
		t.Error("mode none exposes nothing")
	}
}

// KubeSwift's own keys win even if a key slipped past validation.
func TestApplyPodMetadata_NeverOverwrites(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{SandboxLabelKey: "mine"}}}
	applyPodMetadata(pod, &sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{SandboxLabelKey: "theirs", "app": "x"}})
	if pod.Labels[SandboxLabelKey] != "mine" || pod.Labels["app"] != "x" {
		t.Errorf("labels = %v", pod.Labels)
	}
}

// The NetworkPolicy admits exactly the declared ports, from the ingress peers
// when given, and stays deny-all without ports.
func TestBuildNetworkPolicy_Ingress(t *testing.T) {
	if np := buildNetworkPolicy(egSandbox("")); len(np.Spec.Ingress) != 0 {
		t.Errorf("no ports must deny all ingress, got %+v", np.Spec.Ingress)
	}

	sb := exposedSandbox()
	np := buildNetworkPolicy(sb)
	tcp, port := corev1.ProtocolTCP, intstr.FromInt32(3000)
	want := []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}}}
	if !reflect.DeepEqual(np.Spec.Ingress, want) {
		t.Errorf("ingress = %+v, want port 3000 from anywhere", np.Spec.Ingress)
	}
	if len(np.Spec.Egress) != 1 || !reflect.DeepEqual(np.Spec.Egress[0], networkingv1.NetworkPolicyEgressRule{}) {
		t.Errorf("egress must stay allow-all (in-pod iptables does egress), got %+v", np.Spec.Egress)
	}

	from := []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}}}
	sb.Spec.Network.Ingress = &sandboxv1alpha1.SandboxIngress{From: from}
	if np := buildNetworkPolicy(sb); !reflect.DeepEqual(np.Spec.Ingress[0].From, from) {
		t.Errorf("from = %+v, want %+v", np.Spec.Ingress[0].From, from)
	}
}

// With the webhook off, metadata under KubeSwift's domain still never reaches
// a launcher: the sandbox fails before one exists.
func TestReconcile_ReservedPodMetadataFailsBeforeLaunch(t *testing.T) {
	sb := plainSandbox(testImage(t))
	sb.Spec.PodMetadata = &sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{"sandbox.kubeswift.io/slot": "p-slot-x"}}
	r, c := sandboxReconciler(sb, readyKernel("default", defaultKernelProfile))
	reconcileSB(t, r, "sb")
	got := getSandbox(t, c, "sb")
	if got.Status.Phase != sandboxv1alpha1.SwiftSandboxFailed || !strings.Contains(got.Status.Message, "KubeSwift's own") {
		t.Errorf("want Failed naming the reserved key, got %s: %q", got.Status.Phase, got.Status.Message)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "sb"}, &pod); err == nil {
		t.Error("no launcher may be created")
	}
}

// A checkout puts the sandbox's labels on the slot in the same write as the
// claim, so a Service selecting by them picks the slot up only once it is
// this sandbox's.
func TestCheckout_AppliesPodMetadataWithTheClaim(t *testing.T) {
	ctx := context.Background()
	pool := shapedPool()
	pool.Namespace = "default"
	pool.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{httpApp}
	slot := readySlot("p-slot-aaaaa", poolSlotProfile(pool))
	slot.Namespace = "default"
	sb := matchingSandbox()
	sb.Namespace = "default"
	sb.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{httpApp}
	sb.Spec.PodMetadata = &sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{"app": "hello"}}
	r, c := sandboxReconciler(pool, slot, sb)

	reconcileSB(t, r, "sb")
	if got := getSandbox(t, c, "sb"); got.Status.PodRef != slot.Name {
		t.Fatalf("want a checkout of %s, got %q (%s)", slot.Name, got.Status.PodRef, got.Status.Message)
	}
	var p corev1.Pod
	if err := c.Get(ctx, client.ObjectKeyFromObject(slot), &p); err != nil {
		t.Fatal(err)
	}
	if p.Labels["app"] != "hello" || p.Labels[SlotStateLabelKey] != slotStateClaimed {
		t.Errorf("claimed slot labels = %v", p.Labels)
	}
}

// Ports and ingress are part of the slot shape; a pool that sets neither (nor
// an egress allowlist) keeps the profile it had before, so its warm slots are
// not recycled by an upgrade.
func TestSlotShape_PortsAndIngress(t *testing.T) {
	pool := shapedPool()
	pool.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{httpApp}
	pool.Spec.Network.Ingress = &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{
		{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}}}}

	sb := matchingSandbox()
	got := slotMismatches(pool, sb)
	if len(got) != 2 || !strings.HasPrefix(got[0], "network ports (pool http-app:3000/tcp, sandbox none)") ||
		!strings.HasPrefix(got[1], "network ingress (pool [") {
		t.Errorf("mismatches = %q", got)
	}
	sb.Spec.Network.Ports = []sandboxv1alpha1.SandboxPort{{Name: "http-app", Port: 3000, Protocol: corev1.ProtocolTCP}}
	sb.Spec.Network.Ingress = pool.Spec.Network.Ingress.DeepCopy()
	if got := slotMismatches(pool, sb); len(got) != 0 {
		t.Errorf("the same ports and ingress must match, got %q", got)
	}

	plain := shapedPool()
	const before = `{"image":"busybox:1","network":"restricted","verifyKey":"cosign-pub","rootfsMode":"block","kernel":"sandbox","cpu":1,"memory":"512Mi","nodeSelector":{"tier":"trusted","zone":"a"}}`
	if got := poolSlotProfile(plain); got != before {
		t.Errorf("a pool without ports, ingress or egress changed profile:\n got %s\nwant %s", got, before)
	}
}
