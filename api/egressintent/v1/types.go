// SPDX-License-Identifier: Apache-2.0

// Package v1 is the egress-intent.kubeswift.io/v1 file: the product-neutral
// policy KubeSwift hands to the egress proxy of a proxied SwiftSandbox whose
// spec.network.egress.proxy.config is Intent. The proxy, or a translator
// next to it, turns the file into its own configuration; KubeSwift never
// renders product-specific config.
//
// The JSON Schema in egress-intent.schema.json is the contract adapters
// consume; these Go types generate nothing and are checked against it by
// tests. Like the rest of KubeSwift it is Apache-2.0, so other projects can
// implement the contract freely.
//
// Rules of the file:
//
//   - It is mounted read-only at Path and never changes for the life of the
//     launcher pod. Per-sandbox data that arrives later (credential values,
//     TLS keys) is in separate files named here, all in SecretsDir.
//   - Within v1 fields may be added only where a consumer that ignores them
//     stays safe. Consumers MUST ignore fields they do not know. A change a
//     consumer must not ignore is a new version (v2).
//   - A proxy allows a guest connection only if it matches an Allow rule and
//     denies everything else. See Rule for the matching rules.
package v1

// APIVersion and Kind identify the file.
const (
	APIVersion = "egress-intent.kubeswift.io/v1"
	Kind       = "EgressIntent"
)

// Path is where the file is mounted in the proxy container.
const Path = "/etc/kubeswift/egress/intent.json"

// ImageLabel on a translator or proxy image lists the intent versions it
// reads, comma-separated (for example "v1").
const ImageLabel = "io.kubeswift.egress-intent.versions"

// FeaturesLabel on the same image lists optional parts of v1 it implements,
// comma-separated. KubeSwift writes an optional part only for an image that
// declares it, and refuses a sandbox that needs one its image lacks
// (EgressIntentUnsupported), rather than have it ignored.
const FeaturesLabel = "io.kubeswift.egress-intent.features"

// FeatureTLSOriginate: Rule.TLS. The proxy connects to the rule's
// destinations over TLS presenting the X.509-SVID from Identity.SPIFFE and
// accepts only an upstream SVID of the same trust domain; the guest speaks
// plaintext (for a host rule, HTTP, matched by its Host header).
const FeatureTLSOriginate = "tls-originate"

// FeatureProxyProtocol: Listeners.ProxyProtocol. The proxy reads a PROXY
// protocol header at the start of every connection on the transparent
// listener and takes the original destination from it.
const FeatureProxyProtocol = "proxy-protocol"

// EgressIntent is the whole file.
type EgressIntent struct {
	// APIVersion is APIVersion.
	APIVersion string `json:"apiVersion"`
	// Kind is Kind.
	Kind string `json:"kind"`
	// Owner is the object the launcher pod belongs to: the pool for a warm
	// slot, the sandbox for a cold launcher. Which sandbox claimed a slot is
	// not in the file; it shows in the launcher pod's labels.
	Owner Owner `json:"owner"`
	// Listeners are the ports the proxy must listen on.
	Listeners Listeners `json:"listeners"`
	// Allow is the guest's allowlist. Empty allows nothing.
	Allow []Rule `json:"allow"`
	// Credentials the proxy adds to requests in place of placeholders.
	Credentials []Credential `json:"credentials,omitempty"`
	// TLS material the proxy uses. Present when Credentials is not empty.
	TLS *TLS `json:"tls,omitempty"`
	// SecretsDir holds every file named in Credentials and TLS, on a
	// memory-backed volume shared only with the proxy. KubeSwift replaces
	// the whole content at once: the files are symlinks to "..data/<name>",
	// and "..data" is a symlink to a fresh directory, swapped by rename (the
	// kubelet's layout for projected volumes). A proxy watches SecretsDir and
	// reloads all of its files together. Present when Credentials is not
	// empty. The files exist before the proxy starts; a warm slot's hold
	// throwaway material and empty values until a sandbox claims it.
	SecretsDir string `json:"secretsDir,omitempty"`
	// Identity the proxy may use. Present when the sandbox has a SPIFFE
	// identity.
	Identity *Identity `json:"identity,omitempty"`
}

