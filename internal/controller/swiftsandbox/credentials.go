package swiftsandbox

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	egressintentv1 "github.com/kubeswift-io/kubeswift/api/egressintent/v1"
	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// Egress credentials (spec.network.egress.credentials): the guest holds a
// placeholder in an environment variable; the real value only ever reaches a
// memory-backed volume shared by swiftletd and the egress proxy
// (EgressSecretsDir). The proxy terminates the guest's TLS for each
// credential destination with a leaf certificate swiftletd issues from a
// per-launcher CA the guest trusts, and sets the header there. The CA key
// stays in swiftletd's memory.
//
// The directory is filled before the proxy starts (egress-secrets-init: a
// throwaway CA, leaves and empty values, so the proxy can load its
// configuration); swiftletd replaces it as a whole with the real material
// before the workload starts (cold) or at checkout (warm), then waits for the
// preset's readiness probe to list the new certificates.
const (
	// EgressSecretsDir is the directory in swiftletd and in the proxy.
	EgressSecretsDir     = "/run/kubeswift/egress/secrets"
	egressSecretsVolume  = "egress-secrets"
	egressSecretsInit    = "egress-secrets-init"
	envEgressSecrets     = "KUBESWIFT_EGRESS_SECRETS"
	egressCACertGuest    = "/etc/kubeswift/egress/ca.crt"
	egressCABundleGuest  = "/etc/kubeswift/egress/ca-bundle.crt"
	placeholderPrefix    = "ks-"
	placeholderKeyPool   = "pool"
	placeholderKeyCold   = "sandbox"
	defaultCredentialHdr = "Authorization"
)

func credentials(n sandboxv1alpha1.SandboxNetwork) []sandboxv1alpha1.SandboxEgressCredential {
	if !n.Mode.EgressMediated() || n.Egress == nil {
		return nil
	}
	return n.Egress.Credentials
}

// hasCredentials reports whether the sandbox's proxy receives credentials.
func hasCredentials(sb *sandboxv1alpha1.SwiftSandbox) bool {
	return len(credentials(sb.Spec.Network)) > 0 && intentConfig(sb)
}

// credentialHeader is the header and value template with their defaults.
func credentialHeader(c sandboxv1alpha1.SandboxEgressCredential) (header, template string) {
	header = c.Header
	if header == "" {
		header = defaultCredentialHdr
	}
	template = c.ValueTemplate
	if template == "" {
		template = "{}"
		if strings.EqualFold(header, defaultCredentialHdr) {
			template = "Bearer {}"
		}
	}
	return header, template
}

// credentialDest is "host:port" with the default port (validated).
func credentialDest(d string) string {
	if i := strings.LastIndex(d, ":"); i >= 0 {
		return d
	}
	return d + ":443"
}

func destHost(d string) string {
	return d[:strings.LastIndex(d, ":")]
}

// placeholderKey says whose placeholders a launcher uses: a warm slot's are
// its pool's (so every checkout of the pool sees the same ones, and they can
// be part of the slot's static egress-intent file), a cold launcher's its
// sandbox's.
func placeholderKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

// coldPlaceholderKey is the key of a sandbox that runs in its own launcher.
func coldPlaceholderKey(sb *sandboxv1alpha1.SwiftSandbox) string {
	return placeholderKey(placeholderKeyCold, sb.Namespace, sb.Name)
}

// poolPlaceholderKey is the key of a pool's slots.
func poolPlaceholderKey(namespace, pool string) string {
	return placeholderKey(placeholderKeyPool, namespace, pool)
}

// placeholderFor is the value the guest sees: the spec's, else derived from
// key and the variable name. It is not a secret (the guest has it, and it
// means nothing outside the proxy's intercepted destinations); deriving it
// keeps it stable across a pool's slots without storing it.
func placeholderFor(c sandboxv1alpha1.SandboxEgressCredential, key string) string {
	if c.Placeholder != "" {
		return c.Placeholder
	}
	sum := sha256.Sum256([]byte(key + "/" + c.Name))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return placeholderPrefix + strings.ToLower(enc[:32])
}

