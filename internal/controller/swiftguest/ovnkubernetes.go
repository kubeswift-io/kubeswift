package swiftguest

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// OVN-Kubernetes primary-on-NAD integration — the ovnKubernetesBackend
// implementation of the ovnBackend seam (ovn_backend.go).
//
// Like kube-ovn, OVN-Kubernetes binds each logical-switch port to a MAC and answers
// ARP for the port IP; unless the LSP identity is the guest, OVN delivers the
// guest's traffic to the wrong MAC behind the re-MAC'd pod NIC. The mechanism is
// DIFFERENT from kube-ovn's flat per-provider annotations: OVN-K's identity rides
// INSIDE the Multus network-selection element (the spike P2-step-1 finding,
// 2026-06-17):
//   - "mac": <guest MAC>             -> OVN-K creates the LSP with this MAC, so its
//     ARP responder + L2 delivery target the guest (the kube-ovn mac_address
//     equivalent — empirically a foreign MAC requested here is honored and reachable
//     cross-node).
//   - "ipam-claim-reference": <claim> -> pins the IP via a pre-created IPAMClaim
//     (OVN-K does NOT auto-create it — there is no kubevirt-ipam-claims webhook here,
//     so this backend creates+owns it per guest; spike P0-c / §8).
//
// Both fields land on the guest's PRIMARY NAD entry of k8s.v1.cni.cncf.io/networks
// (MultusAnnotationKey) — the SAME annotation the SwiftGuest pod builder sets and
// the live-migration dst pod already inherits (mergeAnnotationsForDst). And OVN-K
// cross-node claim OVERLAP is allowed by default (spike P0-c — no migrationJobName
// marker needed, unlike kube-ovn), so the dst keeps the identity + IP automatically:
// MigrationDstAnnotations is empty for this backend.
//
// v1 scope: topology layer2 (the IP-preserving, IPAMClaim-capable topology proven in
// the spike). localnet/provider OVN-K networks are documented-advanced (a later
// addition), so Detect narrows to layer2.

const (
	// OVNKubernetesCNIType is the NAD config "type" of an OVN-Kubernetes network.
	OVNKubernetesCNIType = "ovn-k8s-cni-overlay"
	// OVNKubernetesLayer2Topology is the v1-supported topology (IP-preserving,
	// IPAMClaim-capable). localnet is a later, advanced addition.
	OVNKubernetesLayer2Topology = "layer2"
)

// ipamClaimGVK identifies the NPWG IPAMClaim CRD for an unstructured create (the
// type is not registered in the controller scheme — like the NAD).
var ipamClaimGVK = schema.GroupVersionKind{
	Group:   "k8s.cni.cncf.io",
	Version: "v1alpha1",
	Kind:    "IPAMClaim",
}

// ovnKubernetesPrimaryNAD returns the OVN logical-network name + the primary
// interface's Multus name (net1, ...) when the guest's primary rides an
// OVN-Kubernetes layer2 NAD. ok=false (no error) means "not an OVN-K layer2
// primary-on-NAD guest" → the caller skips. A Get error is returned so the caller
// requeues (fail closed).
func ovnKubernetesPrimaryNAD(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (network, iface string, ok bool, err error) {
	prim := guest.PrimaryInterface()
	if prim == nil || prim.NetworkRef == nil {
		return "", "", false, nil
	}
	ns := prim.NetworkRef.Namespace
	if ns == "" {
		ns = guest.Namespace
	}
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	if e := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: prim.NetworkRef.Name}, nad); e != nil {
		return "", "", false, fmt.Errorf("get NAD %s/%s for ovn-kubernetes identity: %w", ns, prim.NetworkRef.Name, e)
	}
	cfgStr, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	if cfgStr == "" {
		return "", "", false, nil
	}
	var cfg struct {
		Type     string `json:"type"`
		Topology string `json:"topology"`
		Name     string `json:"name"`
	}
	if json.Unmarshal([]byte(cfgStr), &cfg) != nil {
		return "", "", false, nil
	}
	// v1: only ovn-k8s-cni-overlay + layer2 + a named OVN network (the IPAMClaim's
	// spec.network). Any other type/topology is not this backend's (kube-ovn,
	// bridge, localnet → skip).
	if cfg.Type != OVNKubernetesCNIType || cfg.Topology != OVNKubernetesLayer2Topology || cfg.Name == "" {
		return "", "", false, nil
	}
	// The primary's Multus interface name (net1, ...) — the IPAMClaim spec.interface
	// and the entry to augment. primaryIdx is the primary's position among the
	// NAD-bearing entries; it is >= 0 here because the primary has a NetworkRef.
	entries, primaryIdx := buildMultusEntries(guest)
	if primaryIdx < 0 || primaryIdx >= len(entries) {
		return "", "", false, nil
	}
	return cfg.Name, entries[primaryIdx].Interface, true, nil
}

