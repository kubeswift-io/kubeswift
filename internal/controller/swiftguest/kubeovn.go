package swiftguest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/runtimeintent"
)

// kube-ovn primary-on-NAD integration — the kubeOVNBackend implementation of the
// ovnBackend seam (ovn_backend.go).
//
// When a SwiftGuest's PRIMARY interface rides a kube-ovn-managed NAD, the guest's
// portable IP lives on a real OVN logical switch. OVN binds each logical-switch
// port (LSP) to the POD interface's MAC and answers ARP for the port IP with that
// MAC. KubeSwift's datapath, however, bridges the guest's OWN (distinct)
// hypervisor MAC behind the pod NIC (network-init's setup_primary_nad_nic), so
// without telling kube-ovn the guest's MAC, OVN delivers the guest's traffic to
// the wrong MAC and the guest is unreachable on the segment. This was diagnosed
// and fixed-by-hand on the multi-node-L2 OVN validation spike; this makes it
// automatic — the KubeVirt model: program the LSP identity to be the guest.
//
// Mechanism (all pod annotations; no CNI/datapath change):
//   - "<provider>.kubernetes.io/mac_address" = the guest's primary MAC  -> the LSP
//     identity is the guest, so OVN's ARP responder + L2 delivery target the guest.
//   - "<provider>.kubernetes.io/ip_address"  = the guest's current IP   -> a stable
//     static IP across pod recreate (and, with the migration path below, across a
//     live migration).
//   - (migration dst only) "kubevirt.io/migrationJobName" -> kube-ovn skips the IPAM
//     conflict check so the dst pod can acquire the SAME static IP the src still
//     holds during the cutover overlap.
//
// "<provider>" is the kube-ovn provider of the NAD (its config "provider", e.g.
// "ovn-l2.<ns>.ovn") — the PER-PROVIDER annotation form a Multus secondary uses;
// the bare "ovn.kubernetes.io/..." form is the primary network only.

const (
	// KubeOVNCNIType is the NAD config "type" of a kube-ovn-managed network.
	KubeOVNCNIType = "kube-ovn"

	// KubeOVNMACAnnotationSuffix / KubeOVNIPAnnotationSuffix are kube-ovn's
	// per-provider pod-annotation suffixes; the full key is
	// "<provider>.kubernetes.io/<suffix>".
	KubeOVNMACAnnotationSuffix = ".kubernetes.io/mac_address"
	KubeOVNIPAnnotationSuffix  = ".kubernetes.io/ip_address"

	// MigrationJobNameAnnotation is the (KubeVirt-originated) annotation kube-ovn
	// reads to recognise a live-migration TARGET pod: present -> its IPAM skips the
	// conflict check, letting the dst pod share the src's still-held static IP
	// across the cutover overlap. kube-ovn (pkg/controller/pod.go) sets its
	// AllowLiveMigration flag purely from this annotation's presence — no KubeVirt
	// object is consulted on that path — so a non-KubeVirt controller can use it by
	// setting the annotation alone. The migration controller sets it on the dst pod
	// with the SwiftMigration name as the (synthetic) value.
	MigrationJobNameAnnotation = "kubevirt.io/migrationJobName"
)

// networkAttachmentDefinitionGVK identifies the Multus NAD CRD for an
// unstructured Get (the type is not registered in the controller scheme).
var networkAttachmentDefinitionGVK = schema.GroupVersionKind{
	Group:   "k8s.cni.cncf.io",
	Version: "v1",
	Kind:    "NetworkAttachmentDefinition",
}

// KubeOVNMACAnnotationKey / KubeOVNIPAnnotationKey build a kube-ovn provider's
// per-provider mac/ip annotation key.
func KubeOVNMACAnnotationKey(provider string) string { return provider + KubeOVNMACAnnotationSuffix }
func KubeOVNIPAnnotationKey(provider string) string  { return provider + KubeOVNIPAnnotationSuffix }

