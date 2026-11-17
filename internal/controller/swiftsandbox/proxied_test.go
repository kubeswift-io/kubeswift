package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func containerEnv(cs []corev1.Container, name string) map[string]string {
	for _, c := range cs {
		if c.Name == name {
			m := map[string]string{}
			for _, e := range c.Env {
				m[e.Name] = e.Value
			}
			return m
		}
	}
	return nil
}

func proxiedSandbox() *sandboxv1alpha1.SwiftSandbox {
	sb := plainSandbox("busybox:1")
	sb.Spec.Network = sandboxv1alpha1.SandboxNetwork{
		Mode: sandboxv1alpha1.SandboxNetworkProxied,
		Egress: &sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{
			{Host: "api.example.com", Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 443}, {Port: 8443}}},
			{Host: "*.example.org"},
			{Service: &sandboxv1alpha1.SandboxEgressService{Name: "llm", Namespace: "ai"}},
		}},
	}
	setEgressAllowed(sb, []sandboxv1alpha1.SandboxEgressAllowed{
		{CIDR: "10.96.0.12/32", Protocol: corev1.ProtocolTCP, Port: 8000, From: "service ai/llm"},
	})
	return sb
}

// A proxied launcher pod: network-init and the launcher get the mode, the
// broker's port and uid, the resolved CIDR rules and the host rules; the pod
// carries the platform deny list.
func TestBuildPod_Proxied(t *testing.T) {
	pod := buildPod(proxiedSandbox(), "sandbox")
	want := map[string]string{
		"KUBESWIFT_SANDBOX_EGRESS":          "proxied",
		"KUBESWIFT_EGRESS_BROKER_PORT":      "15001",
		"KUBESWIFT_EGRESS_PROXY_UID":        "65531",
		"KUBESWIFT_SANDBOX_EGRESS_ALLOW":    "tcp,10.96.0.12/32,8000",
		"KUBESWIFT_SANDBOX_EGRESS_HOSTS":    "*.example.org,0 api.example.com,443 api.example.com,8443",
		"KUBESWIFT_SANDBOX_EGRESS_SERVICES": "llm.ai",
	}
	for _, c := range []struct {
		name string
		env  map[string]string
	}{
		{"network-init", containerEnv(pod.Spec.InitContainers, networkInitName)},
		{"launcher", containerEnv(pod.Spec.Containers, launcherName)},
	} {
		if c.env == nil {
			t.Fatalf("%s container missing", c.name)
		}
		for k, v := range want {
			if c.env[k] != v {
				t.Errorf("%s: %s = %q, want %q", c.name, k, c.env[k], v)
			}
		}
	}
	if _, ok := pod.Annotations[EgressDenyAnnotation]; !ok {
		t.Error("proxied launcher has no egress deny list")
	}

	// A restricted launcher gets none of the proxied environment.
	plain := buildPod(plainSandbox("busybox:1"), "sandbox")
	if env := containerEnv(plain.Spec.Containers, launcherName); env["KUBESWIFT_SANDBOX_EGRESS"] != "" || env["KUBESWIFT_EGRESS_BROKER_PORT"] != "" {
		t.Errorf("restricted launcher got proxied env: %v", env)
	}
}

// Host rules are part of a pool's slot shape.
func TestEgressKey_Host(t *testing.T) {
	k := egressKey("default", proxiedSandbox().Spec.Network)
	joined := strings.Join(k, "|")
	for _, want := range []string{"host api.example.com tcp/443,tcp/8443", "host *.example.org all ports", "service ai/llm all ports"} {
		if !strings.Contains(joined, want) {
			t.Errorf("egressKey %v lacks %q", k, want)
		}
	}
}
