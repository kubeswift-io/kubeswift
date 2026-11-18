package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// available turns the proxied runtime switch on for one test.
func available(t *testing.T) {
	t.Helper()
	saved := []bool{ProxiedAvailable, EgressProxyAvailable, TLSOriginationAvailable, PresetsAvailable, CredentialsAvailable, ServiceUpstreamsAvailable, HTTPListenerAvailable}
	savedImages, savedPresets, savedSPIFFE := AllowedProxyImages, EgressProxyPresets, SPIFFE
	SPIFFE = &SPIFFEConfig{TrustDomain: "kubeswift.test", CSIDriver: "csi.spiffe.io", SocketName: "spire-agent.sock"}
	ready := &EgressProxyReadiness{}
	ready.HTTPGet.Port, ready.HTTPGet.Path = 15000, "/certs"
	EgressProxyPresets = map[string]EgressProxyPreset{
		"envoy":    {Image: "docker.io/envoyproxy/envoy:v1.36.2", Readiness: ready},
		"no-probe": {Image: "docker.io/envoyproxy/envoy:v1.36.2"},
	}
	ProxiedAvailable, EgressProxyAvailable, TLSOriginationAvailable = true, true, true
	PresetsAvailable, CredentialsAvailable, ServiceUpstreamsAvailable, HTTPListenerAvailable = true, true, true, true
	AllowedProxyImages = []string{"*", "*/*", "*/*/*"}
	t.Cleanup(func() {
		ProxiedAvailable, EgressProxyAvailable, TLSOriginationAvailable = saved[0], saved[1], saved[2]
		PresetsAvailable, CredentialsAvailable, ServiceUpstreamsAvailable, HTTPListenerAvailable = saved[3], saved[4], saved[5], saved[6]
		AllowedProxyImages, EgressProxyPresets, SPIFFE = savedImages, savedPresets, savedSPIFFE
	})
}

func proxied(e *sandboxv1alpha1.SandboxEgress) sandboxv1alpha1.SandboxNetwork {
	return sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkProxied, Egress: e}
}

func tcp(p int32) []sandboxv1alpha1.SandboxEgressPort {
	return []sandboxv1alpha1.SandboxEgressPort{{Port: p}}
}

func cred(dest ...string) sandboxv1alpha1.SandboxEgressCredential {
	return sandboxv1alpha1.SandboxEgressCredential{
		Name:         "API_KEY",
		SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "api"}, Key: "token"},
		Destinations: dest,
	}
}

func preset() *sandboxv1alpha1.SandboxEgressProxy {
	return &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy"}
}

// Mode proxied with the built-in broker, an allowed proxy image or a defined
// preset is accepted; an unknown preset is refused; credentials, service
// upstreams and the HTTP listener are refused, with that reason only, until
// their runtime ships.
func TestProxyNotAvailableYet(t *testing.T) {
	saved := AllowedProxyImages
	AllowedProxyImages = []string{"docker.io/envoyproxy/envoy:*"}
	t.Cleanup(func() { AllowedProxyImages = saved })
	listeners := &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15001}
	ok := []sandboxv1alpha1.SandboxNetwork{
		proxied(&sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com"}}}),
		proxied(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{
			Image: "docker.io/envoyproxy/envoy:v1.36.2", Listeners: listeners,
			Upstreams: []sandboxv1alpha1.SandboxEgressRule{{CIDR: "10.96.0.12/32"}}}}),
		proxied(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{
			Image: "docker.io/envoyproxy/envoy:v1.36.2", Listeners: listeners,
			Upstreams: []sandboxv1alpha1.SandboxEgressRule{{Service: svc("llm", "ai")}}}}),
		proxied(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "docker.io/envoyproxy/envoy:v1.36.2",
			Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15001, HTTP: 15002}}}),
	}
	for i, n := range ok {
		if err := ValidateNetwork(n); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	for _, tc := range []struct {
		name, want string
		n          sandboxv1alpha1.SandboxNetwork
	}{
		{"unknown preset", "is not a preset this cluster defines", proxied(&sandboxv1alpha1.SandboxEgress{Proxy: preset()})},
		{"image not allowed", "is not an egress proxy image this cluster allows", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "docker.io/library/nginx:1", Listeners: listeners}})},
	} {
		if err := ValidateNetwork(tc.n); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
	// A real error still wins over the availability refusal.
	bad := proxied(&sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "api"}}, Proxy: preset()})
	if err := ValidateNetwork(bad); err == nil || !strings.Contains(err.Error(), "two labels") {
		t.Fatalf("got %v, want the host error first", err)
	}
	exposed := proxied(nil)
	exposed.Ports = []sandboxv1alpha1.SandboxPort{{Name: "web", Port: EgressBrokerPort}}
	if err := ValidateNetwork(exposed); err == nil || !strings.Contains(err.Error(), "egress broker") {
		t.Fatalf("broker port exposed: got %v", err)
	}
	id := &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	if err := ValidateIdentity(id, proxied(nil)); err == nil || !strings.Contains(err.Error(), "SPIFFE on the platform") {
		t.Fatalf("identity: got %v, want the platform refusal", err)
	}
}

