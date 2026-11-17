package swiftsandbox

import (
	"encoding/json"
	"fmt"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// ProxiedAvailable reports whether this build runs proxied sandboxes (with
// the built-in broker).
var ProxiedAvailable = true

// EgressProxyAvailable reports whether this build runs an egress proxy
// container from an image (spec.network.egress.proxy.image).
var EgressProxyAvailable = true

// PresetsAvailable, CredentialsAvailable and ServiceUpstreamsAvailable are
// parts of the egress proxy API this build does not run yet. They are
// validated in full and then refused, so a spec written against the API fails
// with its real errors first.
var (
	PresetsAvailable          = true
	CredentialsAvailable      = true
	ServiceUpstreamsAvailable = true
	HTTPListenerAvailable     = true
)

// AllowedProxyImages are the image patterns a proxied sandbox may run as its
// egress proxy (chart sandboxMaterialize.egressProxyImages), path.Match
// patterns where "*" does not cross "/". Empty allows none: the proxy runs in
// the launcher pod, so the administrator chooses which images may.
var AllowedProxyImages []string

// EgressProxyPreset is a proxy the administrator defines in the chart
// (sandboxMaterialize.egressProxyPresets), which a sandbox names with
// spec.network.egress.proxy.preset. Its images need not be in
// AllowedProxyImages: the administrator wrote them.
type EgressProxyPreset struct {
	// Image is the proxy, run as the egress-proxy container.
	Image string   `json:"image"`
	Args  []string `json:"args,omitempty"`
	// TranslatorImage, when set, runs once before the proxy (config Intent):
	// it reads the egress-intent file and writes the proxy's configuration
	// into /etc/kubeswift/egress-proxy, which the proxy reads. Unset, the
	// proxy reads the egress-intent file itself.
	TranslatorImage string   `json:"translatorImage,omitempty"`
	TranslatorArgs  []string `json:"translatorArgs,omitempty"`
	Listeners       struct {
		Transparent int32 `json:"transparent"`
		DNS         int32 `json:"dns,omitempty"`
		// HTTP is an explicit HTTP proxy port the guest's HTTP_PROXY and
		// HTTPS_PROXY point at.
		HTTP int32 `json:"http,omitempty"`
	} `json:"listeners"`
	// Readiness tells swiftletd when the proxy has loaded newly delivered
	// credentials. Only a preset with it can receive credentials.
	Readiness *EgressProxyReadiness `json:"readiness,omitempty"`
}

// EgressProxyReadiness is an HTTP GET swiftletd sends to the proxy on
// 127.0.0.1 after it replaces the credential files: the proxy has loaded them
// once the response (200) contains the serial number, in lowercase hex, of
// every certificate just issued (for Envoy, /certs on its admin port).
type EgressProxyReadiness struct {
	HTTPGet struct {
		Port int32  `json:"port"`
		Path string `json:"path"`
	} `json:"httpGet"`
}

// EgressProxyPresets are the presets this cluster defines, by name.
var EgressProxyPresets map[string]EgressProxyPreset

// ParseProxyPresets reads the presets (JSON object, name -> preset),
// refusing a malformed one.
func ParseProxyPresets(s string) (map[string]EgressProxyPreset, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out map[string]EgressProxyPreset
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("egress proxy presets: %w", err)
	}
	for name, p := range out {
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("egress proxy preset %q: %s", name, errs[0])
		}
		if p.Image == "" {
			return nil, fmt.Errorf("egress proxy preset %q: image is required", name)
		}
		l := p.Listeners
		if l.Transparent < 1024 || l.Transparent > 65535 || (l.DNS != 0 && (l.DNS < 1024 || l.DNS > 65535 || l.DNS == l.Transparent)) ||
			(l.HTTP != 0 && (l.HTTP < 1024 || l.HTTP > 65535 || l.HTTP == l.Transparent || l.HTTP == l.DNS)) {
			return nil, fmt.Errorf("egress proxy preset %q: listeners must be distinct ports in 1024-65535", name)
		}
		if r := p.Readiness; r != nil {
			port := r.HTTPGet.Port
			if port < 1024 || port > 65535 || port == l.Transparent || port == l.DNS || port == l.HTTP {
				return nil, fmt.Errorf("egress proxy preset %q: readiness.httpGet.port must be a port in 1024-65535 other than the listeners", name)
			}
			if !strings.HasPrefix(r.HTTPGet.Path, "/") || strings.ContainsAny(r.HTTPGet.Path, " \r\n") {
				return nil, fmt.Errorf("egress proxy preset %q: readiness.httpGet.path must be an absolute path without spaces", name)
			}
		}
	}
	return out, nil
}

