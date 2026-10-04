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
	return validateExposure(n)
}

func validateEgress(n sandboxv1alpha1.SandboxNetwork) error {
	if n.Egress == nil {
		return nil
	}
	if n.Mode != "" && n.Mode != sandboxv1alpha1.SandboxNetworkRestricted {
		return fmt.Errorf("spec.network.egress applies to mode restricted only (mode is %s)", n.Mode)
	}
	if len(n.Egress.Allow) > 32 {
		return fmt.Errorf("spec.network.egress.allow has %d entries, at most 32", len(n.Egress.Allow))
	}
	for i, r := range n.Egress.Allow {
		if err := validateEgressRule(r); err != nil {
			return fmt.Errorf("spec.network.egress.allow[%d]: %w", i, err)
		}
	}
	return nil
}

func validateEgressRule(r sandboxv1alpha1.SandboxEgressRule) error {
	if (r.Service == nil) == (r.CIDR == "") {
		return fmt.Errorf("set exactly one of service and cidr")
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
