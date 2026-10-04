package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func allow(rules ...sandboxv1alpha1.SandboxEgressRule) sandboxv1alpha1.SandboxNetwork {
	return sandboxv1alpha1.SandboxNetwork{Egress: &sandboxv1alpha1.SandboxEgress{Allow: rules}}
}

func svc(name, ns string) *sandboxv1alpha1.SandboxEgressService {
	return &sandboxv1alpha1.SandboxEgressService{Name: name, Namespace: ns}
}

func TestValidateNetwork(t *testing.T) {
	ok := []struct {
		name string
		n    sandboxv1alpha1.SandboxNetwork
	}{
		{"no egress", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkOpen}},
		{"service in another namespace, one port", allow(sandboxv1alpha1.SandboxEgressRule{
			Service: svc("llm", "inference"), Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 8000}}})},
		{"cidr with a UDP port", allow(sandboxv1alpha1.SandboxEgressRule{
			CIDR: "10.20.0.0/24", Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 5432, Protocol: corev1.ProtocolUDP}}})},
		// A range that merely contains the metadata range is allowed: the
		// launcher DROPs 169.254/16 before any allow rule.
		{"a wide range around 169.254/16", allow(sandboxv1alpha1.SandboxEgressRule{CIDR: "169.0.0.0/8"})},
		{"explicit restricted", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkRestricted,
			Egress: &sandboxv1alpha1.SandboxEgress{Allow: []sandboxv1alpha1.SandboxEgressRule{{CIDR: "10.0.0.1/32"}}}}},
	}
	for _, tc := range ok {
		if err := ValidateNetwork(tc.n); err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}

	bad := []struct {
		name, want string
		n          sandboxv1alpha1.SandboxNetwork
	}{
		{"egress with open", "applies to mode restricted only", sandboxv1alpha1.SandboxNetwork{
			Mode: sandboxv1alpha1.SandboxNetworkOpen, Egress: &sandboxv1alpha1.SandboxEgress{}}},
		{"egress with none", "applies to mode restricted only", sandboxv1alpha1.SandboxNetwork{
			Mode: sandboxv1alpha1.SandboxNetworkNone, Egress: &sandboxv1alpha1.SandboxEgress{}}},
		{"both service and cidr", "exactly one of service and cidr",
			allow(sandboxv1alpha1.SandboxEgressRule{Service: svc("a", ""), CIDR: "10.0.0.0/8"})},
		{"neither", "exactly one of service and cidr", allow(sandboxv1alpha1.SandboxEgressRule{})},
		{"the metadata address", "never allowed", allow(sandboxv1alpha1.SandboxEgressRule{CIDR: "169.254.169.254/32"})},
		{"the link-local range", "never allowed", allow(sandboxv1alpha1.SandboxEgressRule{CIDR: "169.254.0.0/16"})},
		{"a bare address", "is not a CIDR", allow(sandboxv1alpha1.SandboxEgressRule{CIDR: "10.0.0.1"})},
		{"IPv6", "is IPv6", allow(sandboxv1alpha1.SandboxEgressRule{CIDR: "fd00::/64"})},
		{"a port out of range", "out of range", allow(sandboxv1alpha1.SandboxEgressRule{
			CIDR: "10.0.0.0/8", Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 70000}}})},
		{"SCTP", "is not TCP or UDP", allow(sandboxv1alpha1.SandboxEgressRule{
			CIDR: "10.0.0.0/8", Ports: []sandboxv1alpha1.SandboxEgressPort{{Port: 1, Protocol: corev1.ProtocolSCTP}}})},
		{"a bad service name", "service.name", allow(sandboxv1alpha1.SandboxEgressRule{Service: svc("Not_A_Name", "")})},
		{"a bad namespace", "service.namespace", allow(sandboxv1alpha1.SandboxEgressRule{Service: svc("a", "-x")})},
	}
	for _, tc := range bad {
		err := ValidateNetwork(tc.n)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}

	var many []sandboxv1alpha1.SandboxEgressRule
	for i := 0; i < 33; i++ {
		many = append(many, sandboxv1alpha1.SandboxEgressRule{CIDR: "10.0.0.0/8"})
	}
	if err := ValidateNetwork(allow(many...)); err == nil {
		t.Error("33 rules must be refused")
	}
}

func TestEgressCIDR_Canonical(t *testing.T) {
	if got, err := EgressCIDR("10.20.0.5/24"); err != nil || got != "10.20.0.0/24" {
		t.Errorf("EgressCIDR(10.20.0.5/24) = %q, %v; want 10.20.0.0/24", got, err)
	}
}