// Owner names the KubeSwift object behind the launcher pod.
type Owner struct {
	Namespace string `json:"namespace"`
	// Kind is SwiftSandbox or SwiftSandboxPool.
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// Listeners are TCP ports in the launcher pod's network namespace. The
// transparent listener must bind all addresses (the guest's connections are
// redirected to the bridge address, not to loopback), except with
// ProxyProtocol; KubeSwift keeps it unreachable from outside the pod.
type Listeners struct {
	// Transparent receives every guest TCP connection. The original
	// destination is read with getsockopt(SO_ORIGINAL_DST).
	Transparent int32 `json:"transparent"`
	// DNS, when set, receives every guest DNS query (UDP and TCP).
	DNS int32 `json:"dns,omitempty"`
	// HTTP, when set, is an explicit HTTP proxy (CONNECT and absolute-URI
	// requests); the guest's HTTP_PROXY and HTTPS_PROXY point at it.
	HTTP int32 `json:"http,omitempty"`
	// ProxyProtocol, when set ("v1" or "v2"), means the proxy cannot read
	// SO_ORIGINAL_DST: KubeSwift redirects guest connections to a relay that
	// connects to Transparent on 127.0.0.1 and starts each connection with a
	// PROXY protocol header of this version carrying the guest's address and
	// the original destination. Transparent then only needs to listen on
	// loopback, and only the relay reaches it. The proxy must require the
	// header (refuse a connection without one).
	ProxyProtocol string `json:"proxyProtocol,omitempty"`
}

// Rule allows one destination. Exactly one of Host and CIDR is set.
//
// Matching: a Host rule matches a connection whose TLS server name, HTTP Host
// or CONNECT authority equals Host, or, for "*.example.com", ends in
// ".example.com" (one or more labels). A server name alone must never reach
// another address: a proxy either refuses the connection unless its original
// destination is one of the addresses the name resolves to (KubeSwift's
// built-in broker), or connects to the name's own address instead of the
// original destination (a dynamic forward proxy, such as Envoy's). Either
// way it never connects to an address the name does not resolve to. A CIDR
// rule matches the original destination, and the proxy connects to it. When
// Ports is set the destination port must be one of them.
type Rule struct {
	// Host is a DNS name, optionally with a leading "*.".
	Host string `json:"host,omitempty"`
	// CIDR is an IPv4 range. Service rules in the sandbox spec arrive here
	// resolved to their ClusterIP.
	CIDR string `json:"cidr,omitempty"`
	// Ports narrows the rule; empty allows every port.
	Ports []Port `json:"ports,omitempty"`
	// TLS asks the proxy to originate TLS to this destination.
	TLS *RuleTLS `json:"tls,omitempty"`
	// From is the sandbox spec entry this came from, for logs:
	// "host <name>", "cidr <cidr>" or "service <namespace>/<name>".
	From string `json:"from"`
}

// Port is one destination port. Only TCP reaches a proxy.
type Port struct {
	Port int32 `json:"port"`
	// Protocol is TCP.
	Protocol string `json:"protocol"`
}

// RuleTLS is TLS the proxy originates on the guest's behalf.
type RuleTLS struct {
	// Originate is SPIFFE: present the sandbox's X.509-SVID as client
	// certificate. The guest connects in plaintext.
	Originate string `json:"originate"`
}

// Credential is one secret the proxy swaps in for a placeholder.
//
// The proxy terminates the guest's TLS for Destinations (with a certificate
// from TLS.Certificates, or one it mints with TLS.InterceptCA), sets Header
// to ValueTemplate ("{}" standing for the value read from File) on requests
// to Destinations, and must not let a request whose Host differs from the
// destination use the credential. An empty value (a warm slot not yet
// claimed) must fail the request, never forward the placeholder. A proxy
// cannot see inside TLS it does not terminate, so a placeholder sent to
// another allowed destination leaves unchanged; it is worthless there.
type Credential struct {
	// Name of the guest environment variable holding the placeholder.
	Name string `json:"name"`
	// Placeholder is the value the guest sees.
	Placeholder string `json:"placeholder"`
	// Destinations are host:port, port always present.
	Destinations []string `json:"destinations"`
	// Header is the request header to rewrite.
	Header string `json:"header"`
	// ValueTemplate is the header value, "{}" standing for the secret.
	ValueTemplate string `json:"valueTemplate"`
	// File holds the secret value, in SecretsDir. It exists from start,
	// empty until the value is delivered; the proxy must pick up a new value
	// without a restart.
	File string `json:"file"`
	// HeaderValueFile holds ValueTemplate with the value filled in, for a
	// proxy that cannot apply a template. Same lifecycle as File.
	HeaderValueFile string `json:"headerValueFile,omitempty"`
}

// TLS is TLS material for the proxy.
type TLS struct {
	// InterceptCA is the certificate authority the guest trusts for
	// intercepted connections. It is name-constrained to the credential
	// destinations' hosts and replaced when a warm slot is claimed.
	InterceptCA CA `json:"interceptCA"`
	// Certificates are leaf certificates KubeSwift issued with InterceptCA,
	// one per credential destination host. A proxy presents the one whose
	// Hosts match the TLS server name.
	Certificates []Certificate `json:"certificates,omitempty"`
}

// CA is a certificate authority as PEM files.
type CA struct {
	// Cert is the CA certificate.
	Cert string `json:"cert"`
	// Key is the CA private key, for a proxy that mints its own
	// certificates. KubeSwift keeps the key out of the proxy and leaves this
	// empty; such a proxy uses Certificates.
	Key string `json:"key,omitempty"`
}

// Certificate is a leaf certificate and its private key as PEM files, in
// SecretsDir.
type Certificate struct {
	// Hosts are the DNS names in the certificate (subjectAltName).
	Hosts []string `json:"hosts"`
	Cert  string   `json:"cert"`
	Key   string   `json:"key"`
}

// Identity is what the proxy may use to prove the sandbox's identity.
type Identity struct {
	SPIFFE *SPIFFE `json:"spiffe,omitempty"`
}

// SPIFFE points at the SPIFFE Workload API, where the proxy fetches the
// launcher's X.509-SVID. A warm slot's ID is issued when the slot boots and
// does not change at checkout.
type SPIFFE struct {
	// WorkloadAPISocket is the Workload API address, e.g.
	// unix:///run/spiffe/spire-agent.sock.
	WorkloadAPISocket string `json:"workloadAPISocket"`
	// ID is the SPIFFE ID registered for this launcher. The SVID may arrive
	// a moment after the proxy starts; a proxy waits for it rather than
	// failing.
	ID string `json:"id,omitempty"`
}
