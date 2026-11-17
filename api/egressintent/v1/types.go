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
//     launcher pod. Per-sandbox data that arrives later (credential values)
//     is in separate files named here.
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
// redirected to the bridge address, not to loopback); KubeSwift keeps it
// unreachable from outside the pod.
type Listeners struct {
	// Transparent receives every guest TCP connection. The original
	// destination is read with getsockopt(SO_ORIGINAL_DST).
	Transparent int32 `json:"transparent"`
	// DNS, when set, receives every guest DNS query (UDP and TCP).
	DNS int32 `json:"dns,omitempty"`
	// HTTP, when set, is an explicit HTTP proxy (CONNECT and absolute-URI
	// requests); the guest's HTTP_PROXY and HTTPS_PROXY point at it.
	HTTP int32 `json:"http,omitempty"`
}

// Rule allows one destination. Exactly one of Host and CIDR is set.
//
// Matching: a Host rule matches a connection whose TLS server name, HTTP Host
// or CONNECT authority equals Host, or, for "*.example.com", ends in
// ".example.com" (one or more labels). The connection's original destination
// must also be an address Host resolves to, so a server name alone cannot
// reach another address. A CIDR rule matches the original destination. When
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
// The proxy terminates the guest's TLS with TLS.InterceptCA, replaces
// Placeholder in Header with ValueTemplate ("{}" standing for the value read
// from File) on requests to Destinations, and refuses any request to another
// destination that still contains Placeholder.
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
	// File holds the secret value. It exists from start, empty until the
	// value is delivered, and is replaced atomically (rename); the proxy
	// must pick up a replaced file without a restart.
	File string `json:"file"`
}

// TLS is TLS material for the proxy.
type TLS struct {
	// InterceptCA signs the certificates the proxy presents to the guest.
	InterceptCA CA `json:"interceptCA"`
}

// CA is a certificate authority as two PEM files. The guest trusts Cert;
// Key never leaves the launcher pod. Both are replaced atomically when a
// warm slot is claimed.
type CA struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// Identity is what the proxy may use to prove the sandbox's identity.
type Identity struct {
	SPIFFE *SPIFFE `json:"spiffe,omitempty"`
}

// SPIFFE points at the SPIFFE Workload API. The SVID's ID is the sandbox's;
// a proxy must treat an SVID whose path has no "/sandbox/" segment as not yet
// issued (a warm slot before checkout).
type SPIFFE struct {
	// WorkloadAPISocket is the Workload API address, e.g.
	// unix:///run/spiffe/agent.sock.
	WorkloadAPISocket string `json:"workloadAPISocket"`
}