// ProxyImageAllowed reports whether image matches AllowedProxyImages.
func ProxyImageAllowed(image string) bool {
	for _, p := range AllowedProxyImages {
		if ok, err := path.Match(p, image); err == nil && ok {
			return true
		}
	}
	return false
}

// ParseProxyImages reads the comma- or space-separated pattern list, refusing
// a malformed pattern.
func ParseProxyImages(s string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if _, err := path.Match(f, ""); err != nil {
			return nil, fmt.Errorf("egress proxy image pattern %q: %w", f, err)
		}
		out = append(out, f)
	}
	return out, nil
}

// proxyNotAvailable is the refusal of a proxy feature this build lacks, or
// "" when the spec uses none.
func proxyNotAvailable(n sandboxv1alpha1.SandboxNetwork) string {
	e := n.Egress
	if e == nil {
		return ""
	}
	if p := e.Proxy; p != nil {
		switch {
		case !EgressProxyAvailable:
			return "spec.network.egress.proxy"
		case p.Preset != "" && !PresetsAvailable:
			return "spec.network.egress.proxy.preset (use an image)"
		case p.Listeners != nil && p.Listeners.HTTP != 0 && !HTTPListenerAvailable:
			return "spec.network.egress.proxy.listeners.http"
		}
		if !ServiceUpstreamsAvailable {
			for _, r := range p.Upstreams {
				if r.Service != nil {
					return "a service entry in spec.network.egress.proxy.upstreams (use the Service's ClusterIP as a cidr)"
				}
			}
		}
	}
	if len(e.Credentials) > 0 && !CredentialsAvailable {
		return "spec.network.egress.credentials"
	}
	return ""
}

// EgressBrokerPort is the built-in broker's transparent listener in the
// launcher pod. A proxied sandbox without a proxy cannot expose it.
const EgressBrokerPort = 15001

// SPIFFEConfig is the platform's SPIFFE setup (chart
// sandboxMaterialize.spiffe), from KUBESWIFT_SANDBOX_SPIFFE. Nil: off, and
// spec.identity.spiffe Enabled is refused.
type SPIFFEConfig struct {
	// TrustDomain of the IDs the chart's ClusterSPIFFEIDs register.
	TrustDomain string `json:"trustDomain"`
	// CSIDriver mounts the Workload API socket directory (csi.spiffe.io);
	// empty uses HostPath instead.
	CSIDriver string `json:"csiDriver,omitempty"`
	// HostPath is the node directory holding the agent socket.
	HostPath string `json:"hostPath,omitempty"`
	// SocketName is the socket's file name in that directory.
	SocketName string `json:"socketName"`
}

// SPIFFE is the platform's SPIFFE setup; nil when off.
var SPIFFE *SPIFFEConfig

// TLSOriginationAvailable is the switch for spec.network.egress.allow[].tls.
var TLSOriginationAvailable = false

var trustDomainRe = regexp.MustCompile(`^[a-z0-9._-]{1,255}$`)

// ParseSPIFFE reads KUBESWIFT_SANDBOX_SPIFFE (JSON). Empty is off.
func ParseSPIFFE(s string) (*SPIFFEConfig, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var c SPIFFEConfig
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return nil, fmt.Errorf("spiffe config: %w", err)
	}
	switch {
	case !trustDomainRe.MatchString(c.TrustDomain):
		return nil, fmt.Errorf("spiffe trustDomain %q is not a SPIFFE trust domain name", c.TrustDomain)
	case c.SocketName == "" || strings.Contains(c.SocketName, "/"):
		return nil, fmt.Errorf("spiffe socketName %q must be a file name", c.SocketName)
	case c.CSIDriver == "" && !strings.HasPrefix(c.HostPath, "/"):
		return nil, fmt.Errorf("spiffe needs csiDriver or an absolute hostPath")
	}
	return &c, nil
}

