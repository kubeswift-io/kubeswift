package resolved

import (
	"context"
	"testing"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Model A: a primary interface with no networkRef in a primary-UDN namespace
// rides ovn-udn1. The resolver sets ResolvedGuest.PrimaryUDNInterface; GetNICs
// applies it to that NIC (and not a Multus interface — the UDN is namespace-bound).
func TestGetNICs_ModelA_PrimaryUDNAppliedToNodeLocalPrimary(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces:          []swiftv1alpha1.GuestInterface{{Name: "app", Primary: true}},
		PrimaryUDNInterface: OVNPrimaryUDNInterface,
		Meta:                Meta{Namespace: "model-a", Name: "vm", UID: types.UID("uid")},
	}
	nics := rg.GetNICs()
	if len(nics) != 1 {
		t.Fatalf("GetNICs = %d NICs, want 1", len(nics))
	}
	if !nics[0].Primary {
		t.Error("primary NIC not marked primary")
	}
	if nics[0].PrimaryUDNInterface != OVNPrimaryUDNInterface {
		t.Errorf("PrimaryUDNInterface = %q, want %q", nics[0].PrimaryUDNInterface, OVNPrimaryUDNInterface)
	}
	if nics[0].MultusInterface != "" {
		t.Errorf("MultusInterface = %q, want empty (primary UDN is namespace-bound, not a NAD)", nics[0].MultusInterface)
	}
}

// A primary that rides a secondary NAD keeps net1; the namespace UDN signal does
// NOT override it (primary-on-NAD stays on its own path — Model B coexistence).
func TestGetNICs_ModelA_NotAppliedToPrimaryOnNAD(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: []swiftv1alpha1.GuestInterface{
			{Name: "app", Primary: true, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "ovn-l2"}},
		},
		PrimaryUDNInterface: OVNPrimaryUDNInterface,
		Meta:                Meta{Namespace: "model-a", Name: "vm", UID: types.UID("uid")},
	}
	nics := rg.GetNICs()
	if nics[0].PrimaryUDNInterface != "" {
		t.Errorf("PrimaryUDNInterface = %q, want empty (primary rides the NAD)", nics[0].PrimaryUDNInterface)
	}
	if nics[0].MultusInterface != "net1" {
		t.Errorf("MultusInterface = %q, want net1", nics[0].MultusInterface)
	}
}

// Regression: outside a primary-UDN namespace (PrimaryUDNInterface unset), a
// node-local primary stays node-local.
func TestGetNICs_NoPrimaryUDN_Unset(t *testing.T) {
	rg := &ResolvedGuest{
		Interfaces: []swiftv1alpha1.GuestInterface{{Name: "app", Primary: true}},
		Meta:       Meta{Namespace: "default", Name: "vm", UID: types.UID("uid")},
	}
	if nics := rg.GetNICs(); nics[0].PrimaryUDNInterface != "" {
		t.Errorf("PrimaryUDNInterface = %q, want empty", nics[0].PrimaryUDNInterface)
	}
}

func TestNamespaceHasPrimaryUDN(t *testing.T) {
	s := testScheme()
	labeled := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Labels: map[string]string{OVNPrimaryUDNNamespaceLabel: ""}}}
	plain := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(labeled, plain).Build()

	for _, tc := range []struct {
		ns   string
		want bool
	}{
		{"tenant", true},   // labeled → Model A
		{"default", false}, // unlabeled
		{"ghost", false},   // missing → NotFound → false, no error (resolution proceeds)
	} {
		ok, err := NamespaceHasPrimaryUDN(context.Background(), c, tc.ns)
		if err != nil {
			t.Errorf("ns %q: unexpected err %v", tc.ns, err)
		}
		if ok != tc.want {
			t.Errorf("ns %q: ok = %v, want %v", tc.ns, ok, tc.want)
		}
	}
}