// credentialHosts are the hosts of every credential destination, sorted:
// leaf certificate i is issued for host i.
func credentialHosts(n sandboxv1alpha1.SandboxNetwork) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range credentials(n) {
		for _, d := range c.Destinations {
			h := destHost(credentialDest(d))
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

func secretsFile(name string) string { return EgressSecretsDir + "/" + name }

func credValueFile(name string) string  { return secretsFile("cred-" + name) }
func credHeaderFile(name string) string { return secretsFile("header-" + name) }
func leafCertFile(i int) string         { return secretsFile("cert-" + strconv.Itoa(i) + ".crt") }
func leafKeyFile(i int) string          { return secretsFile("cert-" + strconv.Itoa(i) + ".key") }

// addCredentialsToIntent adds the credentials, the TLS material and the
// secrets directory to an egress-intent file. Paths only: the files are
// swiftletd's.
func addCredentialsToIntent(in *egressintentv1.EgressIntent, n sandboxv1alpha1.SandboxNetwork, key string) {
	creds := credentials(n)
	if len(creds) == 0 {
		return
	}
	for _, c := range creds {
		header, tmpl := credentialHeader(c)
		dests := make([]string, 0, len(c.Destinations))
		for _, d := range c.Destinations {
			dests = append(dests, credentialDest(d))
		}
		in.Credentials = append(in.Credentials, egressintentv1.Credential{
			Name: c.Name, Placeholder: placeholderFor(c, key), Destinations: dests,
			Header: header, ValueTemplate: tmpl,
			File: credValueFile(c.Name), HeaderValueFile: credHeaderFile(c.Name),
		})
	}
	tls := &egressintentv1.TLS{InterceptCA: egressintentv1.CA{Cert: secretsFile("ca.crt")}}
	for i, h := range credentialHosts(n) {
		tls.Certificates = append(tls.Certificates, egressintentv1.Certificate{Hosts: []string{h}, Cert: leafCertFile(i), Key: leafKeyFile(i)})
	}
	in.TLS = tls
	in.SecretsDir = EgressSecretsDir
}

// egressSecretsSpec is what swiftletd needs to fill EgressSecretsDir, as the
// KUBESWIFT_EGRESS_SECRETS env (JSON) of swiftletd and egress-secrets-init.
// Everything in it is pool shape; secret references arrive with the
// workload (runtimeintent.EgressCredentialRef).
type egressSecretsSpec struct {
	Dir         string                  `json:"dir"`
	GID         int64                   `json:"gid"`
	Hosts       []string                `json:"hosts"`
	Credentials []egressSecretsSpecCred `json:"credentials"`
	Readiness   *egressSecretsReadiness `json:"readiness,omitempty"`
	CACert      string                  `json:"caCert"`
	CABundle    string                  `json:"caBundle"`
}

type egressSecretsSpecCred struct {
	Name          string `json:"name"`
	ValueTemplate string `json:"valueTemplate"`
}

type egressSecretsReadiness struct {
	Port int32  `json:"port"`
	Path string `json:"path"`
}

func egressSecretsJSON(sb *sandboxv1alpha1.SwiftSandbox) string {
	spec := egressSecretsSpec{Dir: EgressSecretsDir, GID: EgressProxyUID, Hosts: credentialHosts(sb.Spec.Network),
		CACert: egressCACertGuest, CABundle: egressCABundleGuest}
	for _, c := range credentials(sb.Spec.Network) {
		_, tmpl := credentialHeader(c)
		spec.Credentials = append(spec.Credentials, egressSecretsSpecCred{Name: c.Name, ValueTemplate: tmpl})
	}
	if p := egressProxy(sb); p != nil && p.Preset != "" {
		if pr, ok := sandboxwebhook.EgressProxyPresets[p.Preset]; ok && pr.Readiness != nil {
			spec.Readiness = &egressSecretsReadiness{Port: pr.Readiness.HTTPGet.Port, Path: pr.Readiness.HTTPGet.Path}
		}
	}
	b, err := json.Marshal(spec)
	if err != nil {
		// Strings, ints and slices of them always marshal.
		panic(err)
	}
	return string(b)
}

// credentialRefs are the Secret references swiftletd reads, with the
// workload (cold: the exec spec; warm: the checkout action).
func credentialRefs(sb *sandboxv1alpha1.SwiftSandbox) []runtimeintent.EgressCredentialRef {
	if !hasCredentials(sb) {
		return nil
	}
	var out []runtimeintent.EgressCredentialRef
	for _, c := range credentials(sb.Spec.Network) {
		out = append(out, runtimeintent.EgressCredentialRef{Name: c.Name, Secret: c.SecretKeyRef.Name, Key: c.SecretKeyRef.Key})
	}
	return out
}

// credentialEnv is what the workload's environment gains: each placeholder,
// and the CA bundle swiftletd writes (the image's bundle plus the sandbox's
// intercept CA) for the TLS stacks that read an environment variable.
func credentialEnv(sb *sandboxv1alpha1.SwiftSandbox, key string) []string {
	if !hasCredentials(sb) {
		return nil
	}
	var out []string
	for _, c := range credentials(sb.Spec.Network) {
		out = append(out, c.Name+"="+placeholderFor(c, key))
	}
	return append(out,
		"SSL_CERT_FILE="+egressCABundleGuest,
		"CURL_CA_BUNDLE="+egressCABundleGuest,
		"REQUESTS_CA_BUNDLE="+egressCABundleGuest,
		"NODE_EXTRA_CA_CERTS="+egressCACertGuest,
	)
}

// credentialsKey is the credentials' shape in a pool's slot shape: names,
// placeholders, destinations, headers and templates. Secret references are
// the checkout's own.
func credentialsKey(n sandboxv1alpha1.SandboxNetwork) string {
	creds := credentials(n)
	if len(creds) == 0 {
		return ""
	}
	type shape struct {
		Name, Placeholder, Header, Template string
		Destinations                        []string
	}
	var out []shape
	for _, c := range creds {
		h, t := credentialHeader(c)
		out = append(out, shape{Name: c.Name, Placeholder: c.Placeholder, Header: h, Template: t, Destinations: c.Destinations})
	}
	b, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// credentialSecretNames are the Secrets the launcher's account may read for
// credentials.
func credentialSecretNames(sb *sandboxv1alpha1.SwiftSandbox) []string {
	var out []string
	for _, r := range credentialRefs(sb) {
		out = append(out, r.Secret)
	}
	return out
}

// addEgressSecrets adds the secrets volume to a launcher whose proxy
// receives credentials: mounted read-write in swiftletd and in
// egress-secrets-init (which fills it before the proxy starts), read-only in
// the proxy. Call after addEgressProxy.
func addEgressSecrets(pod *corev1.Pod, sb *sandboxv1alpha1.SwiftSandbox) {
	if !hasCredentials(sb) || !hasEgressProxy(pod) {
		return
	}
	spec := egressSecretsJSON(sb)
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: egressSecretsVolume, VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)}}})
	mount := corev1.VolumeMount{Name: egressSecretsVolume, MountPath: EgressSecretsDir}
	launcher := &pod.Spec.Containers[0]
	launcher.VolumeMounts = append(launcher.VolumeMounts, mount)
	launcher.Env = append(launcher.Env, corev1.EnvVar{Name: envEgressSecrets, Value: spec})
	init := corev1.Container{
		Name:    egressSecretsInit,
		Image:   launcher.Image,
		Command: []string{"swiftletd", "egress-secrets-init"},
		Env:     []corev1.EnvVar{{Name: envEgressSecrets, Value: spec}},
		// Root only to give the files to the proxy's group.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To(int64(0)),
			RunAsNonRoot:             ptr.To(false),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Privileged:               ptr.To(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"CHOWN"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		VolumeMounts:             []corev1.VolumeMount{mount, {Name: egressProxyNoToken, MountPath: serviceAccountPath, ReadOnly: true}},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	// Before the proxy, which needs the files to load its configuration.
	inits := pod.Spec.InitContainers
	for i := range inits {
		if inits[i].Name == EgressProxyContainerName {
			inits[i].VolumeMounts = append(inits[i].VolumeMounts, corev1.VolumeMount{Name: egressSecretsVolume, MountPath: EgressSecretsDir, ReadOnly: true})
			pod.Spec.InitContainers = append(inits[:i:i], append([]corev1.Container{init}, inits[i:]...)...)
			return
		}
	}
}
