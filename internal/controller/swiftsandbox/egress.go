package swiftsandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
	sandboxwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsandbox"
)

// EgressAllowedAnnotation records, on a launcher pod, the egress allowlist its
// network-init enforces (JSON of []SandboxEgressAllowed). Checkout copies it
// into the claiming sandbox's status, and a pool replaces warm slots whose
// list no longer matches what its Services resolve to.
const EgressAllowedAnnotation = "sandbox.kubeswift.io/egress-allowed"

// egressProblem is why an egress allowlist cannot be enforced. Invalid is a
// spec error; otherwise a Service is not usable yet and the caller waits.
type egressProblem struct {
	Reason, Message string
	Invalid         bool
}

// resolveEgress turns spec.network.egress.allow into the rules the launcher
// enforces, reading each Service (namespace defaults to ns) for its IPv4
// ClusterIPs and ports.
func resolveEgress(ctx context.Context, c client.Reader, ns string, n sandboxv1alpha1.SandboxNetwork) ([]sandboxv1alpha1.SandboxEgressAllowed, *egressProblem, error) {
	if err := sandboxwebhook.ValidateNetwork(n); err != nil {
		return nil, &egressProblem{Reason: "InvalidEgress", Message: err.Error(), Invalid: true}, nil
	}
	if n.Egress == nil {
		return nil, nil, nil
	}
	var out []sandboxv1alpha1.SandboxEgressAllowed
	for i, r := range n.Egress.Allow {
		if r.CIDR != "" {
			cidr, err := sandboxwebhook.EgressCIDR(r.CIDR)
			if err != nil { // validated above
				return nil, &egressProblem{Reason: "InvalidEgress", Message: err.Error(), Invalid: true}, nil
			}
			from := "cidr " + cidr
			if len(r.Ports) == 0 {
				out = append(out, sandboxv1alpha1.SandboxEgressAllowed{CIDR: cidr, From: from})
			}
			for _, p := range r.Ports {
				out = append(out, sandboxv1alpha1.SandboxEgressAllowed{CIDR: cidr, Protocol: protocol(p.Protocol), Port: p.Port, From: from})
			}
			continue
		}

		svcNS := r.Service.Namespace
		if svcNS == "" {
			svcNS = ns
		}
		ref := svcNS + "/" + r.Service.Name
		at := fmt.Sprintf("spec.network.egress.allow[%d]: Service %s", i, ref)
		var svc corev1.Service
		if err := c.Get(ctx, types.NamespacedName{Namespace: svcNS, Name: r.Service.Name}, &svc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, &egressProblem{Reason: "EgressServiceNotFound", Message: at + " not found"}, nil
			}
			return nil, nil, err
		}
		ips := ipv4ClusterIPs(&svc)
		if len(ips) == 0 {
			return nil, &egressProblem{Reason: "EgressServiceNoClusterIP", Message: at +
				" has no IPv4 ClusterIP (headless, ExternalName or IPv6-only); allow its endpoints with a cidr instead"}, nil
		}
		ports, missing := servicePorts(&svc, r.Ports)
		if missing != "" {
			return nil, &egressProblem{Reason: "EgressServicePortNotFound", Message: at + " declares no port " + missing}, nil
		}
		for _, ip := range ips {
			for _, p := range ports {
				out = append(out, sandboxv1alpha1.SandboxEgressAllowed{CIDR: ip + "/32", Protocol: p.Protocol, Port: p.Port, From: "service " + ref})
			}
		}
	}
	return out, nil, nil
}

func protocol(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP // the CRD default
	}
	return p
}

func ipv4ClusterIPs(svc *corev1.Service) []string {
	ips := svc.Spec.ClusterIPs
	if len(ips) == 0 && svc.Spec.ClusterIP != "" {
		ips = []string{svc.Spec.ClusterIP}
	}
	var out []string
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			out = append(out, ip.To4().String())
		}
	}
	return out
}

// servicePorts returns the ports to allow on a Service: the ones asked for,
// each of which the Service must declare, or all its TCP and UDP ports.
// missing names the first asked-for port it does not declare.
func servicePorts(svc *corev1.Service, want []sandboxv1alpha1.SandboxEgressPort) (ports []sandboxv1alpha1.SandboxEgressPort, missing string) {
	declared := func(port int32, proto corev1.Protocol) bool {
		for _, sp := range svc.Spec.Ports {
			if sp.Port == port && protocol(sp.Protocol) == proto {
				return true
			}
		}
		return false
	}
	if len(want) == 0 {
		for _, sp := range svc.Spec.Ports {
			if p := protocol(sp.Protocol); p == corev1.ProtocolTCP || p == corev1.ProtocolUDP {
				ports = append(ports, sandboxv1alpha1.SandboxEgressPort{Port: sp.Port, Protocol: p})
			}
		}
		return ports, ""
	}
	for _, w := range want {
		p := protocol(w.Protocol)
		if !declared(w.Port, p) {
			return nil, fmt.Sprintf("%d/%s", w.Port, p)
		}
		ports = append(ports, sandboxv1alpha1.SandboxEgressPort{Port: w.Port, Protocol: p})
	}
	return ports, ""
}

// egressAllowEnv renders the rules for network-init.sh, space-separated
// "<proto>,<cidr>,<port>" with proto tcp, udp or any (port 0). Every field was
// produced here from parsed values, so it holds only [a-z0-9.,/].
func egressAllowEnv(allowed []sandboxv1alpha1.SandboxEgressAllowed) string {
	rules := make([]string, 0, len(allowed))
	for _, a := range allowed {
		proto := "any"
		if a.Port != 0 {
			proto = strings.ToLower(string(a.Protocol))
		}
		rules = append(rules, proto+","+a.CIDR+","+strconv.Itoa(int(a.Port)))
	}
	return strings.Join(rules, " ")
}

// egressAllowedJSON is the EgressAllowedAnnotation value; "" for no rules.
func egressAllowedJSON(allowed []sandboxv1alpha1.SandboxEgressAllowed) string {
	if len(allowed) == 0 {
		return ""
	}
	b, err := json.Marshal(allowed)
	if err != nil {
		panic(err) // strings and ints always marshal
	}
	return string(b)
}

// parseEgressAllowed reads EgressAllowedAnnotation back; nil when absent or unreadable.
func parseEgressAllowed(s string) []sandboxv1alpha1.SandboxEgressAllowed {
	if s == "" {
		return nil
	}
	var out []sandboxv1alpha1.SandboxEgressAllowed
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

// egressAllowed is the allowlist a launch of sb enforces, as stamped into its
// status by the caller before the pod is built.
func egressAllowed(sb *sandboxv1alpha1.SwiftSandbox) []sandboxv1alpha1.SandboxEgressAllowed {
	if sb.Status.Network == nil {
		return nil
	}
	return sb.Status.Network.EgressAllowed
}

// setEgressAllowed records the allowlist a launch of sb enforces; buildPod
// renders it and the launcher pod carries it (EgressAllowedAnnotation).
func setEgressAllowed(sb *sandboxv1alpha1.SwiftSandbox, allowed []sandboxv1alpha1.SandboxEgressAllowed) {
	if sb.Status.Network == nil {
		if len(allowed) == 0 {
			return
		}
		sb.Status.Network = &sandboxv1alpha1.SandboxNetworkStatus{}
	}
	sb.Status.Network.EgressAllowed = allowed
}
