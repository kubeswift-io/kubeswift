package swiftsandbox

import (
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// buildNetworkPolicy builds the "restricted" NetworkPolicy for a networked
// sandbox: deny ALL ingress to the launcher pod (nothing on the cluster can reach
// the sandbox) while allowing egress (a CI/agent sandbox's outbound is the point).
// mode=none has no pod network at all, so no policy is created there. Narrowing
// egress to specific destinations is a follow-up; deny-ingress is the v1 floor.
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
			// No ingress rules -> deny all inbound.
			Ingress: []networkingv1.NetworkPolicyIngressRule{},
			// One empty egress rule -> allow all outbound.
			Egress: []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
}
