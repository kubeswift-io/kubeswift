package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
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
	// As the controller resolves it before building the pod.
	setProxyUpstreams(sb, []sandboxv1alpha1.SandboxEgressAllowed{
		{CIDR: "10.96.0.50/32", Protocol: corev1.ProtocolTCP, Port: 8080, From: "cidr 10.96.0.50/32"},
	})
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

func withPreset(t *testing.T, p sandboxwebhook.EgressProxyPreset) {
	t.Helper()
	saved := sandboxwebhook.EgressProxyPresets
	sandboxwebhook.EgressProxyPresets = map[string]sandboxwebhook.EgressProxyPreset{"envoy": p}
	t.Cleanup(func() { sandboxwebhook.EgressProxyPresets = saved })
}

func envoyPreset() sandboxwebhook.EgressProxyPreset {
	p := sandboxwebhook.EgressProxyPreset{
		Image:           "docker.io/envoyproxy/envoy:v1.36.2",
		Args:            []string{"-c", "/etc/kubeswift/egress-proxy/envoy.json", "--disable-hot-restart"},
		TranslatorImage: "ghcr.io/kubeswift-io/kubeswift-egress-adapters/envoy-translator:v0.1.0",
		TranslatorArgs:  []string{"--out", "/etc/kubeswift/egress-proxy/envoy.json"},
	}
	p.Listeners.Transparent = 15006
	return p
}

// A preset runs its translator once, after network-init and before the
// proxy, with the intent file and a config volume the proxy reads.
func TestBuildPod_Preset(t *testing.T) {
	withPreset(t, envoyPreset())
	sb := proxiedSandbox()
	sb.Spec.Network.Egress.Proxy = &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy"}
	pod := buildPod(sb, "sandbox")
	n := len(pod.Spec.InitContainers)
	if n < 2 || pod.Spec.InitContainers[n-2].Name != EgressTranslatorContainerName || pod.Spec.InitContainers[n-1].Name != EgressProxyContainerName {
		var names []string
		for _, c := range pod.Spec.InitContainers {
			names = append(names, c.Name)
		}
		t.Fatalf("init containers %v: want ..., egress-translator, egress-proxy", names)
	}
	tr, px := pod.Spec.InitContainers[n-2], pod.Spec.InitContainers[n-1]
	if tr.RestartPolicy != nil {
		t.Error("the translator is a sidecar; it must run once")
	}
	if px.Image != "docker.io/envoyproxy/envoy:v1.36.2" || px.Args[1] != "/etc/kubeswift/egress-proxy/envoy.json" {
		t.Errorf("proxy %s %v", px.Image, px.Args)
	}
	ro := func(c corev1.Container, vol string) (mounted, readOnly bool) {
		for _, m := range c.VolumeMounts {
			if m.Name == vol {
				return true, m.ReadOnly
			}
		}
		return false, false
	}
	if m, r := ro(tr, egressProxyConfigVolume); !m || r {
		t.Error("translator must mount the config volume read-write")
	}
	if m, r := ro(px, egressProxyConfigVolume); !m || !r {
		t.Error("proxy must mount the config volume read-only")
	}
	if m, _ := ro(tr, egressIntentVolume); !m {
		t.Error("translator has no intent file")
	}
	if *tr.SecurityContext.RunAsUser != EgressProxyUID || *tr.SecurityContext.Privileged {
		t.Errorf("translator security context %+v", tr.SecurityContext)
	}
	if env := containerEnv(pod.Spec.Containers, launcherName); env["KUBESWIFT_EGRESS_BROKER_PORT"] != "15006" || env["KUBESWIFT_EGRESS_PROXY"] != "container" {
		t.Errorf("launcher env %v", env)
	}
	if _, image, ok := intentReaderRequest(sb); !ok || image != envoyPreset().TranslatorImage {
		t.Errorf("intent reader %q %v, want the translator", image, ok)
	}
}

// A changed preset definition changes the slot shape, so warm slots booted
// with the old one are recycled.
func TestProxyKey_PresetDefinition(t *testing.T) {
	n := proxiedSandbox().Spec.Network
	n.Egress.Proxy = &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy"}
	withPreset(t, envoyPreset())
	before := proxyKey(n)
	p := envoyPreset()
	p.Image = "docker.io/envoyproxy/envoy:v1.36.3"
	withPreset(t, p)
	if after := proxyKey(n); after == before || before == "" {
		t.Errorf("proxy key did not change with the preset image: %q", before)
	}
}