func TestValidateNetworkProxied(t *testing.T) {
	available(t)
	listeners := &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15001, DNS: 15053}
	ok := []struct {
		name string
		n    sandboxv1alpha1.SandboxNetwork
	}{
		{"built-in broker, no rules", proxied(nil)},
		{"host, wildcard, cidr and service rules", proxied(&sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{
			{Host: "api.example.com", Ports: tcp(443)},
			{Host: "*.githubusercontent.com"},
			{CIDR: "10.20.0.0/24"},
			{Service: svc("llm", "ai")},
		}})},
		{"preset with upstreams", proxied(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{
			Preset: "envoy", Config: sandboxv1alpha1.SandboxEgressProxyConfigSelf,
			Upstreams: []sandboxv1alpha1.SandboxEgressRule{{CIDR: "0.0.0.0/0"}, {Service: svc("llm", "ai"), Ports: tcp(8080)}},
		}})},
		{"image with listeners, args and a secret env", proxied(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{
			Image: "registry.example/proxy:1", Listeners: listeners, Args: []string{"--x"},
			Env: []corev1.EnvVar{{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}}},
		}})},
		{"credential to an allowed host, default port", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com", Ports: tcp(443)}},
			Proxy:       preset(),
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("api.example.com")},
		})},
		{"credential covered by a wildcard rule", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "*.example.com"}},
			Proxy:       preset(),
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("a.b.example.com:8443", "x.example.com")},
		})},
	}
	for _, tc := range ok {
		if err := ValidateNetwork(tc.n); err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}

	withCred := func(c sandboxv1alpha1.SandboxEgressCredential) sandboxv1alpha1.SandboxNetwork {
		return proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com", Ports: tcp(443)}},
			Proxy:       preset(),
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{c},
		})
	}
	restricted := func(e *sandboxv1alpha1.SandboxEgress) sandboxv1alpha1.SandboxNetwork {
		return sandboxv1alpha1.SandboxNetwork{Egress: e}
	}
	bad := []struct {
		name, want string
		n          sandboxv1alpha1.SandboxNetwork
	}{
		{"host in restricted", "host needs mode proxied", allow(sandboxv1alpha1.SandboxEgressRule{Host: "api.example.com"})},
		{"proxy in restricted", "proxy needs mode proxied", restricted(&sandboxv1alpha1.SandboxEgress{Proxy: preset()})},
		{"credentials in restricted", "credentials need mode proxied", restricted(&sandboxv1alpha1.SandboxEgress{
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("api.example.com")}})},
		{"egress in open", "restricted, proxied or brokered only", sandboxv1alpha1.SandboxNetwork{
			Mode: sandboxv1alpha1.SandboxNetworkOpen, Egress: &sandboxv1alpha1.SandboxEgress{}}},
		{"two destinations in one rule", "service, cidr and host", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "a.example.com", CIDR: "10.0.0.0/8"}}})},
		{"host is an address", "use cidr", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "10.0.0.1"}}})},
		{"single-label host", "two labels", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "llm"}}})},
		{"wildcard inside", "leading", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "a.*.example.com"}}})},
		{"uppercase host", "host \"API.example.com\"", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "API.example.com"}}})},
		{"UDP allow", "no guest UDP", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow: []sandboxv1alpha1.SandboxEgressRule{{CIDR: "10.0.0.0/8", Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 5000, Protocol: corev1.ProtocolUDP}}}}})},
		{"tls in restricted", "tls needs mode proxied", allow(sandboxv1alpha1.SandboxEgressRule{
			CIDR: "10.0.0.1/32", TLS: &sandboxv1alpha1.SandboxEgressTLS{Originate: sandboxv1alpha1.SandboxEgressTLSSPIFFE}})},
		{"preset and image", "exactly one of preset and image", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy", Image: "x"}})},
		{"neither preset nor image", "exactly one of preset and image", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{}})},
		{"preset with listeners", "a preset supplies them", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy", Listeners: listeners}})},
		{"image without listeners", "needs listeners", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "x"}})},
		{"privileged listener port", "out of range 1024-65535", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "x", Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 80}}})},
		{"duplicate listener", "also used by listeners.transparent", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "x", Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15001, HTTP: 15001}}})},
		{"listener on an exposed port", "also used by spec.network.ports web", sandboxv1alpha1.SandboxNetwork{
			Mode:  sandboxv1alpha1.SandboxNetworkProxied,
			Ports: []sandboxv1alpha1.SandboxPort{{Name: "web", Port: 15001}},
			Egress: &sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{
				Image: "x", Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15001}}}}},
		{"env from a ConfigMap", "only valueFrom.secretKeyRef", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "x", Listeners: listeners, Env: []corev1.EnvVar{{Name: "A",
				ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "k"}}}}}})},
		{"host upstream", "host is not valid in upstreams", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy", Upstreams: []sandboxv1alpha1.SandboxEgressRule{{Host: "a.example.com"}}}})},
		{"metadata upstream", "169.254.0.0/16", proxied(&sandboxv1alpha1.SandboxEgress{
			Proxy: &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy", Upstreams: []sandboxv1alpha1.SandboxEgressRule{{CIDR: "169.254.169.254/32"}}}})},
		{"credentials without proxy", "built-in broker does not terminate TLS", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com"}},
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("api.example.com")}})},
		{"credentials with an image proxy", "need a proxy preset", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com"}},
			Proxy:       &sandboxv1alpha1.SandboxEgressProxy{Image: "x", Listeners: listeners},
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("api.example.com")}})},
		{"credentials with a Self preset", "need proxy.config Intent", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com"}},
			Proxy:       &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy", Config: sandboxv1alpha1.SandboxEgressProxyConfigSelf},
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("api.example.com")}})},
		{"destination not allowed", "is not allowed by a host rule", withCred(cred("other.example.com"))},
		{"destination port not allowed", "is not allowed by a host rule", withCred(cred("api.example.com:8443"))},
		{"bad env name", "not an environment variable name", withCred(func() sandboxv1alpha1.SandboxEgressCredential {
			c := cred("api.example.com")
			c.Name = "1BAD"
			return c
		}())},
		{"short placeholder", "16 to 128", withCred(func() sandboxv1alpha1.SandboxEgressCredential {
			c := cred("api.example.com")
			c.Placeholder = "short"
			return c
		}())},
		{"Host header", "cannot be Host", withCred(func() sandboxv1alpha1.SandboxEgressCredential {
			c := cred("api.example.com")
			c.Header = "host"
			return c
		}())},
		{"template without {}", "exactly once", withCred(func() sandboxv1alpha1.SandboxEgressCredential {
			c := cred("api.example.com")
			c.ValueTemplate = "Bearer"
			return c
		}())},
		{"no secret key", "needs a name and a key", withCred(func() sandboxv1alpha1.SandboxEgressCredential {
			c := cred("api.example.com")
			c.SecretKeyRef.Key = ""
			return c
		}())},
		{"wildcard destination", "not a wildcard", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "*.example.com"}},
			Proxy:       preset(),
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("*.example.com")},
		})},
		{"preset without readiness", "no readiness probe", proxied(&sandboxv1alpha1.SandboxEgress{
			Allow:       []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com"}},
			Proxy:       &sandboxv1alpha1.SandboxEgressProxy{Preset: "no-probe"},
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("api.example.com")},
		})},
	}
	for _, tc := range bad {
		err := ValidateNetwork(tc.n)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}

	dup := withCred(cred("api.example.com"))
	dup.Egress.Credentials = append(dup.Egress.Credentials, cred("api.example.com"))
	if err := ValidateNetwork(dup); err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("duplicate credential: got %v", err)
	}
}