var (
	envNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	placeholderRe = regexp.MustCompile(`^[A-Za-z0-9._-]{16,128}$`)
	headerRe      = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// ruleKind is where an egress rule appears.
type ruleKind int

const (
	ruleAllow    ruleKind = iota // spec.network.egress.allow
	ruleUpstream                 // spec.network.egress.proxy.upstreams
)

// EgressHost checks a host rule: a DNS name with at least two labels,
// optionally prefixed with "*." to match every name under it.
func EgressHost(h string) error {
	name := strings.TrimPrefix(h, "*.")
	if net.ParseIP(name) != nil {
		return fmt.Errorf("host %q is an address; use cidr", h)
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("host %q: a wildcard is only valid as a leading \"*.\"", h)
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("host %q: %s", h, errs[0])
	}
	if !strings.Contains(name, ".") {
		return fmt.Errorf("host %q needs at least two labels (a single label resolves through search domains)", h)
	}
	return nil
}

// hostMatches reports whether allow-rule host pattern p covers host h (both
// valid; h may itself be a wildcard, covered only by the same pattern or a
// broader one).
func hostMatches(p, h string) bool {
	if p == h {
		return true
	}
	if !strings.HasPrefix(p, "*.") {
		return false
	}
	return strings.HasSuffix(strings.TrimPrefix(h, "*."), p[1:])
}

func validateProxy(n sandboxv1alpha1.SandboxNetwork) error {
	p := n.Egress.Proxy
	if p == nil {
		return nil
	}
	const f = "spec.network.egress.proxy"
	if (p.Preset == "") == (p.Image == "") {
		return fmt.Errorf("%s: set exactly one of preset and image", f)
	}
	if p.Preset != "" {
		if errs := validation.IsDNS1123Label(p.Preset); len(errs) > 0 {
			return fmt.Errorf("%s.preset %q: %s", f, p.Preset, errs[0])
		}
		if PresetsAvailable {
			pr, ok := EgressProxyPresets[p.Preset]
			if !ok {
				return fmt.Errorf("%s.preset %q is not a preset this cluster defines (chart sandboxMaterialize.egressProxyPresets)", f, p.Preset)
			}
			for _, pt := range n.Ports {
				if pt.Port == pr.Listeners.Transparent || (pr.Listeners.DNS != 0 && pt.Port == pr.Listeners.DNS) ||
					(pr.Listeners.HTTP != 0 && pt.Port == pr.Listeners.HTTP) ||
					(pr.Readiness != nil && pt.Port == pr.Readiness.HTTPGet.Port) {
					return fmt.Errorf("spec.network.ports %s: port %d is a listener of egress proxy preset %q", pt.Name, pt.Port, p.Preset)
				}
			}
		}
		if len(p.Args) > 0 || len(p.Env) > 0 || p.Listeners != nil {
			return fmt.Errorf("%s: args, env and listeners apply to an image; a preset supplies them", f)
		}
	}
	if p.Image != "" && p.Listeners == nil {
		return fmt.Errorf("%s: an image needs listeners", f)
	}
	if l := p.Listeners; l != nil {
		seen := map[int32]string{}
		for _, pt := range n.Ports {
			seen[pt.Port] = "spec.network.ports " + pt.Name
		}
		for _, x := range []struct {
			name string
			port int32
			opt  bool
		}{{"transparent", l.Transparent, false}, {"dns", l.DNS, true}, {"http", l.HTTP, true}} {
			if x.port == 0 && x.opt {
				continue
			}
			if x.port < 1024 || x.port > 65535 {
				return fmt.Errorf("%s.listeners.%s %d is out of range 1024-65535 (the proxy runs without privileges)", f, x.name, x.port)
			}
			if other, dup := seen[x.port]; dup {
				return fmt.Errorf("%s.listeners.%s %d is also used by %s", f, x.name, x.port, other)
			}
			seen[x.port] = "listeners." + x.name
		}
	}
	switch p.Config {
	case "", sandboxv1alpha1.SandboxEgressProxyConfigSelf, sandboxv1alpha1.SandboxEgressProxyConfigIntent:
	default:
		return fmt.Errorf("%s.config %q is not Self or Intent", f, p.Config)
	}
	for i, e := range p.Env {
		if e.Name == "" {
			return fmt.Errorf("%s.env[%d].name is required", f, i)
		}
		vf := e.ValueFrom
		if vf == nil {
			continue
		}
		if e.Value != "" {
			return fmt.Errorf("%s.env[%d] (%s): set value or valueFrom, not both", f, i, e.Name)
		}
		if vf.FieldRef != nil || vf.ResourceFieldRef != nil || vf.ConfigMapKeyRef != nil || vf.FileKeyRef != nil {
			return fmt.Errorf("%s.env[%d] (%s): only valueFrom.secretKeyRef is supported", f, i, e.Name)
		}
		if ref := vf.SecretKeyRef; ref == nil || ref.Name == "" || ref.Key == "" {
			return fmt.Errorf("%s.env[%d] (%s): valueFrom.secretKeyRef needs a name and a key", f, i, e.Name)
		}
	}
	if len(p.Upstreams) > 32 {
		return fmt.Errorf("%s.upstreams has %d entries, at most 32", f, len(p.Upstreams))
	}
	for i, r := range p.Upstreams {
		if err := validateEgressRule(r, ruleUpstream, true); err != nil {
			return fmt.Errorf("%s.upstreams[%d]: %w", f, i, err)
		}
	}
	if p.Image != "" && EgressProxyAvailable && !ProxyImageAllowed(p.Image) {
		return fmt.Errorf("%s.image %q is not an egress proxy image this cluster allows (chart sandboxMaterialize.egressProxyImages)", f, p.Image)
	}
	return nil
}

func validateCredentials(n sandboxv1alpha1.SandboxNetwork) error {
	creds := n.Egress.Credentials
	if len(creds) == 0 {
		return nil
	}
	const f = "spec.network.egress.credentials"
	p := n.Egress.Proxy
	switch {
	case p == nil:
		return fmt.Errorf("%s need a proxy: the built-in broker does not terminate TLS", f)
	case p.Preset == "":
		return fmt.Errorf("%s need a proxy preset; an image proxy cannot receive credentials in this version", f)
	case p.Config == sandboxv1alpha1.SandboxEgressProxyConfigSelf:
		return fmt.Errorf("%s need proxy.config Intent; a Self proxy uses its own credential source", f)
	}
	if pr, ok := EgressProxyPresets[p.Preset]; ok && pr.Readiness == nil {
		return fmt.Errorf("%s: egress proxy preset %q has no readiness probe, so swiftletd cannot tell when it has loaded a credential", f, p.Preset)
	}
	if len(creds) > 16 {
		return fmt.Errorf("%s has %d entries, at most 16", f, len(creds))
	}
	type hostRule struct {
		host  string
		ports []sandboxv1alpha1.SandboxEgressPort
	}
	var hosts []hostRule
	for _, r := range n.Egress.Allow {
		if r.Host != "" {
			hosts = append(hosts, hostRule{r.Host, r.Ports})
		}
	}
	names := map[string]bool{}
	for i, c := range creds {
		at := fmt.Sprintf("%s[%d]", f, i)
		if !envNameRe.MatchString(c.Name) {
			return fmt.Errorf("%s.name %q is not an environment variable name", at, c.Name)
		}
		if names[c.Name] {
			return fmt.Errorf("%s.name %q appears twice", at, c.Name)
		}
		names[c.Name] = true
		if c.Placeholder != "" && !placeholderRe.MatchString(c.Placeholder) {
			return fmt.Errorf("%s.placeholder must be 16 to 128 characters from [A-Za-z0-9._-]", at)
		}
		if c.SecretKeyRef.Name == "" || c.SecretKeyRef.Key == "" {
			return fmt.Errorf("%s.secretKeyRef needs a name and a key", at)
		}
		if len(c.Destinations) == 0 || len(c.Destinations) > 16 {
			return fmt.Errorf("%s.destinations needs 1 to 16 entries", at)
		}
		for _, d := range c.Destinations {
			host, port, err := credentialDestination(d)
			if err != nil {
				return fmt.Errorf("%s.destinations: %w", at, err)
			}
			if strings.HasPrefix(host, "*.") {
				return fmt.Errorf("%s.destinations: %s: a credential destination names one host, not a wildcard (its certificate is issued per host)", at, d)
			}
			covered := false
			for _, h := range hosts {
				if hostMatches(h.host, host) && portAllowed(h.ports, port) {
					covered = true
					break
				}
			}
			if !covered {
				return fmt.Errorf("%s.destinations: %s is not allowed by a host rule in spec.network.egress.allow", at, d)
			}
		}
		if c.Header != "" {
			if !headerRe.MatchString(c.Header) {
				return fmt.Errorf("%s.header %q is not an HTTP header name", at, c.Header)
			}
			if strings.EqualFold(c.Header, "host") {
				return fmt.Errorf("%s.header cannot be Host", at)
			}
		}
		if t := c.ValueTemplate; t != "" {
			if strings.Count(t, "{}") != 1 {
				return fmt.Errorf("%s.valueTemplate must contain {} exactly once", at)
			}
			if strings.ContainsAny(t, "\r\n") {
				return fmt.Errorf("%s.valueTemplate cannot contain a line break", at)
			}
		}
	}
	return nil
}

// credentialDestination parses host or host:port (443 when omitted).
func credentialDestination(d string) (string, int32, error) {
	host, port := d, int32(443)
	if i := strings.LastIndex(d, ":"); i >= 0 {
		n, err := strconv.Atoi(d[i+1:])
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("%q: port is not 1-65535", d)
		}
		host, port = d[:i], int32(n)
	}
	if err := EgressHost(host); err != nil {
		return "", 0, err
	}
	return host, port, nil
}

