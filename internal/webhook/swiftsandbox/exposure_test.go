package swiftsandbox

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

func ports(ps ...sandboxv1alpha1.SandboxPort) sandboxv1alpha1.SandboxNetwork {
	return sandboxv1alpha1.SandboxNetwork{Ports: ps}
}

func TestValidateNetwork_Exposure(t *testing.T) {
	http := sandboxv1alpha1.SandboxPort{Name: "http-app", Port: 3000}
	fromNS := &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}}
	for _, tc := range []struct {
		name string
		n    sandboxv1alpha1.SandboxNetwork
	}{
		{"one named port", ports(http)},
		{"two ports", ports(http, sandboxv1alpha1.SandboxPort{Name: "metrics", Port: 9090, Protocol: corev1.ProtocolTCP})},
		{"ports from every namespace", sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{http}, Ingress: fromNS}},
		{"ports from an ipBlock", sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{http},
			Ingress: &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.1.0.0/16"}}}}}}},
		{"ports with open", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkOpen, Ports: []sandboxv1alpha1.SandboxPort{http}}},
	} {
		if err := ValidateNetwork(tc.n); err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}

	for _, tc := range []struct {
		name, want string
		n          sandboxv1alpha1.SandboxNetwork
	}{
		{"ports with mode none", "mode none", sandboxv1alpha1.SandboxNetwork{Mode: sandboxv1alpha1.SandboxNetworkNone, Ports: []sandboxv1alpha1.SandboxPort{http}}},
		{"ingress without ports", "needs spec.network.ports", sandboxv1alpha1.SandboxNetwork{Ingress: fromNS}},
		{"a name that is not an IANA service name", "name", ports(sandboxv1alpha1.SandboxPort{Name: "HTTP_App", Port: 80})},
		{"a name of digits only", "name", ports(sandboxv1alpha1.SandboxPort{Name: "8080", Port: 8080})},
		{"a name longer than 15", "name", ports(sandboxv1alpha1.SandboxPort{Name: "a-very-long-port-name", Port: 80})},
		{"UDP", "only TCP", ports(sandboxv1alpha1.SandboxPort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP})},
		{"a port out of range", "out of range", ports(sandboxv1alpha1.SandboxPort{Name: "x", Port: 0})},
		{"a name twice", "declared twice", ports(http, sandboxv1alpha1.SandboxPort{Name: "http-app", Port: 3001})},
		{"a port twice", "declared twice", ports(http, sandboxv1alpha1.SandboxPort{Name: "other", Port: 3000})},
		{"an empty peer", "set podSelector", sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{http},
			Ingress: &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{{}}}}},
		{"ipBlock with a selector", "cannot be combined", sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{http},
			Ingress: &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}, PodSelector: &metav1.LabelSelector{}}}}}},
		{"an except outside the block", "strict subset", sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{http},
			Ingress: &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/16", Except: []string{"10.9.0.0/24"}}}}}}},
		{"a bad selector", "podSelector", sandboxv1alpha1.SandboxNetwork{Ports: []sandboxv1alpha1.SandboxPort{http},
			Ingress: &sandboxv1alpha1.SandboxIngress{From: []networkingv1.NetworkPolicyPeer{{
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"bad key!": "x"}}}}}}},
	} {
		err := ValidateNetwork(tc.n)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestValidatePodMetadata(t *testing.T) {
	ok := &sandboxv1alpha1.SandboxPodMetadata{
		Labels:      map[string]string{"core.spinkube.dev/app.hello.status": "ready", "app": "hello"},
		Annotations: map[string]string{"prometheus.io/scrape": "true", "example.com/note": "anything at all"},
	}
	if err := ValidatePodMetadata(ok); err != nil {
		t.Errorf("unexpected error %v", err)
	}
	if err := ValidatePodMetadata(nil); err != nil {
		t.Errorf("nil: %v", err)
	}

	for _, tc := range []struct {
		name string
		m    sandboxv1alpha1.SandboxPodMetadata
	}{
		{"the sandbox label", sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{"sandbox.kubeswift.io/sandbox": "other"}}},
		{"the slot label", sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{"sandbox.kubeswift.io/slot": "p-slot-x"}}},
		{"a kubeswift.io label", sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{"kubeswift.io/gpu-node": "true"}}},
		{"a kubeswift.io annotation", sandboxv1alpha1.SandboxPodMetadata{Annotations: map[string]string{"kubeswift.io/sandbox-exec-action": "run"}}},
		{"a Multus network", sandboxv1alpha1.SandboxPodMetadata{Annotations: map[string]string{"k8s.v1.cni.cncf.io/networks": "evil"}}},
		{"a Multus default network", sandboxv1alpha1.SandboxPodMetadata{Annotations: map[string]string{"v1.multus-cni.io/default-network": "x"}}},
		{"an OVN-Kubernetes annotation", sandboxv1alpha1.SandboxPodMetadata{Annotations: map[string]string{"k8s.ovn.org/pod-networks": "{}"}}},
		{"an invalid label value", sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{"app": "not a valid value!"}}},
		{"an invalid annotation key", sandboxv1alpha1.SandboxPodMetadata{Annotations: map[string]string{"bad key": "x"}}},
	} {
		m := tc.m
		if err := ValidatePodMetadata(&m); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
	// A domain that merely ends in "kubeswift.io" without the dot is not ours.
	if err := ValidatePodMetadata(&sandboxv1alpha1.SandboxPodMetadata{Labels: map[string]string{"notkubeswift.io/x": "y"}}); err != nil {
		t.Errorf("notkubeswift.io is not a KubeSwift domain: %v", err)
	}
}
