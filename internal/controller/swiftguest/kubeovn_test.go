package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

func nadObj(ns, name, cniType, provider string) *unstructured.Unstructured {
	cfg := `{"cniVersion":"0.3.1","name":"` + name + `","type":"` + cniType + `"`
	if provider != "" {
		cfg += `,"provider":"` + provider + `"`
	}
	cfg += `}`
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	_ = unstructured.SetNestedField(u.Object, cfg, "spec", "config")
	return u
}

func nadAwareClientBuilder(objs ...*unstructured.Unstructured) *fake.ClientBuilder {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(networkAttachmentDefinitionGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(
		networkAttachmentDefinitionGVK.GroupVersion().WithKind("NetworkAttachmentDefinitionList"),
		&unstructured.UnstructuredList{},
	)
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b
}

func guestWithPrimaryNAD(ns, name, nadName, mac string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{
				{Name: "app", Primary: true, MAC: mac, NetworkRef: &swiftv1alpha1.NetworkReference{Name: nadName}},
			},
		},
	}
}

// --- boot-time pod identity (stampOVNIdentity → kubeOVNBackend.Identity PodAnnotations) ---

// kube-ovn-class primary NAD -> the pod gets the provider-scoped mac_address (the
// guest's MAC) and, once status carries an IP, the pinned ip_address.
func TestStampOVNIdentity_KubeOVNPrimary_StampsMacAndIP(t *testing.T) {
	guest := guestWithPrimaryNAD("ovn-val", "ovn-vm", "ovn-l2", "52:54:00:c4:0d:90")
	guest.Status.Network = &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "10.20.0.4"}
	c := nadAwareClientBuilder(nadObj("ovn-val", "ovn-l2", "kube-ovn", "ovn-l2.ovn-val.ovn")).Build()
	r := &SwiftGuestReconciler{Client: c}

	pod := &corev1.Pod{}
	if err := r.stampOVNIdentity(context.Background(), guest, pod); err != nil {
		t.Fatalf("stampOVNIdentity: %v", err)
	}
	if got := pod.Annotations[KubeOVNMACAnnotationKey("ovn-l2.ovn-val.ovn")]; got != "52:54:00:c4:0d:90" {
		t.Errorf("mac_address annotation = %q, want the guest MAC", got)
	}
	if got := pod.Annotations[KubeOVNIPAnnotationKey("ovn-l2.ovn-val.ovn")]; got != "10.20.0.4" {
		t.Errorf("ip_address annotation = %q, want 10.20.0.4", got)
	}
}

// First boot (no recorded IP): MAC is stamped, IP is not (kube-ovn allocates).
func TestStampOVNIdentity_NoIPUntilRecorded(t *testing.T) {
	guest := guestWithPrimaryNAD("ovn-val", "ovn-vm", "ovn-l2", "")
	c := nadAwareClientBuilder(nadObj("ovn-val", "ovn-l2", "kube-ovn", "")).Build()
	r := &SwiftGuestReconciler{Client: c}

	pod := &corev1.Pod{}
	if err := r.stampOVNIdentity(context.Background(), guest, pod); err != nil {
		t.Fatalf("stampOVNIdentity: %v", err)
	}
	// provider falls back to the kube-ovn convention <name>.<ns>.ovn when omitted.
	wantMAC := runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed("ovn-val", "ovn-vm", "app"))
	if got := pod.Annotations[KubeOVNMACAnnotationKey("ovn-l2.ovn-val.ovn")]; got != wantMAC {
		t.Errorf("mac_address = %q, want the deterministic generated MAC %q", got, wantMAC)
	}
	if _, ok := pod.Annotations[KubeOVNIPAnnotationKey("ovn-l2.ovn-val.ovn")]; ok {
		t.Errorf("no ip pin should be stamped before the IP is recorded")
	}
}

// A non-kube-ovn NAD (e.g. a bridge NAD) -> no OVN annotations.
func TestStampOVNIdentity_NonKubeOVN_NoOp(t *testing.T) {
	guest := guestWithPrimaryNAD("ns", "g", "bridge-nad", "")
	c := nadAwareClientBuilder(nadObj("ns", "bridge-nad", "bridge", "")).Build()
	r := &SwiftGuestReconciler{Client: c}

	pod := &corev1.Pod{}
	if err := r.stampOVNIdentity(context.Background(), guest, pod); err != nil {
		t.Fatalf("stampOVNIdentity: %v", err)
	}
	for k := range pod.Annotations {
		t.Errorf("non-OVN NAD should stamp nothing; got annotation %q", k)
	}
}

