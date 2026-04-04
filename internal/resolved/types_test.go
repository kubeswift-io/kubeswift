package resolved

import (
	"testing"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
	"k8s.io/apimachinery/pkg/types"
)

func TestGetNICs_NilInterfaces(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: nil,
		Meta:       Meta{Namespace: "default", Name: "test"},
	}
	nics := rg.GetNICs()
	if nics != nil {
		t.Errorf("GetNICs(nil interfaces) = %v, want nil", nics)
	}
}

func TestGetNICs_EmptyInterfaces(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: []swiftv1alpha1.GuestInterface{},
		Meta:       Meta{Namespace: "default", Name: "test"},
	}
	nics := rg.GetNICs()
	if nics != nil {
		t.Errorf("GetNICs(empty interfaces) = %v, want nil", nics)
	}
}

func TestGetNICs_SinglePrimary(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt", NetworkRef: nil},
		},
		Meta: Meta{Namespace: "default", Name: "myguest", UID: types.UID("uid-1")},
	}
	nics := rg.GetNICs()
	if len(nics) != 1 {
		t.Fatalf("GetNICs = %d NICs, want 1", len(nics))
	}
	nic := nics[0]
	if nic.Name != "mgmt" {
		t.Errorf("Name = %q, want mgmt", nic.Name)
	}
	if nic.TapDevice != "tap0" {
		t.Errorf("TapDevice = %q, want tap0", nic.TapDevice)
	}
	if nic.Bridge != "br0" {
		t.Errorf("Bridge = %q, want br0", nic.Bridge)
	}
	if !nic.Primary {
		t.Error("Primary = false, want true")
	}
	if nic.MultusInterface != "" {
		t.Errorf("MultusInterface = %q, want empty", nic.MultusInterface)
	}
	if nic.MAC == "" {
		t.Error("MAC is empty")
	}
}

func TestGetNICs_MultipleInterfaces(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt", NetworkRef: nil},
			{Name: "data1", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-a"}},
			{Name: "data2", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-b"}},
		},
		Meta: Meta{Namespace: "default", Name: "multinic", UID: types.UID("uid-2")},
	}
	nics := rg.GetNICs()
	if len(nics) != 3 {
		t.Fatalf("GetNICs = %d NICs, want 3", len(nics))
	}

	// Primary NIC
	if !nics[0].Primary {
		t.Error("nics[0].Primary = false, want true")
	}
	if nics[0].TapDevice != "tap0" {
		t.Errorf("nics[0].TapDevice = %q, want tap0", nics[0].TapDevice)
	}
	if nics[0].Bridge != "br0" {
		t.Errorf("nics[0].Bridge = %q, want br0", nics[0].Bridge)
	}
	if nics[0].MultusInterface != "" {
		t.Errorf("nics[0].MultusInterface = %q, want empty", nics[0].MultusInterface)
	}

	// First secondary
	if nics[1].Primary {
		t.Error("nics[1].Primary = true, want false")
	}
	if nics[1].TapDevice != "tap1" {
		t.Errorf("nics[1].TapDevice = %q, want tap1", nics[1].TapDevice)
	}
	if nics[1].Bridge != "br1" {
		t.Errorf("nics[1].Bridge = %q, want br1", nics[1].Bridge)
	}
	if nics[1].MultusInterface != "net1" {
		t.Errorf("nics[1].MultusInterface = %q, want net1", nics[1].MultusInterface)
	}

	// Second secondary
	if nics[2].Primary {
		t.Error("nics[2].Primary = true, want false")
	}
	if nics[2].TapDevice != "tap2" {
		t.Errorf("nics[2].TapDevice = %q, want tap2", nics[2].TapDevice)
	}
	if nics[2].Bridge != "br2" {
		t.Errorf("nics[2].Bridge = %q, want br2", nics[2].Bridge)
	}
	if nics[2].MultusInterface != "net2" {
		t.Errorf("nics[2].MultusInterface = %q, want net2", nics[2].MultusInterface)
	}
}

func TestGetNICs_MACDeterminism(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "mgmt", NetworkRef: nil},
			{Name: "data", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-a"}},
		},
		Meta: Meta{Namespace: "default", Name: "mactest", UID: types.UID("uid-3")},
	}

	nics1 := rg.GetNICs()
	nics2 := rg.GetNICs()

	for i := range nics1 {
		if nics1[i].MAC != nics2[i].MAC {
			t.Errorf("NIC %d MAC not deterministic: %q != %q", i, nics1[i].MAC, nics2[i].MAC)
		}
	}

	// Also verify against direct generation
	expectedMAC := runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed("default", "mactest", "mgmt"))
	if nics1[0].MAC != expectedMAC {
		t.Errorf("NIC 0 MAC = %q, want %q (from GenerateMAC)", nics1[0].MAC, expectedMAC)
	}
}

func TestGetNICs_MACUniqueness(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "eth0", NetworkRef: nil},
			{Name: "eth1", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-a"}},
			{Name: "eth2", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "net-b"}},
		},
		Meta: Meta{Namespace: "default", Name: "uniq", UID: types.UID("uid-4")},
	}
	nics := rg.GetNICs()

	seen := make(map[string]string) // mac -> interface name
	for _, nic := range nics {
		if prev, ok := seen[nic.MAC]; ok {
			t.Errorf("MAC collision between interfaces %q and %q: %s", prev, nic.Name, nic.MAC)
		}
		seen[nic.MAC] = nic.Name
	}
}
