package resolved

import (
	"context"
	"testing"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testNAD(ns, name, config string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, config, "spec", "config")
	return u
}

// nadScheme is testScheme plus the NAD kind, which the resolver Gets unstructured.
func nadScheme() *runtime.Scheme {
	s := testScheme()
	s.AddKnownTypeWithName(networkAttachmentDefinitionGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(networkAttachmentDefinitionGVK.GroupVersion().WithKind("NetworkAttachmentDefinitionList"), &unstructured.UnstructuredList{})
	return s
}

func TestNADGateway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "whereabouts gateway",
			config: `{"type":"ovs","ipam":{"type":"whereabouts","range":"10.10.0.0/24","gateway":"10.10.0.1"}}`,
			want:   "10.10.0.1",
		},
		{
			name:   "host-local ranges gateway",
			config: `{"type":"ovs","ipam":{"type":"host-local","ranges":[[{"subnet":"10.10.0.0/24","gateway":"10.10.0.1"}]]}}`,
			want:   "10.10.0.1",
		},
		{
			name:   "static addresses gateway",
			config: `{"type":"bridge","ipam":{"type":"static","addresses":[{"address":"10.10.0.5/24","gateway":"10.10.0.1"}]}}`,
			want:   "10.10.0.1",
		},
		{
			name:   "conflist plugin gateway",
			config: `{"cniVersion":"0.4.0","plugins":[{"type":"ovs","ipam":{"type":"whereabouts","gateway":"10.10.0.1"}},{"type":"tuning"}]}`,
			want:   "10.10.0.1",
		},
		{
			name:   "no gateway",
			config: `{"type":"ovs","ipam":{"type":"whereabouts","range":"10.10.0.0/24"}}`,
		},
		{
			name:   "no ipam",
			config: `{"type":"ovs","trunk":[{"minID":200,"maxID":299}]}`,
		},
		{
			name:   "invalid config",
			config: `{`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(nadScheme()).WithObjects(testNAD("ns", "net", tc.config)).Build()
			got, err := NADGateway(context.Background(), c, "ns", &swiftv1alpha1.NetworkReference{Name: "net"})
			if err != nil {
				t.Fatalf("NADGateway: %v", err)
			}
			if got != tc.want {
				t.Errorf("NADGateway = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNADGateway_ReferenceNamespace(t *testing.T) {
	nad := testNAD("shared", "net", `{"type":"ovs","ipam":{"type":"whereabouts","gateway":"10.10.0.1"}}`)
	c := fake.NewClientBuilder().WithScheme(nadScheme()).WithObjects(nad).Build()
	got, err := NADGateway(context.Background(), c, "ns", &swiftv1alpha1.NetworkReference{Name: "net", Namespace: "shared"})
	if err != nil {
		t.Fatalf("NADGateway: %v", err)
	}
	if got != "10.10.0.1" {
		t.Errorf("NADGateway = %q, want 10.10.0.1 from the referenced namespace", got)
	}
}

// A missing NAD is not a resolution error: the pod cannot start without it anyway.
func TestNADGateway_MissingNAD(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(nadScheme()).Build()
	got, err := NADGateway(context.Background(), c, "ns", &swiftv1alpha1.NetworkReference{Name: "ghost"})
	if err != nil || got != "" {
		t.Errorf("NADGateway = %q, %v, want empty and no error", got, err)
	}
}

// resolveWithNADs resolves a disk-boot guest in namespace "ns" with the given
// interfaces and NADs.
func resolveWithNADs(t *testing.T, ifaces []swiftv1alpha1.GuestInterface, nads ...client.Object) *ResolvedGuest {
	t.Helper()
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
	objs := append([]client.Object{guestClass, image, ns}, nads...)
	c := fake.NewClientBuilder().WithScheme(nadScheme()).WithObjects(objs...).Build()
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", UID: "uid-123"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: "img"},
			GuestClassRef: corev1.LocalObjectReference{Name: "gc"},
			Interfaces:    ifaces,
		},
	}
	rg, err := NewResolver(c).Resolve(context.Background(), guest)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return rg
}

// A primary-on-NAD guest carries its NAD's IPAM gateway on the primary NIC only.
func TestResolve_PrimaryOnNADGateway(t *testing.T) {
	rg := resolveWithNADs(t,
		[]swiftv1alpha1.GuestInterface{
			{Name: "mgmt", Primary: true, NetworkRef: &swiftv1alpha1.NetworkReference{Name: "mgmt-net"}},
			{Name: "storage", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "storage-net"}},
		},
		testNAD("ns", "mgmt-net", `{"type":"ovs","ipam":{"type":"whereabouts","range":"10.10.0.0/24","gateway":"10.10.0.1"}}`),
		testNAD("ns", "storage-net", `{"type":"ovs","ipam":{"type":"whereabouts","range":"172.30.0.0/16","gateway":"172.30.0.1"}}`),
	)
	if rg.PrimaryNADGateway != "10.10.0.1" {
		t.Errorf("PrimaryNADGateway = %q, want 10.10.0.1", rg.PrimaryNADGateway)
	}
	nics := rg.GetNICs()
	if len(nics) != 2 {
		t.Fatalf("GetNICs = %d NICs, want 2", len(nics))
	}
	if nics[0].Gateway != "10.10.0.1" {
		t.Errorf("primary NIC Gateway = %q, want 10.10.0.1", nics[0].Gateway)
	}
	if nics[1].Gateway != "" {
		t.Errorf("secondary NIC Gateway = %q, want empty (the primary owns the default route)", nics[1].Gateway)
	}
}

// A node-local primary never looks up a NAD gateway.
func TestResolve_NodeLocalPrimaryHasNoNADGateway(t *testing.T) {
	rg := resolveWithNADs(t,
		[]swiftv1alpha1.GuestInterface{
			{Name: "mgmt", Primary: true},
			{Name: "node", NetworkRef: &swiftv1alpha1.NetworkReference{Name: "node-net"}},
		},
		testNAD("ns", "node-net", `{"type":"ovs","ipam":{"type":"whereabouts","gateway":"10.20.0.1"}}`),
	)
	if rg.PrimaryNADGateway != "" {
		t.Errorf("PrimaryNADGateway = %q, want empty for a node-local primary", rg.PrimaryNADGateway)
	}
	for _, nic := range rg.GetNICs() {
		if nic.Gateway != "" {
			t.Errorf("NIC %s Gateway = %q, want empty", nic.Name, nic.Gateway)
		}
	}
}