func TestValidateIdentity(t *testing.T) {
	available(t)
	on := &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	tlsRule := []sandboxv1alpha1.SandboxEgressRule{{CIDR: "10.0.0.1/32",
		TLS: &sandboxv1alpha1.SandboxEgressTLS{Originate: sandboxv1alpha1.SandboxEgressTLSSPIFFE}}}
	withProxy := &sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "docker.io/envoyproxy/envoy:v1.36.2"}}
	if err := ValidateIdentity(on, proxied(withProxy)); err != nil {
		t.Errorf("enabled with a proxy: %v", err)
	}
	if err := ValidateIdentity(on, proxied(&sandboxv1alpha1.SandboxEgress{Allow: tlsRule})); err != nil {
		t.Errorf("broker with a tls rule: %v", err)
	}
	if err := ValidateIdentity(on, proxied(nil)); err != nil {
		t.Errorf("broker without tls rules: %v", err)
	}
	withTLS := *withProxy
	withTLS.Allow = tlsRule
	if err := ValidateIdentity(on, proxied(&withTLS)); err == nil || !strings.Contains(err.Error(), "config Intent") {
		t.Errorf("tls rule with a Self proxy: got %v", err)
	}
	intent := withTLS
	ip := *withProxy.Proxy
	ip.Config = sandboxv1alpha1.SandboxEgressProxyConfigIntent
	intent.Proxy = &ip
	if err := ValidateIdentity(on, proxied(&intent)); err != nil {
		t.Errorf("tls rule with an Intent proxy: %v", err)
	}
	preset := withTLS
	preset.Proxy = &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy"}
	if err := ValidateIdentity(on, proxied(&preset)); err != nil {
		t.Errorf("tls rule with a preset (Intent by default): %v", err)
	}
	TLSOriginationAvailable = false
	if err := ValidateIdentity(on, proxied(&sandboxv1alpha1.SandboxEgress{Allow: tlsRule})); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("tls switched off: got %v", err)
	}
	if err := ValidateIdentity(nil, sandboxv1alpha1.SandboxNetwork{}); err != nil {
		t.Errorf("unset: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		id         *sandboxv1alpha1.SandboxIdentity
		n          sandboxv1alpha1.SandboxNetwork
	}{
		{"tls without identity", "needs spec.identity.spiffe Enabled", nil, proxied(&sandboxv1alpha1.SandboxEgress{Allow: tlsRule})},
		{"enabled in restricted", "needs network mode proxied", on, sandboxv1alpha1.SandboxNetwork{}},
		{"bad value", "not Enabled or Disabled", &sandboxv1alpha1.SandboxIdentity{SPIFFE: "On"}, proxied(nil)},
	} {
		if err := ValidateIdentity(tc.id, tc.n); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestParseProxyPresets(t *testing.T) {
	got, err := ParseProxyPresets(`{"envoy":{"image":"docker.io/envoyproxy/envoy:v1.36.2","args":["-c","/etc/kubeswift/egress-proxy/envoy.json"],
		"translatorImage":"ghcr.io/kubeswift-io/kubeswift-egress-adapters/envoy-translator:v0.1.0","translatorArgs":["--out","/etc/kubeswift/egress-proxy/envoy.json"],
		"listeners":{"transparent":15006},"readiness":{"httpGet":{"port":15000,"path":"/certs"}}}}`)
	if err != nil || got["envoy"].Listeners.Transparent != 15006 || got["envoy"].TranslatorImage == "" ||
		got["envoy"].Readiness == nil || got["envoy"].Readiness.HTTPGet.Port != 15000 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := ParseProxyPresets(""); err != nil || got != nil {
		t.Errorf("empty: %v %v", got, err)
	}
	for name, in := range map[string]string{
		"bad name":                `{"Envoy":{"image":"x","listeners":{"transparent":15006}}}`,
		"no image":                `{"envoy":{"listeners":{"transparent":15006}}}`,
		"privileged port":         `{"envoy":{"image":"x","listeners":{"transparent":80}}}`,
		"same ports":              `{"envoy":{"image":"x","listeners":{"transparent":15006,"dns":15006}}}`,
		"unknown field":           `{"envoy":{"image":"x","listeners":{"transparent":15006},"command":["sh"]}}`,
		"not json":                `envoy`,
		"readiness on a listener": `{"envoy":{"image":"x","listeners":{"transparent":15006},"readiness":{"httpGet":{"port":15006,"path":"/certs"}}}}`,
		"http on transparent":     `{"envoy":{"image":"x","listeners":{"transparent":15006,"http":15006}}}`,
		"readiness on http":       `{"envoy":{"image":"x","listeners":{"transparent":15006,"http":15002},"readiness":{"httpGet":{"port":15002,"path":"/certs"}}}}`,
		"readiness relative path": `{"envoy":{"image":"x","listeners":{"transparent":15006},"readiness":{"httpGet":{"port":15000,"path":"certs"}}}}`,
	} {
		if _, err := ParseProxyPresets(in); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
	// A preset's listener cannot be an exposed port.
	saved := EgressProxyPresets
	EgressProxyPresets = map[string]EgressProxyPreset{"envoy": {Image: "x"}}
	EgressProxyPresets["envoy"] = func() EgressProxyPreset { p := EgressProxyPresets["envoy"]; p.Listeners.Transparent = 15006; return p }()
	t.Cleanup(func() { EgressProxyPresets = saved })
	n := proxied(&sandboxv1alpha1.SandboxEgress{Proxy: preset()})
	n.Ports = []sandboxv1alpha1.SandboxPort{{Name: "web", Port: 15006}}
	if err := ValidateNetwork(n); err == nil || !strings.Contains(err.Error(), "listener of egress proxy preset") {
		t.Errorf("preset listener exposed: got %v", err)
	}
}

func TestValidateIdentityPlatformOff(t *testing.T) {
	available(t)
	SPIFFE = nil
	on := &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	p := proxied(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "docker.io/envoyproxy/envoy:v1.36.2"}})
	if err := ValidateIdentity(on, p); err == nil || !strings.Contains(err.Error(), "sandboxMaterialize.spiffe.enabled") {
		t.Errorf("got %v, want the platform refusal", err)
	}
}

func TestParseSPIFFE(t *testing.T) {
	if c, err := ParseSPIFFE(""); c != nil || err != nil {
		t.Errorf("empty: %v %v", c, err)
	}
	c, err := ParseSPIFFE(`{"trustDomain":"example.org","csiDriver":"csi.spiffe.io","socketName":"spire-agent.sock"}`)
	if err != nil || c.TrustDomain != "example.org" {
		t.Errorf("csi: %+v %v", c, err)
	}
	if _, err := ParseSPIFFE(`{"trustDomain":"example.org","hostPath":"/run/spire/agent-sockets","socketName":"spire-agent.sock"}`); err != nil {
		t.Errorf("hostPath: %v", err)
	}
	for _, bad := range []string{
		`{"trustDomain":"Example.org","csiDriver":"x","socketName":"s"}`,
		`{"trustDomain":"example.org","csiDriver":"x","socketName":"a/b"}`,
		`{"trustDomain":"example.org","hostPath":"run/x","socketName":"s"}`,
		`{`,
	} {
		if _, err := ParseSPIFFE(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

// listeners.proxyProtocol: v1 or v2, and port 15001 stays the relay's.
func TestProxyProtocolListeners(t *testing.T) {
	got, err := ParseProxyPresets(`{"agw":{"image":"x","listeners":{"transparent":15006,"proxyProtocol":"v2"}}}`)
	if err != nil || got["agw"].Listeners.ProxyProtocol != "v2" {
		t.Fatalf("got %+v, %v", got, err)
	}
	for name, in := range map[string]string{
		"bad version":            `{"agw":{"image":"x","listeners":{"transparent":15006,"proxyProtocol":"v3"}}}`,
		"transparent on relay":   `{"agw":{"image":"x","listeners":{"transparent":15001,"proxyProtocol":"v1"}}}`,
		"readiness on relay":     `{"agw":{"image":"x","listeners":{"transparent":15006,"proxyProtocol":"v1"},"readiness":{"httpGet":{"port":15001,"path":"/x"}}}}`,
		"http listener on relay": `{"agw":{"image":"x","listeners":{"transparent":15006,"http":15001,"proxyProtocol":"v2"}}}`,
	} {
		if _, err := ParseProxyPresets(in); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
	// Without proxyProtocol a preset may use 15001 (the broker is not there).
	if _, err := ParseProxyPresets(`{"agw":{"image":"x","listeners":{"transparent":15001}}}`); err != nil {
		t.Errorf("15001 without a relay: %v", err)
	}

	saved := EgressProxyPresets
	t.Cleanup(func() { EgressProxyPresets = saved })
	pr := EgressProxyPreset{Image: "x"}
	pr.Listeners.Transparent = 15006
	pr.Listeners.ProxyProtocol = "v2"
	EgressProxyPresets = map[string]EgressProxyPreset{"envoy": pr}
	n := proxied(&sandboxv1alpha1.SandboxEgress{Proxy: preset()})
	if !UsesBrokerPort(n) {
		t.Error("a relayed preset must reserve the relay's port")
	}
	pr.Listeners.ProxyProtocol = ""
	EgressProxyPresets["envoy"] = pr
	if UsesBrokerPort(n) {
		t.Error("a preset without proxyProtocol does not use 15001")
	}
	if !UsesBrokerPort(proxied(&sandboxv1alpha1.SandboxEgress{})) {
		t.Error("the built-in broker uses 15001")
	}
	img := proxied(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{Image: "x",
		Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15001, ProxyProtocol: "v2"}}})
	if err := validateProxy(img); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Errorf("image proxy on the relay's port: %v", err)
	}
}

// Mode brokered takes the same egress API as proxied; a proxy must declare
// listeners.http (the guest's requests arrive as CONNECT), and ports and
// network probes are refused (no guest NIC).
func TestValidateNetworkBrokered(t *testing.T) {
	available(t)
	EgressProxyPresets["agw-http"] = EgressProxyPreset{Image: "ghcr.io/x/agentgateway:1"}
	p := EgressProxyPresets["agw-http"]
	p.Listeners.Transparent, p.Listeners.HTTP = 15002, 15003
	EgressProxyPresets["agw-http"] = p
	brokered := func(e *sandboxv1alpha1.SandboxEgress) sandboxv1alpha1.SandboxNetwork {
		return sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkBrokered, Egress: e}
	}
	ok := []struct {
		name string
		n    sandboxv1alpha1.SandboxNetwork
	}{
		{"built-in broker, no egress", brokered(nil)},
		{"built-in broker with host and cidr rules", brokered(&sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{
			{Host: "api.example.com", Ports: tcp(443)}, {CIDR: "203.0.113.0/24"}}})},
		{"preset with listeners.http", brokered(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{Preset: "agw-http"}})},
		{"image with listeners.http", brokered(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{
			Image: "registry.example/proxy:1", Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15002, HTTP: 15003}}})},
	}
	for _, tc := range ok {
		if err := ValidateNetwork(tc.n); err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}
	bad := []struct {
		name, want string
		n          sandboxv1alpha1.SandboxNetwork
	}{
		{"preset without listeners.http", "declares no listeners.http", brokered(&sandboxv1alpha1.SandboxEgress{Proxy: preset()})},
		{"image without listeners.http", "listeners.http is required", brokered(&sandboxv1alpha1.SandboxEgress{Proxy: &sandboxv1alpha1.SandboxEgressProxy{
			Image: "registry.example/proxy:1", Listeners: &sandboxv1alpha1.SandboxEgressProxyListeners{Transparent: 15002}}})},
		{"ports", "not supported in mode brokered", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkBrokered,
			Ports: []sandboxv1alpha1.SandboxPort{{Name: "http", Port: 8080}}}},
	}
	for _, tc := range bad {
		err := ValidateNetwork(tc.n)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
	on := &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	if err := ValidateIdentity(on, brokered(nil)); err != nil {
		t.Errorf("spiffe in brokered: %v", err)
	}
	probe := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{}}}
	if err := validateProbe(probe, brokered(nil), false); err == nil || !strings.Contains(err.Error(), "brokered") {
		t.Errorf("probe in brokered: got %v", err)
	}
}
