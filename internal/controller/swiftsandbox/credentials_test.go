package swiftsandbox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	egressintentv1 "github.com/kubeswift-io/kubeswift/api/egressintent/v1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

func readyPreset() sandboxwebhook.EgressProxyPreset {
	p := envoyPreset()
	p.Readiness = &sandboxwebhook.EgressProxyReadiness{}
	p.Readiness.HTTPGet.Port, p.Readiness.HTTPGet.Path = 15000, "/certs"
	return p
}

func credSandbox() *sandboxv1alpha1.SwiftSandbox {
	sb := proxiedSandbox()
	sb.Spec.Network.Egress.Proxy = &sandboxv1alpha1.SandboxEgressProxy{Preset: "envoy"}
	sb.Spec.Network.Egress.Credentials = []sandboxv1alpha1.SandboxEgressCredential{
		{Name: "API_KEY", SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "api"}, Key: "token"},
			Destinations: []string{"api.example.com", "models.example.org:443"}},
		{Name: "ORG", Placeholder: "ks-explicit-placeholder-0001", Header: "X-Org",
			SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "org"}, Key: "id"},
			Destinations: []string{"api.example.com:8443"}},
	}
	return sb
}

func initNames(pod *corev1.Pod) []string {
	var out []string
	for _, c := range pod.Spec.InitContainers {
		out = append(out, c.Name)
	}
	return out
}

// The secrets volume is filled by egress-secrets-init before the proxy
// starts, written by swiftletd and read-only for the proxy; the translator
// and the console never mount it.
func TestBuildPod_Credentials(t *testing.T) {
	withPreset(t, readyPreset())
	sb := credSandbox()
	pod := buildPod(sb, "sandbox")
	names := initNames(pod)
	n := len(names)
	if n < 3 || names[n-3] != EgressTranslatorContainerName || names[n-2] != egressSecretsInit || names[n-1] != EgressProxyContainerName {
		t.Fatalf("init containers %v: want ..., egress-translator, egress-secrets-init, egress-proxy", names)
	}
	mount := func(c corev1.Container) (bool, bool) {
		for _, m := range c.VolumeMounts {
			if m.Name == egressSecretsVolume {
				return true, m.ReadOnly
			}
		}
		return false, false
	}
	for _, c := range append(append([]corev1.Container(nil), pod.Spec.InitContainers...), pod.Spec.Containers...) {
		m, ro := mount(c)
		switch c.Name {
		case egressSecretsInit, launcherName:
			if !m || ro {
				t.Errorf("%s must mount the secrets volume read-write", c.Name)
			}
		case EgressProxyContainerName:
			if !m || !ro {
				t.Errorf("the proxy must mount the secrets volume read-only")
			}
		default:
			if m {
				t.Errorf("%s mounts the secrets volume", c.Name)
			}
		}
	}
	init := pod.Spec.InitContainers[n-2]
	if init.Image != pod.Spec.Containers[0].Image || init.SecurityContext.Privileged == nil || *init.SecurityContext.Privileged {
		t.Errorf("egress-secrets-init: image %s, security %+v", init.Image, init.SecurityContext)
	}
	var spec egressSecretsSpec
	if err := json.Unmarshal([]byte(containerEnv(pod.Spec.Containers, launcherName)[envEgressSecrets]), &spec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec.Hosts, []string{"api.example.com", "models.example.org"}) || spec.GID != EgressProxyUID ||
		spec.Readiness == nil || spec.Readiness.Port != 15000 || spec.Credentials[0].ValueTemplate != "Bearer {}" || spec.Credentials[1].ValueTemplate != "{}" {
		t.Errorf("secrets spec %+v", spec)
	}
}

