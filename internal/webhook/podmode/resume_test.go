package podmode

import (
	"context"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/sandbox/podmode"
	"github.com/kubeswift-io/kubeswift/internal/scheme"
)

const ownerUID = "5a1b2c3d-0000-4000-8000-00000000a001"

// ownedPod is a pod-mode Pod of agent-sandbox Sandbox p.
func ownedPod() *corev1.Pod {
	p := pod(ptr.To(podmode.RuntimeClassName))
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "agents.x-k8s.io/v1beta1", Kind: podmode.AgentSandboxKind,
		Name: "p", UID: ownerUID, Controller: ptr.To(true)}}
	return p
}

// suspendedSandbox is a microVM suspended on node for stand-in tenant/p of
// that Sandbox.
func suspendedSandbox(node string) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "pm-old", Namespace: "kubeswift-sandboxes",
			Labels:      map[string]string{podmode.LabelStandInNamespace: "tenant", podmode.LabelOwnerUID: ownerUID},
			Annotations: map[string]string{podmode.AnnotationStandIn: "tenant/p"}},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{OperatingMode: sandboxv1alpha1.SandboxOperatingSuspended},
		Status: sandboxv1alpha1.SwiftSandboxStatus{Phase: sandboxv1alpha1.SwiftSandboxSuspended,
			SuspendedState: &sandboxv1alpha1.SandboxSuspendedState{NodeName: node}},
	}
}

func pinningHandler(objs ...client.Object) *Handler {
	h := handler()
	h.Namespace = "kubeswift-sandboxes"
	h.Reader = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).
		WithStatusSubresource(&sandboxv1alpha1.SwiftSandbox{}).Build()
	return h
}

func pinnedTo(t *testing.T, h *Handler, p *corev1.Pod) string {
	t.Helper()
	resp := h.Handle(context.Background(), request(t, admissionv1.Create, p))
	if !resp.Allowed {
		t.Fatalf("refused: %v", resp.Result)
	}
	for _, op := range resp.Patches {
		if op.Path == "/spec/affinity" {
			a, _ := op.Value.(map[string]any)
			na, _ := a["nodeAffinity"].(map[string]any)
			req, _ := na["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
			terms, _ := req["nodeSelectorTerms"].([]any)
			if len(terms) != 1 {
				t.Fatalf("terms %v", terms)
			}
			fields, _ := terms[0].(map[string]any)["matchFields"].([]any)
			f, _ := fields[0].(map[string]any)
			vals, _ := f["values"].([]any)
			if f["key"] != "metadata.name" || len(vals) != 1 {
				t.Fatalf("matchFields %v", fields)
			}
			return vals[0].(string)
		}
	}
	return ""
}

func TestResumingPodPinnedToTheSuspendedNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	if got := pinnedTo(t, pinningHandler(node, suspendedSandbox("node-a")), ownedPod()); got != "node-a" {
		t.Errorf("pinned to %q, want node-a", got)
	}
}

func TestNoPinWithoutAMatchingSuspendedMicroVM(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	other := suspendedSandbox("node-a")
	other.Annotations[podmode.AnnotationStandIn] = "tenant/other"
	running := suspendedSandbox("node-a")
	running.Name = "pm-running"
	running.Status.Phase = sandboxv1alpha1.SwiftSandboxRunning
	for name, c := range map[string]struct {
		h *Handler
		p *corev1.Pod
	}{
		"not owned by a Sandbox": {pinningHandler(node, suspendedSandbox("node-a")), pod(ptr.To(podmode.RuntimeClassName))},
		"another stand-in's":     {pinningHandler(node, other), ownedPod()},
		"not suspended":          {pinningHandler(node, running), ownedPod()},
		"node gone (cold boot)":  {pinningHandler(suspendedSandbox("node-a")), ownedPod()},
		"no reader":              {handler(), ownedPod()},
	} {
		if got := pinnedTo(t, c.h, c.p); got != "" {
			t.Errorf("%s: pinned to %q", name, got)
		}
	}
}
