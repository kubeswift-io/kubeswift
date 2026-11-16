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
	prev, prevID := ProxiedAvailable, IdentityAvailable
	ProxiedAvailable, IdentityAvailable = true, true
	t.Cleanup(func() { ProxiedAvailable, IdentityAvailable = prev, prevID })
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

// A valid proxied spec is refused, with that reason only, until the runtime ships.
func TestProxiedNotAvailableYet(t *testing.T) {
	n := proxied(&sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{{Host: "api.example.com"}}})
	err := ValidateNetwork(n)
	if err == nil || !strings.Contains(err.Error(), "proxied is not available") {
		t.Fatalf("got %v, want the not-available refusal", err)
	}
	// A real error still wins over the availability refusal.
	n.Egress.Allow[0].Host = "api"
	if err := ValidateNetwork(n); err == nil || !strings.Contains(err.Error(), "two labels") {
		t.Fatalf("got %v, want the host error first", err)
	}
	id := &sandboxv1alpha1.SandboxIdentity{SPIFFE: sandboxv1alpha1.SandboxIdentitySPIFFEEnabled}
	if err := ValidateIdentity(id, proxied(nil)); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("identity: got %v, want the not-available refusal", err)
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
			Credentials: []sandboxv1alpha1.SandboxEgressCredential{cred("a.b.example.com:8443", "*.x.example.com")},
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
		{"egress in open", "restricted or proxied only", sandboxv1alpha1.SandboxNetwork{
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
	if err := ValidateIdentity(on, proxied(&sandboxv1alpha1.SandboxEgress{Allow: tlsRule})); err != nil {
		t.Errorf("enabled with a tls rule: %v", err)
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
