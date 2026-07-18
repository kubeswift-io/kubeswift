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
// ovnKubernetesNADNetwork resolves a networkRef to the OVN logical-network name (the
// NAD config "name", used as the IPAMClaim spec.network) when it names an
// OVN-Kubernetes layer2 NAD. ok=false (no error) means "not an OVN-K layer2 NAD"
// (kube-ovn / bridge / localnet → the caller skips). A Get error is returned so the
// caller requeues (fail closed).
func ovnKubernetesNADNetwork(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, ref *swiftv1alpha1.NetworkReference) (network string, ok bool, err error) {
	ns := ref.Namespace
	if ns == "" {
		ns = guest.Namespace
	}
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	if e := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, nad); e != nil {
		return "", false, fmt.Errorf("get NAD %s/%s for ovn-kubernetes identity: %w", ns, ref.Name, e)
	}
	cfgStr, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	if cfgStr == "" {
		return "", false, nil
	}
	var cfg struct {
		Type     string `json:"type"`
		Topology string `json:"topology"`
		Name     string `json:"name"`
	}
	if json.Unmarshal([]byte(cfgStr), &cfg) != nil {
		return "", false, nil
	}
	// v1: only ovn-k8s-cni-overlay + layer2 + a named OVN network. Any other
	// type/topology is not this backend's (kube-ovn, bridge, localnet → skip).
	if cfg.Type != OVNKubernetesCNIType || cfg.Topology != OVNKubernetesLayer2Topology || cfg.Name == "" {
		return "", false, nil
	}
	return cfg.Name, true, nil
}

// ovnKubernetesPrimaryNAD returns the OVN logical-network name + the primary
// interface's Multus name when the guest's PRIMARY rides an OVN-Kubernetes layer2
// NAD. ok=false means the primary is not an OVN-K layer2 NAD (a secondary-only guest
// still takes the general Identity path). Retained for the migration-dst code path.
func ovnKubernetesPrimaryNAD(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (network, iface string, ok bool, err error) {
	prim := guest.PrimaryInterface()
	if prim == nil || prim.NetworkRef == nil {
		return "", "", false, nil
	}
	network, ok, err = ovnKubernetesNADNetwork(ctx, c, guest, prim.NetworkRef)
	if err != nil || !ok {
		return "", "", ok, err
	}
	entries, primaryIdx := buildMultusEntries(guest)
	if primaryIdx < 0 || primaryIdx >= len(entries) {
		return "", "", false, nil
	}
	return network, entries[primaryIdx].Interface, true, nil
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

// Detect reports whether ANY of the guest's interfaces (primary or secondary) rides
// an OVN-Kubernetes layer2 NAD.
func (ovnKubernetesBackend) Detect(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest) (bool, error) {
	for i := range guest.Spec.Interfaces {
		ref := guest.Spec.Interfaces[i].NetworkRef
		if ref == nil {
			continue
		}
		_, ok, err := ovnKubernetesNADNetwork(ctx, c, guest, ref)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// Identity injects the guest MAC (the LSP identity) + an ipam-claim-reference into
// EVERY OVN-Kubernetes NAD interface's Multus selection element (primary and/or
// secondary), and returns the per-interface IPAMClaims to ensure. A secondary NAD
// interface (e.g. a routable node-datapath NIC) needs the same treatment as the
// primary, or OVN's per-port ARP responder answers with the pod NIC's MAC and the
// bridged guest is unreachable on the segment. MigrationDstAnnotations is empty: the
// augmented Multus annotation is carried onto the dst pod by mergeAnnotationsForDst
// and OVN-K allows the cross-node claim overlap by default (spike P0-c).
func (ovnKubernetesBackend) Identity(ctx context.Context, c client.Client, guest *swiftv1alpha1.SwiftGuest, _ string) (ovnIdentity, error) {
	// The full networks annotation (matching the pod builder's, since both use
	// buildMultusEntries). Entries are in guest.Spec.Interfaces order among the
	// NAD-bearing interfaces, so the idx counter below indexes straight into it.
	entries, _ := buildMultusEntries(guest)
	if len(entries) == 0 {
		return ovnIdentity{}, nil
	}

	var claims []*unstructured.Unstructured
	idx := -1
	for i := range guest.Spec.Interfaces {
		iface := &guest.Spec.Interfaces[i]
		if iface.NetworkRef == nil {
			continue
		}
		idx++
		if idx >= len(entries) {
			break
		}
		network, ok, err := ovnKubernetesNADNetwork(ctx, c, guest, iface.NetworkRef)
		if err != nil {
			return ovnIdentity{}, err
		}
		if !ok {
			continue // kube-ovn / bridge / localnet — not this backend's
		}
		mac := primaryMAC(guest, iface)
		if mac == "" {
			continue
		}
		claimName := ovnKubernetesClaimName(guest, entries[idx].Interface)
		entries[idx].MAC = mac
		entries[idx].IPAMClaimReference = claimName

		// spec.network is the OVN logical-network name (the NAD config "name"),
		// spec.interface this interface's Multus name. Owner-ref + create are applied
		// by the stamp dispatch (ensureOVNClaim).
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(ipamClaimGVK)
		claim.SetNamespace(guest.Namespace)
		claim.SetName(claimName)
		_ = unstructured.SetNestedField(claim.Object, network, "spec", "network")
		_ = unstructured.SetNestedField(claim.Object, entries[idx].Interface, "spec", "interface")
		claims = append(claims, claim)
	}
	if len(claims) == 0 {
		return ovnIdentity{}, nil
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return ovnIdentity{}, fmt.Errorf("marshal ovn-kubernetes networks annotation: %w", err)
	}
	return ovnIdentity{
		PodAnnotations:          map[string]string{MultusAnnotationKey: string(data)},
		MigrationDstAnnotations: nil,
		ClaimsToEnsure:          claims,
	}, nil
}
