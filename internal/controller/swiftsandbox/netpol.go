package swiftsandbox

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// buildNetworkPolicy builds the deny-ingress NetworkPolicy for a networked sandbox:
// nothing on the cluster can reach the sandbox. Egress is left allow-all HERE on
// purpose — the VM's egress hardening is done in-pod (the restricted-egress FORWARD
// iptables in network-init.sh), NOT in this NetworkPolicy.
//
// Why not the NetworkPolicy: it applies to the whole launcher pod, and after
// MASQUERADE the VM's traffic and swiftletd's own API-server traffic both source
// from the pod IP — a NetworkPolicy that blocked cluster egress would also cut
// swiftletd's status reporting (#347). Only the FORWARD chain can match the VM's
// pre-NAT source (bridge subnet) and filter the VM alone. So this policy owns
// ingress isolation; the iptables owns egress. mode=none has no pod network at all,
// so no policy is created. Same policy for restricted and open (both deny-ingress);
// the restricted/open difference is entirely the in-pod egress rules.
func buildNetworkPolicy(sb *sandboxv1alpha1.SwiftSandbox) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sb.Name + "-restricted",
			Namespace: sb.Namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{SandboxLabelKey: sb.Name}},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// No ingress rules -> deny all inbound, unless the sandbox exposes
			// ports (ingressRules).
			Ingress: ingressRules(sb.Spec.Network),
			// One empty egress rule -> allow all outbound.
			Egress: []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
}

// ingressRules admits the sandbox's declared ports, from spec.network.ingress
// peers when set, else from anywhere; with no ports, nothing.
func ingressRules(n sandboxv1alpha1.SandboxNetwork) []networkingv1.NetworkPolicyIngressRule {
	if len(n.Ports) == 0 {
		return []networkingv1.NetworkPolicyIngressRule{}
	}
	rule := networkingv1.NetworkPolicyIngressRule{}
	tcp := corev1.ProtocolTCP
	for _, p := range n.Ports {
		port := intstr.FromInt32(p.Port)
		rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	if n.Ingress != nil {
		rule.From = n.Ingress.From
	}
	return []networkingv1.NetworkPolicyIngressRule{rule}
}