// primaryMAC returns the MAC the resolver assigns to the guest's primary
// interface: the pinned spec MAC if set, else the deterministic generated MAC
// (the SAME seed the resolver uses in internal/resolved, so this matches the
// running guest and is stable across pod recreate and the migration dst pod).
func primaryMAC(guest *swiftv1alpha1.SwiftGuest, iface *swiftv1alpha1.GuestInterface) string {
	if iface.MAC != "" {
		return iface.MAC
	}
	return runtimeintent.GenerateMAC(runtimeintent.InterfaceMACSeed(guest.Namespace, guest.Name, iface.Name))
}

// kubeOVNPrimaryProvider returns the kube-ovn provider + the guest's primary MAC
// when the guest's primary interface rides a kube-ovn-class NAD. It Gets the NAD
// (unstructured) and inspects its config. ok=false (no error) means "not a
// kube-ovn primary-on-NAD guest" and the caller skips stamping. A Get error is
// returned so the caller requeues.
func kubeOVNPrimaryProvider(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (provider, mac string, ok bool, err error) {
	iface := guest.PrimaryInterface()
	if iface == nil || iface.NetworkRef == nil {
		return "", "", false, nil
	}
	ns := iface.NetworkRef.Namespace
	if ns == "" {
		ns = guest.Namespace
	}
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	if e := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: iface.NetworkRef.Name}, nad); e != nil {
		return "", "", false, fmt.Errorf("get NAD %s/%s for kube-ovn identity: %w", ns, iface.NetworkRef.Name, e)
	}
	cfgStr, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	if cfgStr == "" {
		return "", "", false, nil
	}
	var cfg struct {
		Type     string `json:"type"`
		Provider string `json:"provider"`
	}
	if json.Unmarshal([]byte(cfgStr), &cfg) != nil || cfg.Type != KubeOVNCNIType {
		return "", "", false, nil
	}
	provider = cfg.Provider
	if provider == "" {
		// kube-ovn convention when the NAD omits an explicit provider.
		provider = fmt.Sprintf("%s.%s.ovn", iface.NetworkRef.Name, ns)
	}
	return provider, primaryMAC(guest, iface), true, nil
}

// kubeOVNBackend is the ovnBackend for kube-ovn-managed primary NADs. Stateless;
// the client is passed per call.
type kubeOVNBackend struct{}

func (kubeOVNBackend) Name() string { return KubeOVNCNIType }

// Detect reports whether the guest's primary interface rides a kube-ovn-class NAD.
func (kubeOVNBackend) Detect(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (bool, error) {
	_, _, ok, err := kubeOVNPrimaryProvider(ctx, c, guest)
	return ok, err
}

// Identity computes the kube-ovn LSP-identity annotations for the launcher pod and
// the live-migration dst pod. The pinned IP (status's kube-ovn-assigned IP) is set
// once known; on first boot it is empty and kube-ovn allocates dynamically.
func (kubeOVNBackend) Identity(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, migName string) (ovnIdentity, error) {
	provider, mac, ok, err := kubeOVNPrimaryProvider(ctx, c, guest)
	if err != nil {
		return ovnIdentity{}, err
	}
	if !ok || mac == "" {
		return ovnIdentity{}, nil
	}
	macKey := KubeOVNMACAnnotationKey(provider)

	// Pin the IP once it is known (status carries the kube-ovn-assigned IP from a
	// prior boot). net.ParseIP guards a malformed status value.
	ip := ""
	if guest.Status.Network != nil {
		if pip := guest.Status.Network.PrimaryIP; pip != "" && net.ParseIP(pip) != nil {
			ip = pip
		}
	}

	pod := map[string]string{macKey: mac}
	if ip != "" {
		pod[KubeOVNIPAnnotationKey(provider)] = ip
	}

	// The dst set keeps the same LSP identity (MAC) and IP pin, plus the
	// migrationJobName marker so kube-ovn's IPAM skips the conflict check during
	// the cutover overlap. migName is "" for the boot-time pod-identity call, whose
	// caller uses only PodAnnotations.
	dst := map[string]string{macKey: mac}
	if ip != "" {
		dst[KubeOVNIPAnnotationKey(provider)] = ip
	}
	if migName != "" {
		dst[MigrationJobNameAnnotation] = migName
	}

	return ovnIdentity{PodAnnotations: pod, MigrationDstAnnotations: dst}, nil
}
