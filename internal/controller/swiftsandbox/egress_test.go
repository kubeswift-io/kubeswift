package swiftsandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func egSandbox(mode sandboxv1alpha1.SandboxNetworkMode) *sandboxv1alpha1.SwiftSandbox {
	return &sandboxv1alpha1.SwiftSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
		Spec: sandboxv1alpha1.SwiftSandboxSpec{
			Image:   "alpine:3.20",
			Network: sandboxv1alpha1.SandboxNetwork{Mode: mode},
		},
	}
}

func TestEgressMode(t *testing.T) {
	// The default (unset) posture is the secure one — restricted.
	if got := egressMode(egSandbox("")); got != sandboxv1alpha1.SandboxNetworkRestricted {
		t.Errorf("default -> %q, want restricted", got)
	}
	if got := egressMode(egSandbox(sandboxv1alpha1.SandboxNetworkRestricted)); got != sandboxv1alpha1.SandboxNetworkRestricted {
		t.Errorf("restricted -> %q", got)
	}
	if got := egressMode(egSandbox(sandboxv1alpha1.SandboxNetworkOpen)); got != sandboxv1alpha1.SandboxNetworkOpen {
		t.Errorf("open -> %q, want open", got)
	}
}

// networkInitEgressEnv returns (KUBESWIFT_SANDBOX_EGRESS, network-init-present).
func networkInitEgressEnv(pod *corev1.Pod) (string, bool) {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name != "network-init" {
			continue
		}
		for _, e := range pod.Spec.InitContainers[i].Env {
			if e.Name == "KUBESWIFT_SANDBOX_EGRESS" {
				return e.Value, true
			}
		}
		return "", true // network-init present but env missing
	}
	return "", false
}

func TestBuildPod_EgressEnv(t *testing.T) {
	// restricted (default): network-init carries the restricted signal.
	if v, ok := networkInitEgressEnv(buildPod(egSandbox(""), "sandbox")); !ok || v != "restricted" {
		t.Errorf("default egress env = %q (network-init present=%v), want restricted", v, ok)
	}
	// open: network-init carries open (so the script installs no egress DROP rules).
	if v, ok := networkInitEgressEnv(buildPod(egSandbox(sandboxv1alpha1.SandboxNetworkOpen), "sandbox")); !ok || v != "open" {
		t.Errorf("open egress env = %q (network-init present=%v), want open", v, ok)
	}
	// none: no network-init container at all (no pod network).
	for i := range buildPod(egSandbox(sandboxv1alpha1.SandboxNetworkNone), "sandbox").Spec.InitContainers {
		if buildPod(egSandbox(sandboxv1alpha1.SandboxNetworkNone), "sandbox").Spec.InitContainers[i].Name == "network-init" {
			t.Error("network:none must not get a network-init container")
		}
	}
}