// ovnKubernetesClaimName is the deterministic, per-guest-per-interface IPAMClaim
// name. Stable across pod recreate and the migration dst pod (which references the
// same name via the inherited Multus annotation).
func ovnKubernetesClaimName(guest *swiftv1alpha1.SwiftGuest, iface string) string {
	return fmt.Sprintf("%s-%s", guest.Name, iface)
}

// ovnKubernetesBackend is the ovnBackend for OVN-Kubernetes layer2 primary NADs.
// Stateless; the client is passed per call.
type ovnKubernetesBackend struct{}

func (ovnKubernetesBackend) Name() string { return "ovn-kubernetes" }

// Detect reports whether the guest's primary interface rides an OVN-Kubernetes
// layer2 NAD.
func (ovnKubernetesBackend) Detect(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (bool, error) {
	_, _, ok, err := ovnKubernetesPrimaryNAD(ctx, c, guest)
	return ok, err
}

// Identity injects the guest MAC (the LSP identity) + the ipam-claim-reference into
// the primary NAD's Multus selection element, and returns the per-guest IPAMClaim to
// ensure. MigrationDstAnnotations is empty: the augmented Multus annotation is carried
// onto the dst pod by mergeAnnotationsForDst and OVN-K allows the cross-node claim
// overlap by default (spike P0-c), so the dst keeps the identity + IP with no marker.
func (ovnKubernetesBackend) Identity(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, _ string) (ovnIdentity, error) {
	network, iface, ok, err := ovnKubernetesPrimaryNAD(ctx, c, guest)
	if err != nil {
		return ovnIdentity{}, err
	}
	if !ok {
		return ovnIdentity{}, nil
	}
	mac := primaryMAC(guest, guest.PrimaryInterface())
	if mac == "" {
		return ovnIdentity{}, nil
	}
	claimName := ovnKubernetesClaimName(guest, iface)

	// Rebuild the full networks annotation (matching the pod builder's, since both
	// use buildMultusEntries) with mac + ipam-claim-reference on the primary entry.
	entries, primaryIdx := buildMultusEntries(guest)
	if primaryIdx < 0 || primaryIdx >= len(entries) {
		return ovnIdentity{}, nil
	}
	entries[primaryIdx].MAC = mac
	entries[primaryIdx].IPAMClaimReference = claimName
	data, err := json.Marshal(entries)
	if err != nil {
		return ovnIdentity{}, fmt.Errorf("marshal ovn-kubernetes networks annotation: %w", err)
	}

	// The IPAMClaim this guest's pod (and its migration dst) reference. spec.network
	// is the OVN logical-network name (the NAD config "name"), spec.interface the
	// primary's Multus interface. Owner-ref + create are applied by the stamp
	// dispatch (ensureOVNClaim).
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(ipamClaimGVK)
	claim.SetNamespace(guest.Namespace)
	claim.SetName(claimName)
	_ = unstructured.SetNestedField(claim.Object, network, "spec", "network")
	_ = unstructured.SetNestedField(claim.Object, iface, "spec", "interface")

	return ovnIdentity{
		PodAnnotations:          map[string]string{MultusAnnotationKey: string(data)},
		MigrationDstAnnotations: nil,
		ClaimsToEnsure:          []*unstructured.Unstructured{claim},
	}, nil
}
