package swiftguest

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// ovnkNADObj builds an ovn-k8s-cni-overlay NAD with the given topology and OVN
// network name (the config "name", which the IPAMClaim references).
func ovnkNADObj(ns, nadName, ovnNetwork, topology string) *unstructured.Unstructured {
	cfg := `{"cniVersion":"0.3.1","type":"ovn-k8s-cni-overlay"`
	if ovnNetwork != "" {
		cfg += `,"name":"` + ovnNetwork + `"`
	}
	if topology != "" {
		cfg += `,"topology":"` + topology + `"`
	}
	cfg += `,"netAttachDefName":"` + ns + `/` + nadName + `","allowPersistentIPs":true}`
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	u.SetNamespace(ns)
	u.SetName(nadName)
	_ = unstructured.SetNestedField(u.Object, cfg, "spec", "config")
	return u
}

// Detect matches only ovn-k8s-cni-overlay + layer2 + a named OVN network.
func TestOVNKubernetesDetect_Matrix(t *testing.T) {
	cases := []struct {
		name    string
		nad     *unstructured.Unstructured
		wantOK  bool
		wantErr bool
	}{
		{"layer2+name", ovnkNADObj("ns", "n", "ovnnet", "layer2"), true, false},
		{"no-topology", ovnkNADObj("ns", "n", "ovnnet", ""), false, false},
		{"layer3", ovnkNADObj("ns", "n", "ovnnet", "layer3"), false, false},
		{"layer2-no-name", ovnkNADObj("ns", "n", "", "layer2"), false, false},
		{"kube-ovn-type", nadObj("ns", "n", "kube-ovn", ""), false, false},
		{"bridge-type", nadObj("ns", "n", "bridge", ""), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guest := guestWithPrimaryNAD("ns", "g", "n", "52:54:00:00:00:01")
			c := nadAwareClientBuilder(tc.nad).Build()
			ok, err := (ovnKubernetesBackend{}).Detect(context.Background(), c, guest)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Errorf("Detect ok=%v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// Node-local primary (no networkRef) -> Detect false, no NAD Get.
func TestOVNKubernetesDetect_NodeLocal(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "g"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{Interfaces: []swiftv1alpha1.GuestInterface{{Name: "mgmt"}}},
	}
	c := nadAwareClientBuilder().Build()
	ok, err := (ovnKubernetesBackend{}).Detect(context.Background(), c, guest)
	if err != nil || ok {
		t.Fatalf("node-local: ok=%v err=%v, want false,nil", ok, err)
	}
}

// Identity injects mac + ipam-claim-reference into the PRIMARY Multus entry and
// returns the IPAMClaim to ensure; MigrationDstAnnotations is empty (the dst
// inherits the Multus annotation + default overlap).
func TestOVNKubernetesIdentity_InjectsMacAndClaim(t *testing.T) {
	guest := guestWithPrimaryNAD("ovn-ns", "ovnk-vm", "ovnk-nad", "52:54:00:ab:cd:ef")
	c := nadAwareClientBuilder(ovnkNADObj("ovn-ns", "ovnk-nad", "ovnknet", "layer2")).Build()

	id, err := (ovnKubernetesBackend{}).Identity(context.Background(), c, guest, "ignored-mig")
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if len(id.MigrationDstAnnotations) != 0 {
		t.Errorf("MigrationDstAnnotations must be empty for OVN-K; got %v", id.MigrationDstAnnotations)
	}

	// PodAnnotations: the networks annotation with mac + ipam-claim-reference on the
	// primary (net1) entry.
	var entries []multusNetworkEntry
	if err := json.Unmarshal([]byte(id.PodAnnotations[MultusAnnotationKey]), &entries); err != nil {
		t.Fatalf("unmarshal networks annotation: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	if entries[0].Interface != "net1" {
		t.Errorf("primary interface = %q, want net1", entries[0].Interface)
	}
	if entries[0].MAC != "52:54:00:ab:cd:ef" {
		t.Errorf("primary MAC = %q, want the guest MAC", entries[0].MAC)
	}
	if entries[0].IPAMClaimReference != "ovnk-vm-net1" {
		t.Errorf("ipam-claim-reference = %q, want ovnk-vm-net1", entries[0].IPAMClaimReference)
	}

	// ClaimsToEnsure: one IPAMClaim with the right GVK, name, ns and spec.
	if len(id.ClaimsToEnsure) != 1 {
		t.Fatalf("want 1 claim, got %d", len(id.ClaimsToEnsure))
	}
	claim := id.ClaimsToEnsure[0]
	if claim.GroupVersionKind() != ipamClaimGVK {
		t.Errorf("claim GVK = %v, want %v", claim.GroupVersionKind(), ipamClaimGVK)
	}
	if claim.GetName() != "ovnk-vm-net1" || claim.GetNamespace() != "ovn-ns" {
		t.Errorf("claim = %s/%s, want ovn-ns/ovnk-vm-net1", claim.GetNamespace(), claim.GetName())
	}
	if net, _, _ := unstructured.NestedString(claim.Object, "spec", "network"); net != "ovnknet" {
		t.Errorf("claim spec.network = %q, want ovnknet (the OVN network name)", net)
	}
	if iface, _, _ := unstructured.NestedString(claim.Object, "spec", "interface"); iface != "net1" {
		t.Errorf("claim spec.interface = %q, want net1", iface)
	}
}

// OVNMigrationDstAnnotations is empty for an OVN-K guest (the carried-over Multus
// annotation IS the dst contract; overlap allowed by default).
func TestOVNMigrationDstAnnotations_OVNKubernetes_Empty(t *testing.T) {
	guest := guestWithPrimaryNAD("ovn-ns", "ovnk-vm", "ovnk-nad", "52:54:00:ab:cd:ef")
	c := nadAwareClientBuilder(ovnkNADObj("ovn-ns", "ovnk-nad", "ovnknet", "layer2")).Build()
	out, err := OVNMigrationDstAnnotations(context.Background(), c, guest, "ovnk-vm-live")
	if err != nil {
		t.Fatalf("OVNMigrationDstAnnotations: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("OVN-K dst annotations must be empty (Multus carry-over handles it); got %v", out)
	}
}

// ovnkDispatchScheme has SwiftGuest (for the claim owner-ref) + the NAD/IPAMClaim
// unstructured GVKs (for the NAD Get + the claim Create). A fresh scheme — no
// mutation of the shared internal/scheme.Scheme.
func ovnkDispatchScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift, &swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{})
	metav1.AddToGroupVersion(s, gvSwift)
	s.AddKnownTypeWithName(networkAttachmentDefinitionGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(networkAttachmentDefinitionGVK.GroupVersion().WithKind("NetworkAttachmentDefinitionList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(ipamClaimGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(ipamClaimGVK.GroupVersion().WithKind("IPAMClaimList"), &unstructured.UnstructuredList{})
	return s
}

// The full boot dispatch: stampOVNIdentity on an OVN-K guest creates the IPAMClaim
// (owner-referenced to the guest, for GC) and stamps the augmented Multus annotation
// onto the pod.
func TestStampOVNIdentity_OVNKubernetes_CreatesClaimAndStamps(t *testing.T) {
	s := ovnkDispatchScheme()
	guest := guestWithPrimaryNAD("ovn-ns", "ovnk-vm", "ovnk-nad", "52:54:00:ab:cd:ef")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ovnkNADObj("ovn-ns", "ovnk-nad", "ovnknet", "layer2")).Build()
	r := &SwiftGuestReconciler{Client: c, Scheme: s}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ovn-ns", Name: "p"}}
	if err := r.stampOVNIdentity(context.Background(), guest, pod); err != nil {
		t.Fatalf("stampOVNIdentity: %v", err)
	}

	// The IPAMClaim was created with the right spec + owner-ref to the guest.
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(ipamClaimGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ovn-ns", Name: "ovnk-vm-net1"}, got); err != nil {
		t.Fatalf("IPAMClaim not created: %v", err)
	}
	if net, _, _ := unstructured.NestedString(got.Object, "spec", "network"); net != "ovnknet" {
		t.Errorf("created claim spec.network = %q, want ovnknet", net)
	}
	owners := got.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "SwiftGuest" || owners[0].Name != "ovnk-vm" {
		t.Errorf("claim owner-refs = %+v, want one controller ref to SwiftGuest/ovnk-vm", owners)
	}

	// The pod's networks annotation carries the injected mac + ipam-claim-reference.
	var entries []multusNetworkEntry
	if err := json.Unmarshal([]byte(pod.Annotations[MultusAnnotationKey]), &entries); err != nil {
		t.Fatalf("pod networks annotation: %v", err)
	}
	if len(entries) != 1 || entries[0].MAC != "52:54:00:ab:cd:ef" || entries[0].IPAMClaimReference != "ovnk-vm-net1" {
		t.Errorf("pod networks entry not augmented: %+v", entries)
	}
}

// First-match-wins dispatch over disjoint NAD types: a kube-ovn guest resolves to
// the kube-ovn backend, an OVN-K guest to the ovn-kubernetes backend.
func TestResolveOVNBackend_DisjointTypes(t *testing.T) {
	koGuest := guestWithPrimaryNAD("ns", "g", "ko", "52:54:00:00:00:02")
	koClient := nadAwareClientBuilder(nadObj("ns", "ko", "kube-ovn", "ko.ns.ovn")).Build()
	if b, err := resolveOVNBackend(context.Background(), koClient, koGuest); err != nil || b == nil || b.Name() != "kube-ovn" {
		t.Errorf("kube-ovn guest resolved to %v (err %v), want kube-ovn", b, err)
	}

	okGuest := guestWithPrimaryNAD("ns", "g", "ok", "52:54:00:00:00:03")
	okClient := nadAwareClientBuilder(ovnkNADObj("ns", "ok", "oknet", "layer2")).Build()
	if b, err := resolveOVNBackend(context.Background(), okClient, okGuest); err != nil || b == nil || b.Name() != "ovn-kubernetes" {
		t.Errorf("ovn-k guest resolved to %v (err %v), want ovn-kubernetes", b, err)
	}
}
