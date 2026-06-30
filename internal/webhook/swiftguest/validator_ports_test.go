package swiftguest

import (
	"testing"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

func withNetwork(n *swiftv1alpha1.GuestNetworkSpec) func(*swiftv1alpha1.SwiftGuest) {
	return func(g *swiftv1alpha1.SwiftGuest) { g.Spec.Network = n }
}

func TestValidate_NetworkPorts(t *testing.T) {
	// nil network: OK (today's behavior).
	if err := validateSwiftGuest(guest(nil)); err != nil {
		t.Fatalf("nil network should be valid: %v", err)
	}

	// single exposed port, nat: OK.
	if err := validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Binding: "nat",
		Ports:   []swiftv1alpha1.GuestPort{{Port: 22, Expose: "ClusterIP"}},
	}))); err != nil {
		t.Errorf("single nat ClusterIP port should be valid: %v", err)
	}

	// two named exposed ports, same type: OK.
	if err := validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Ports: []swiftv1alpha1.GuestPort{
			{Name: "ssh", Port: 22, Expose: "ClusterIP"},
			{Name: "http", Port: 80, Expose: "ClusterIP"},
		},
	}))); err != nil {
		t.Errorf("two named ClusterIP ports should be valid: %v", err)
	}

	// ports without expose: OK (DNAT-only).
	if err := validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Ports: []swiftv1alpha1.GuestPort{{Port: 22}},
	}))); err != nil {
		t.Errorf("exposeless port should be valid: %v", err)
	}

	// ports without expose on a bridge guest: OK (NetworkPolicy targeting — decision §12.4).
	if err := validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Binding: "bridge",
		Ports:   []swiftv1alpha1.GuestPort{{Port: 22}},
	}))); err != nil {
		t.Errorf("exposeless port on bridge should be valid: %v", err)
	}

	// >1 port, missing name: rejected.
	errContains(t, validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Ports: []swiftv1alpha1.GuestPort{{Port: 22, Expose: "ClusterIP"}, {Port: 80, Expose: "ClusterIP"}},
	}))), "name is required when more than one port")

	// duplicate proto/port: rejected.
	errContains(t, validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Ports: []swiftv1alpha1.GuestPort{{Name: "a", Port: 22}, {Name: "b", Port: 22}},
	}))), "duplicate TCP port 22")

	// expose on a bridge guest: rejected.
	errContains(t, validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Binding: "bridge",
		Ports:   []swiftv1alpha1.GuestPort{{Port: 22, Expose: "ClusterIP"}},
	}))), "binding is bridge")

	// mixed expose values: rejected (one Service, one type).
	errContains(t, validateSwiftGuest(guest(withNetwork(&swiftv1alpha1.GuestNetworkSpec{
		Ports: []swiftv1alpha1.GuestPort{
			{Name: "a", Port: 22, Expose: "ClusterIP"},
			{Name: "b", Port: 80, Expose: "LoadBalancer"},
		},
	}))), "same expose value")

	// expose with an sriov primary is rejected — caught earlier by
	// validateInterfaces (a primary sriov interface is invalid outright), so the
	// exposure can never reach a tap-less primary. validateNetworkPorts also
	// guards it as defense-in-depth.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.Interfaces = []swiftv1alpha1.GuestInterface{{Name: "p", Type: swiftv1alpha1.InterfaceTypeSRIOV, Primary: true}}
		g.Spec.Network = &swiftv1alpha1.GuestNetworkSpec{Ports: []swiftv1alpha1.GuestPort{{Port: 22, Expose: "ClusterIP"}}}
	})), "bridge interface")
}
