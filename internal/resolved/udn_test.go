package resolved

import (
	"context"
	"testing"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// resolveModelA resolves a disk-boot guest in namespace "ns", optionally carrying
// the OVN-K primary-UDN label, with the given interfaces, and returns the
// ResolvedGuest. Model A eligibility is computed in the resolver (not GetNICs) so
// it covers the DEFAULT guest (no spec.interfaces) — the case the PR1 per-NIC
// plumbing missed and the cluster check caught.
func resolveModelA(t *testing.T, nsLabeled bool, ifaces []swiftv1alpha1.GuestInterface) *ResolvedGuest {
	t.Helper()
	scheme := testScheme()
	guestClass := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gc"},
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			CPU: resource.MustParse("2"), Memory: resource.MustParse("2Gi"),
			RootDisk: swiftv1alpha1.RootDiskSpec{Size: resource.MustParse("10Gi"), Format: swiftv1alpha1.DiskFormatRaw},
		},
	}
	image := &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: "img", Namespace: "ns"},
		Status:     imagev1alpha1.SwiftImageStatus{Phase: imagev1alpha1.SwiftImagePhaseReady, PreparedArtifact: &imagev1alpha1.PreparedArtifactRef{Format: imagev1alpha1.DiskFormatRaw}},
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}
	if nsLabeled {
		ns.Labels = map[string]string{OVNPrimaryUDNNamespaceLabel: ""}
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guestClass, image, ns).Build()
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", UID: "uid-123"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
			Interfaces:    ifaces,
		},
	}
	rg, err := NewResolver(client).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return rg
}

// A DEFAULT guest (no spec.interfaces) in a primary-UDN namespace rides ovn-udn1.
// GetNICs returns nil for such a guest, so the signal MUST be top-level — this is
// exactly the gap the PR1 cluster check found.
func TestResolve_ModelA_DefaultGuest(t *testing.T) {
	rg := resolveModelA(t, true, nil)
	if rg.PrimaryUDNInterface != OVNPrimaryUDNInterface {
		t.Errorf("PrimaryUDNInterface = %q, want %q", rg.PrimaryUDNInterface, OVNPrimaryUDNInterface)
	}
	if got := rg.GetPrimaryUDNInterface(); got != OVNPrimaryUDNInterface {
		t.Errorf("GetPrimaryUDNInterface() = %q, want %q", got, OVNPrimaryUDNInterface)
	}
}

// An explicit node-local primary (no networkRef) in a primary-UDN namespace also
// rides ovn-udn1.
func TestResolve_ModelA_ExplicitNodeLocalPrimary(t *testing.T) {
	rg := resolveModelA(t, true, []swiftv1alpha1.GuestInterface{{Name: "app", Primary: true}})
	if rg.PrimaryUDNInterface != OVNPrimaryUDNInterface {
		t.Errorf("PrimaryUDNInterface = %q, want %q", rg.PrimaryUDNInterface, OVNPrimaryUDNInterface)
	}
}

// A primary that rides a NAD is Model B (primary-on-NAD), NOT Model A — even in a
// primary-UDN namespace. The resolver's node-local gate skips it.
func TestResolve_ModelA_PrimaryOnNADSkipped(t *testing.T) {
	rg := resolveModelA(t, true, []swiftv1alpha1.GuestInterface{
		{Name: "app", Primary: true, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "ovn-l2"}},
	})
	if rg.PrimaryUDNInterface != "" {
		t.Errorf("PrimaryUDNInterface = %q, want empty (primary rides a NAD = Model B)", rg.PrimaryUDNInterface)
	}
}

// Outside a primary-UDN namespace, the signal is never set.
func TestResolve_ModelA_NonUDNNamespace(t *testing.T) {
	rg := resolveModelA(t, false, nil)
	if rg.PrimaryUDNInterface != "" {
		t.Errorf("PrimaryUDNInterface = %q, want empty (namespace not labelled)", rg.PrimaryUDNInterface)
	}
	if got := rg.GetPrimaryUDNInterface(); got != "" {
		t.Errorf("GetPrimaryUDNInterface() = %q, want empty", got)
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