func TestIntentVersionVerdict(t *testing.T) {
	if r, _ := intentVersionVerdict("x", map[string]string{"io.kubeswift.egress-intent.versions": "v1"}); r != "" {
		t.Errorf("v1: %s", r)
	}
	if r, _ := intentVersionVerdict("x", map[string]string{"io.kubeswift.egress-intent.versions": "v0, v1"}); r != "" {
		t.Errorf("v0, v1: %s", r)
	}
	for _, labels := range []map[string]string{nil, {"io.kubeswift.egress-intent.versions": "v2"}} {
		if r, _ := intentVersionVerdict("x", labels); r != reasonEgressIntentUnsupported {
			t.Errorf("%v: %q", labels, r)
		}
	}
}

// A proxied launcher gets the egress-audit container, unprivileged, without
// a token, the audit volume read-only; the launcher writes into it. Other
// modes do not.
func TestBuildPod_EgressAudit(t *testing.T) {
	pod := buildPod(proxiedSandbox(), "sandbox")
	var audit *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == EgressAuditContainerName {
			audit = &pod.Spec.Containers[i]
		}
	}
	if audit == nil {
		t.Fatal("no egress-audit container")
	}
	sc := audit.SecurityContext
	if *sc.Privileged || !*sc.RunAsNonRoot || !*sc.ReadOnlyRootFilesystem || audit.Args[0] != "follow-console" || audit.Args[2] != egressAuditLog {
		t.Errorf("egress-audit %+v %v", sc, audit.Args)
	}
	for _, m := range audit.VolumeMounts {
		if m.Name == egressAuditVolume && !m.ReadOnly {
			t.Error("the audit volume must be read-only in egress-audit")
		}
	}
	if env := containerEnv(pod.Spec.Containers, launcherName); env["KUBESWIFT_EGRESS_AUDIT_DIR"] != egressAuditDir {
		t.Errorf("launcher env %v", env)
	}
	plain := plainSandbox("busybox:1")
	plain.Spec.Network = sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkRestricted}
	for _, c := range buildPod(plain, "sandbox").Spec.Containers {
		if c.Name == EgressAuditContainerName {
			t.Error("a restricted sandbox has an egress-audit container")
		}
	}
}

// listeners.http: network-init and the launcher learn the port, and the
// workload gets the proxy variables pointing at the bridge, cold and warm;
// without it nothing changes.
func TestHTTPListener(t *testing.T) {
	sb := proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigSelf)
	sb.Spec.Network.Egress.Proxy.Listeners.HTTP = 15002
	pod := buildPod(sb, "sandbox")
	for name, env := range map[string]map[string]string{
		"network-init": containerEnv(pod.Spec.InitContainers, networkInitName),
		"launcher":     containerEnv(pod.Spec.Containers, launcherName),
	} {
		if env["KUBESWIFT_EGRESS_PROXY_HTTP_PORT"] != "15002" {
			t.Errorf("%s env %v", name, env)
		}
	}
	want := "HTTPS_PROXY=http://192.168.99.1:15002"
	ri := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{Argv: []string{"sh"}}, false)
	if ri.SandboxExec == nil || !contains(ri.SandboxExec.Env, want) || !contains(ri.SandboxExec.Env, "no_proxy=localhost,127.0.0.1,::1") {
		t.Errorf("cold env %+v", ri.SandboxExec)
	}
	slot := &corev1.Pod{}
	slot.Labels = map[string]string{PoolLabelKey: "p"}
	ann, err := execActionAnnotations(slot, sb, "id", execInject{argv: []string{"sh"}})
	if err != nil || !strings.Contains(ann[annSandboxExecActionArgs], want) {
		t.Errorf("checkout args %v %v", ann, err)
	}
	plain := proxySandbox(sandboxv1alpha1.SandboxEgressProxyConfigSelf)
	if env := httpProxyEnv(plain); env != nil {
		t.Errorf("no http listener, env %v", env)
	}
	if containerEnv(buildPod(plain, "sandbox").Spec.Containers, launcherName)["KUBESWIFT_EGRESS_PROXY_HTTP_PORT"] != "" {
		t.Error("http port env without listeners.http")
	}
}