// The egress-intent file and the workload's environment agree on each
// placeholder; a pool's slots use the pool's placeholders, a cold launcher
// its sandbox's; secret values never appear.
func TestCredentials_IntentAndEnv(t *testing.T) {
	withPreset(t, readyPreset())
	sb := credSandbox()
	js, err := egressIntentJSON(sb, "SwiftSandbox", sb.Name)
	if err != nil {
		t.Fatal(err)
	}
	var in egressintentv1.EgressIntent
	if err := json.Unmarshal([]byte(js), &in); err != nil {
		t.Fatal(err)
	}
	if in.SecretsDir != EgressSecretsDir || in.TLS == nil || len(in.TLS.Certificates) != 2 || in.TLS.InterceptCA.Key != "" {
		t.Fatalf("tls %+v dir %q", in.TLS, in.SecretsDir)
	}
	c := in.Credentials[0]
	if c.Header != "Authorization" || c.ValueTemplate != "Bearer {}" ||
		!reflect.DeepEqual(c.Destinations, []string{"api.example.com:443", "models.example.org:443"}) ||
		c.HeaderValueFile != EgressSecretsDir+"/header-API_KEY" {
		t.Errorf("credential %+v", c)
	}
	if in.Credentials[1].Placeholder != "ks-explicit-placeholder-0001" {
		t.Errorf("explicit placeholder lost: %+v", in.Credentials[1])
	}
	env := credentialEnv(sb, coldPlaceholderKey(sb))
	if !contains(env, "API_KEY="+c.Placeholder) || !contains(env, "SSL_CERT_FILE="+egressCABundleGuest) {
		t.Errorf("env %v does not match the intent's placeholder %s", env, c.Placeholder)
	}
	ri := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{Argv: []string{"sh"}}, false)
	if ri.SandboxExec == nil || !contains(ri.SandboxExec.Env, "API_KEY="+c.Placeholder) || len(ri.SandboxExec.EgressCredentials) != 2 {
		t.Errorf("cold exec %+v", ri.SandboxExec)
	}
	if idle := buildIntent(sb, "sandbox", "/r.ext4", "", execSpec{}, true); idle.SandboxExec != nil {
		t.Error("a warm keeper carries no workload env")
	}
	pjs, _ := egressIntentJSON(sb, "SwiftSandboxPool", "agents")
	var pin egressintentv1.EgressIntent
	_ = json.Unmarshal([]byte(pjs), &pin)
	if pin.Credentials[0].Placeholder == c.Placeholder ||
		pin.Credentials[0].Placeholder != placeholderFor(sb.Spec.Network.Egress.Credentials[0], poolPlaceholderKey(sb.Namespace, "agents")) {
		t.Errorf("slot placeholder %s, cold %s", pin.Credentials[0].Placeholder, c.Placeholder)
	}
	if !strings.HasPrefix(c.Placeholder, "ks-") || len(c.Placeholder) != 35 {
		t.Errorf("placeholder %q", c.Placeholder)
	}
}

// Credentials' shape is pool shape; the Secret references are the checkout's.
func TestCredentialsKey(t *testing.T) {
	a := credSandbox().Spec.Network
	b := credSandbox().Spec.Network
	b.Egress.Credentials[0].SecretKeyRef.Name = "another"
	if credentialsKey(a) != credentialsKey(b) {
		t.Error("a different Secret changed the shape")
	}
	b.Egress.Credentials[0].Destinations = []string{"api.example.com"}
	if credentialsKey(a) == credentialsKey(b) {
		t.Error("different destinations, same shape")
	}
	if credentialsKey(proxiedSandbox().Spec.Network) != "" {
		t.Error("no credentials must leave the shape empty")
	}
}

// The launcher's account may read the credential Secrets.
func TestCredentials_SecretNames(t *testing.T) {
	withPreset(t, readyPreset())
	sb := credSandbox()
	if !usesSecrets(sb) || !reflect.DeepEqual(secretNames(sb), []string{"api", "org"}) {
		t.Errorf("secret names %v", secretNames(sb))
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// A checkout sends the Secret references (never values) and the slot's pool
// placeholders.
func TestExecAction_Credentials(t *testing.T) {
	withPreset(t, readyPreset())
	sb := credSandbox()
	slot := &corev1.Pod{}
	slot.Labels = map[string]string{PoolLabelKey: "agents"}
	ann, err := execActionAnnotations(slot, sb, "id", execInject{argv: []string{"sh"}})
	if err != nil {
		t.Fatal(err)
	}
	var args struct {
		Env               []string `json:"env"`
		EgressCredentials []struct {
			Name, Secret, Key string
		} `json:"egressCredentials"`
	}
	if err := json.Unmarshal([]byte(ann[annSandboxExecActionArgs]), &args); err != nil {
		t.Fatal(err)
	}
	want := "API_KEY=" + placeholderFor(sb.Spec.Network.Egress.Credentials[0], poolPlaceholderKey(sb.Namespace, "agents"))
	if !contains(args.Env, want) || len(args.EgressCredentials) != 2 || args.EgressCredentials[0].Secret != "api" {
		t.Errorf("args %+v, want env %s", args, want)
	}
}
