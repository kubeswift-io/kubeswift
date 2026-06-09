package v1alpha1

import "testing"

func ifaceNAD(name string) *NetworkReference { return &NetworkReference{Name: name} }

func TestPrimaryInterface(t *testing.T) {
	cases := []struct {
		name  string
		ifs   []GuestInterface
		want  string // primary interface name, "" for nil
		preIP bool   // PrimaryIPPreservedCrossNode
	}{
		{"none", nil, "", false},
		{"single node-local", []GuestInterface{{Name: "mgmt"}}, "mgmt", false},
		{"node-local + secondary NAD", []GuestInterface{{Name: "mgmt"}, {Name: "data", NetworkRef: ifaceNAD("n")}}, "mgmt", false},
		{"single NAD (de-facto primary)", []GuestInterface{{Name: "data", NetworkRef: ifaceNAD("n")}}, "data", true},
		{"explicit primary on NAD", []GuestInterface{{Name: "a"}, {Name: "p", Primary: true, NetworkRef: ifaceNAD("n")}}, "p", true},
		{"explicit primary node-local", []GuestInterface{{Name: "p", Primary: true}, {Name: "data", NetworkRef: ifaceNAD("n")}}, "p", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &SwiftGuest{Spec: SwiftGuestSpec{Interfaces: tc.ifs}}
			p := g.PrimaryInterface()
			got := ""
			if p != nil {
				got = p.Name
			}
			if got != tc.want {
				t.Errorf("PrimaryInterface = %q, want %q", got, tc.want)
			}
			if g.PrimaryIPPreservedCrossNode() != tc.preIP {
				t.Errorf("PrimaryIPPreservedCrossNode = %v, want %v", g.PrimaryIPPreservedCrossNode(), tc.preIP)
			}
		})
	}
}

// vhost-user/sriov are never the fallback primary.
func TestPrimaryInterface_SkipsNonBridge(t *testing.T) {
	g := &SwiftGuest{Spec: SwiftGuestSpec{Interfaces: []GuestInterface{
		{Name: "fast", Type: InterfaceTypeVhostUser, Socket: "/s"},
		{Name: "rdma", Type: InterfaceTypeSRIOV, NetworkRef: ifaceNAD("sriov")},
	}}}
	if p := g.PrimaryInterface(); p != nil {
		t.Errorf("PrimaryInterface = %q, want nil (no bridge interface)", p.Name)
	}
}

func TestHasNodeLocalVirtioBackends(t *testing.T) {
	hp := "/srv/x"
	cases := []struct {
		name string
		spec SwiftGuestSpec
		want bool
	}{
		{"none", SwiftGuestSpec{}, false},
		{"bridge-only interfaces", SwiftGuestSpec{Interfaces: []GuestInterface{{Name: "mgmt"}}}, false},
		{"virtiofs", SwiftGuestSpec{Filesystems: []Filesystem{{Name: "d", Source: FilesystemSource{HostPath: &hp}}}}, true},
		{"vhost-user device", SwiftGuestSpec{VhostUserDevices: []VhostUserDevice{{Name: "b", Type: VhostUserDeviceTypeBlk, Socket: "/s"}}}, true},
		{"vhost-user NIC", SwiftGuestSpec{Interfaces: []GuestInterface{{Name: "f", Type: InterfaceTypeVhostUser, Socket: "/s"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &SwiftGuest{Spec: tc.spec}
			if got := g.HasNodeLocalVirtioBackends(); got != tc.want {
				t.Errorf("HasNodeLocalVirtioBackends = %v, want %v", got, tc.want)
			}
		})
	}
}
