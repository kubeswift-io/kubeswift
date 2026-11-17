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

func proxySandbox(cfg sandboxv1alpha1.SandboxEgressProxyConfig) *sandboxv1alpha1.SwiftSandbox {
	sb := proxiedSandbox()
	sb.Spec.Network.Egress.Proxy = &sandboxv1alpha1.SandboxEgressProxy{
		Image:     "docker.io/envoyproxy/envoy:v1.36.2",
		Args:      []string{"--disable-hot-restart"},
		Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15006, DNS: 15053},
		Config:    cfg,
		Upstreams: []sandboxv1alpha1.SandboxEgressRule{{CIDR: "10.96.0.50/32", Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 8080}}}},
	}
	return sb
}

// A proxy container: a native sidecar running as the proxy uid without
// privileges or the launcher's token; guest TCP goes to its transparent
// port, its own reach is upstreams; no broker.
func TestBuildPod_EgressProxy(t *testing.T) {
	pod := buildPod(proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigSelf), "sandbox")
	var proxy *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == EgressProxyContainerName {
			proxy = &pod.Spec.InitContainers[i]
			if i != len(pod.Spec.InitContainers)-1 {
				t.Errorf("proxy is init container %d of %d; it must come after network-init", i, len(pod.Spec.InitContainers))
			}
		}
	}
	if proxy == nil {
		t.Fatal("no egress-proxy container")
	}
	sc := proxy.SecurityContext
	if proxy.RestartPolicy == nil || *proxy.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("proxy is not a native sidecar")
	}
	if *sc.RunAsUser != EgressProxyUID || *sc.Privileged || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Drop) != 1 {
		t.Errorf("proxy security context %+v", sc)
	}
	masked := false
	for _, m := range proxy.VolumeMounts {
		masked = masked || (m.MountPath == serviceAccountPath && m.Name == egressProxyNoToken)
		if m.Name == egressIntentVolume {
			t.Error("config Self mounts the intent file")
		}
	}
	if !masked {
		t.Error("the launcher's service account token is not masked")
	}
	for name, env := range map[string]map[string]string{
		"network-init": containerEnv(pod.Spec.InitContainers, networkInitName),
		"launcher":     containerEnv(pod.Spec.Containers, launcherName),
	} {
		if env["KUBESWIFT_EGRESS_PROXY"] != "container" || env["KUBESWIFT_EGRESS_BROKER_PORT"] != "15006" ||
			env["KUBESWIFT_EGRESS_PROXY_DNS_PORT"] != "15053" || env["KUBESWIFT_SANDBOX_EGRESS_ALLOW"] != "tcp,10.96.0.50/32,8080" {
			t.Errorf("%s env %v", name, env)
		}
	}

	// Config Intent mounts the file, from the intent ConfigMap.
	ipod := buildPod(proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigIntent), "sandbox")
	found := false
	for _, c := range ipod.Spec.InitContainers {
		for _, m := range c.VolumeMounts {
			found = found || (c.Name == EgressProxyContainerName && m.Name == egressIntentVolume && m.MountPath == "/etc/kubeswift/egress")
		}
	}
	if !found {
		t.Error("config Intent: no intent file mount")
	}
}

// The egress-intent file carries host rules as written and resolved rules
// with ports merged, plus the listeners and the owner.
func TestEgressIntentJSON(t *testing.T) {
	sb := proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigIntent)
	cm := buildIntentConfigMap(sb, []byte("{}"))
	if err := addEgressIntent(cm, sb, "SwiftSandboxPool", "agents"); err != nil {
		t.Fatal(err)
	}
	js := cm.Data[egressIntentKey]
	for _, want := range []string{
		`"apiVersion": "egress-intent.kubeswift.io/v1"`,
		`"kind": "SwiftSandboxPool"`, `"name": "agents"`,
		`"transparent": 15006`, `"dns": 15053`,
		`"host": "api.example.com"`, `"host": "*.example.org"`,
		`"cidr": "10.96.0.12/32"`, `"from": "service ai/llm"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("intent lacks %s:\n%s", want, js)
		}
	}
	self := proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigSelf)
	cm2 := buildIntentConfigMap(self, []byte("{}"))
	_ = addEgressIntent(cm2, self, "SwiftSandbox", "sb")
	if _, ok := cm2.Data[egressIntentKey]; ok {
		t.Error("config Self got an intent file")
	}
}