// A node-local (no networkRef) primary guest -> no NAD Get, no annotations.
func TestStampOVNIdentity_NodeLocal_NoOp(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "g"},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Interfaces: []swiftv1alpha1.GuestInterface{{Name: "mgmt"}},
		},
	}
	c := nadAwareClientBuilder().Build()
	r := &SwiftGuestReconciler{Client: c}

	pod := &corev1.Pod{}
	if err := r.stampOVNIdentity(context.Background(), guest, pod); err != nil {
		t.Fatalf("stampOVNIdentity: %v", err)
	}
	if len(pod.Annotations) != 0 {
		t.Errorf("node-local guest should stamp nothing; got %v", pod.Annotations)
	}
}

// --- live-migration dst identity (OVNMigrationDstAnnotations → kubeOVNBackend.Identity MigrationDstAnnotations) ---

// A kube-ovn primary-on-NAD guest's dst pod must preserve the MAC (LSP identity),
// pin the guest's CURRENT IP, and carry migrationJobName (so kube-ovn lets the dst
// share the src's still-held static IP across cutover).
func TestOVNMigrationDstAnnotations_KubeOVN_MacIPAndJobName(t *testing.T) {
	provider := "ovn-l2.ovn-val.ovn"
	guest := guestWithPrimaryNAD("ovn-val", "ovn-vm", "ovn-l2", "52:54:00:c4:0d:90")
	guest.Status.Network = &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: "10.20.0.4"}
	c := nadAwareClientBuilder(nadObj("ovn-val", "ovn-l2", "kube-ovn", provider)).Build()

	out, err := OVNMigrationDstAnnotations(context.Background(), c, guest, "ovn-vm-live")
	if err != nil {
		t.Fatalf("OVNMigrationDstAnnotations: %v", err)
	}
	if out[KubeOVNMACAnnotationKey(provider)] != "52:54:00:c4:0d:90" {
		t.Errorf("dst must preserve the kube-ovn mac_address (LSP identity); got %q", out[KubeOVNMACAnnotationKey(provider)])
	}
	if out[KubeOVNIPAnnotationKey(provider)] != "10.20.0.4" {
		t.Errorf("dst must pin the guest's current IP; got %q", out[KubeOVNIPAnnotationKey(provider)])
	}
	if out[MigrationJobNameAnnotation] != "ovn-vm-live" {
		t.Errorf("dst must carry migrationJobName so kube-ovn shares the src IP; got %q", out[MigrationJobNameAnnotation])
	}
}

// IP not yet recorded in status -> preserve MAC + set migrationJobName but skip
// the ip pin (kube-ovn allocates; the controller records it; a later pod pins it).
func TestOVNMigrationDstAnnotations_KubeOVN_NoIPWhenStatusEmpty(t *testing.T) {
	provider := "ovn-l2.ns.ovn"
	guest := guestWithPrimaryNAD("ns", "ovn-vm", "ovn-l2", "52:54:00:aa:bb:cc")
	c := nadAwareClientBuilder(nadObj("ns", "ovn-l2", "kube-ovn", provider)).Build()

	out, err := OVNMigrationDstAnnotations(context.Background(), c, guest, "m2")
	if err != nil {
		t.Fatalf("OVNMigrationDstAnnotations: %v", err)
	}
	if out[KubeOVNMACAnnotationKey(provider)] != "52:54:00:aa:bb:cc" {
		t.Errorf("MAC must be preserved even before the IP is known")
	}
	if _, ok := out[KubeOVNIPAnnotationKey(provider)]; ok {
		t.Errorf("no ip pin should be set when the guest IP is unknown")
	}
	if out[MigrationJobNameAnnotation] != "m2" {
		t.Errorf("migrationJobName should still be set")
	}
}

// A non-kube-ovn (or node-local) guest -> the dst annotation set is empty (the OVN
// path is inert for every other networking mode).
func TestOVNMigrationDstAnnotations_NonKubeOVN_Empty(t *testing.T) {
	guest := guestWithPrimaryNAD("ns", "g", "bridge-nad", "")
	c := nadAwareClientBuilder(nadObj("ns", "bridge-nad", "bridge", "")).Build()

	out, err := OVNMigrationDstAnnotations(context.Background(), c, guest, "m1")
	if err != nil {
		t.Fatalf("OVNMigrationDstAnnotations: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("non-OVN guest must yield no dst annotations; got %v", out)
	}
}