// portAllowed reports whether a host rule's ports admit TCP port p (no ports
// admits every port).
func portAllowed(ports []sandboxv1alpha1.SandboxEgressPort, p int32) bool {
	if len(ports) == 0 {
		return true
	}
	for _, x := range ports {
		if x.Port == p {
			return true
		}
	}
	return false
}

// ValidateIdentity checks spec.identity of a SwiftSandbox or SwiftSandboxPool
// against its network. The controller runs it too.
func ValidateIdentity(id *sandboxv1alpha1.SandboxIdentity, n sandboxv1alpha1.SandboxNetwork) error {
	enabled := false
	if id != nil {
		switch id.SPIFFE {
		case "", sandboxv1alpha1.SandboxIdentitySPIFFEDisabled:
		case sandboxv1alpha1.SandboxIdentitySPIFFEEnabled:
			enabled = true
		default:
			return fmt.Errorf("spec.identity.spiffe %q is not Enabled or Disabled", id.SPIFFE)
		}
	}
	if n.Egress != nil {
		for i, r := range n.Egress.Allow {
			switch {
			case r.TLS == nil:
			case !enabled:
				return fmt.Errorf("spec.network.egress.allow[%d].tls needs spec.identity.spiffe Enabled", i)
			case !TLSOriginationAvailable:
				return fmt.Errorf("spec.network.egress.allow[%d].tls is not available in this version of KubeSwift yet", i)
			}
		}
	}
	if !enabled {
		return nil
	}
	if n.Mode != sandboxv1alpha1.SandboxNetworkProxied {
		return fmt.Errorf("spec.identity.spiffe Enabled needs network mode proxied: the egress proxy or the built-in broker holds the identity, never the guest")
	}
	if SPIFFE == nil {
		return fmt.Errorf("spec.identity.spiffe Enabled needs SPIFFE on the platform (chart sandboxMaterialize.spiffe.enabled)")
	}
	if n.Egress == nil || n.Egress.Proxy == nil {
		return fmt.Errorf("spec.identity.spiffe Enabled needs an egress proxy (spec.network.egress.proxy): the built-in broker does not hold an SVID in this version of KubeSwift")
	}
	return nil
}
