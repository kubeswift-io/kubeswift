package swiftsandbox

import (
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// linkLocal holds the cloud metadata endpoint. No egress rule may open it.
var linkLocal = &net.IPNet{IP: net.IPv4(169, 254, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}

// ValidateNetwork checks spec.network of a SwiftSandbox or SwiftSandboxPool.
// The controller runs it too: the webhook is off by default, and the launcher
// renders these rules into iptables and its NetworkPolicy.
func ValidateNetwork(n sandboxv1alpha1.SandboxNetwork) error {
	if err := validateEgress(n); err != nil {
		return err
	}
	if err := validateExposure(n); err != nil {
		return err
	}
	if n.Mode == sandboxv1alpha1.SandboxNetworkProxied {
		if !ProxiedAvailable {
			return fmt.Errorf("spec.network.mode proxied is not available in this version of KubeSwift yet")
		}
		if what := proxyNotAvailable(n); what != "" {
			return fmt.Errorf("%s is not available in this version of KubeSwift yet", what)
		}
		if UsesBrokerPort(n) {
			for _, p := range n.Ports {
				if p.Port == EgressBrokerPort {
					return fmt.Errorf("spec.network.ports %s: port %d is the egress broker's (or relay's) in mode proxied", p.Name, p.Port)
				}
			}
		}
	}
	return nil
}

func validateEgress(n sandboxv1alpha1.SandboxNetwork) error {
	if n.Egress == nil {
		return nil
	}
	proxied := n.Mode == sandboxv1alpha1.SandboxNetworkProxied
	if n.Mode != "" && n.Mode != sandboxv1alpha1.SandboxNetworkRestricted && !proxied {
		return fmt.Errorf("spec.network.egress applies to mode restricted or proxied only (mode is %s)", n.Mode)
	}
	if len(n.Egress.Allow) > 32 {
		return fmt.Errorf("spec.network.egress.allow has %d entries, at most 32", len(n.Egress.Allow))
	}
	for i, r := range n.Egress.Allow {
		if err := validateEgressRule(r, ruleAllow, proxied); err != nil {
			return fmt.Errorf("spec.network.egress.allow[%d]: %w", i, err)
		}
	}
	if !proxied {
		if n.Egress.Proxy != nil {
			return fmt.Errorf("spec.network.egress.proxy needs mode proxied")
		}
		if len(n.Egress.Credentials) > 0 {
			return fmt.Errorf("spec.network.egress.credentials need mode proxied")
		}
	}
	if err := validateProxy(n); err != nil {
		return err
	}
	return validateCredentials(n)
}

func validateEgressRule(r sandboxv1alpha1.SandboxEgressRule, kind ruleKind, proxied bool) error {
	set := 0
	for _, b := range []bool{r.Service != nil, r.CIDR != "", r.Host != ""} {
		if b {
			set++
		}
	}
	if set != 1 {
		if proxied && kind == ruleAllow {
			return fmt.Errorf("set exactly one of service, cidr and host")
		}
		return fmt.Errorf("set exactly one of service and cidr")
	}
	if r.Host != "" {
		switch {
		case kind == ruleUpstream:
			return fmt.Errorf("host is not valid in upstreams: the proxy's own connections are filtered by address")
		case !proxied:
			return fmt.Errorf("host needs mode proxied: mode restricted filters by address and cannot match names")
		}
		if err := EgressHost(r.Host); err != nil {
			return err
		}
	}
	if r.TLS != nil {
		switch {
		case kind == ruleUpstream:
			return fmt.Errorf("tls is not valid in upstreams")
		case !proxied:
			return fmt.Errorf("tls needs mode proxied")
		case r.TLS.Originate != sandboxv1alpha1.SandboxEgressTLSSPIFFE:
			return fmt.Errorf("tls.originate %q is not SPIFFE", r.TLS.Originate)
		}
	}
	if s := r.Service; s != nil {
		if errs := validation.IsDNS1035Label(s.Name); len(errs) > 0 {
			return fmt.Errorf("service.name %q: %s", s.Name, errs[0])
		}
		if s.Namespace != "" {
			if errs := validation.IsDNS1123Label(s.Namespace); len(errs) > 0 {
				return fmt.Errorf("service.namespace %q: %s", s.Namespace, errs[0])
			}
		}
	}
	if r.CIDR != "" {
		if _, err := EgressCIDR(r.CIDR); err != nil {
			return err
		}
	}
	if len(r.Ports) > 16 {
		return fmt.Errorf("ports has %d entries, at most 16", len(r.Ports))
	}
	for _, p := range r.Ports {
		if p.Port < 1 || p.Port > 65535 {
			return fmt.Errorf("port %d is out of range 1-65535", p.Port)
		}
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP && p.Protocol != corev1.ProtocolUDP {
			return fmt.Errorf("port %d: protocol %s is not TCP or UDP", p.Port, p.Protocol)
		}
		if p.Protocol == corev1.ProtocolUDP && proxied && kind == ruleAllow {
			return fmt.Errorf("port %d: mode proxied carries no guest UDP other than DNS", p.Port)
		}
	}
	return nil
}

// EgressCIDR parses an allow-rule CIDR into its canonical IPv4 form
// (10.20.0.5/24 -> 10.20.0.0/24). It refuses IPv6, a bare address, and
// any range inside 169.254.0.0/16.
func EgressCIDR(s string) (string, error) {
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return "", fmt.Errorf("cidr %q is not a CIDR (e.g. 10.20.0.0/24, or 10.20.0.5/32 for one address)", s)
	}
	if ip.To4() == nil {
		return "", fmt.Errorf("cidr %q is IPv6; a restricted sandbox has no IPv6 egress", s)
	}
	if ones, _ := ipnet.Mask.Size(); ones >= 16 && linkLocal.Contains(ipnet.IP) {
		return "", fmt.Errorf("cidr %q is in 169.254.0.0/16 (link-local, the cloud metadata endpoint), which is never allowed", s)
	}
	return ipnet.String(), nil
}
