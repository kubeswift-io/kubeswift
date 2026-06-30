package swiftguest

import (
	"encoding/json"
	"fmt"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// MultusAnnotationKey is the standard Multus annotation for requesting additional networks.
const MultusAnnotationKey = "k8s.v1.cni.cncf.io/networks"

// multusNetworkEntry represents a single entry in the Multus networks annotation JSON.
//
// MAC and IPAMClaimReference are the NPWG network-selection-element fields an OVN
// backend injects on the guest's primary entry (see ovnkubernetes.go): OVN-K binds
// the logical-switch-port to the requested MAC (so it delivers to the guest behind
// the re-MAC'd pod NIC) and pins the IP via the referenced IPAMClaim. They are
// omitempty, so a plain (kube-ovn / bridge / SR-IOV) entry serializes exactly as
// before — no behavior change for non-OVN-Kubernetes networks.
type multusNetworkEntry struct {
	Name               string `json:"name"`
	Namespace          string `json:"namespace,omitempty"`
	Interface          string `json:"interface"`
	MAC                string `json:"mac,omitempty"`
	IPAMClaimReference string `json:"ipam-claim-reference,omitempty"`
}

// buildMultusEntries returns the per-interface Multus selection elements for the
// guest's NAD-bearing interfaces (in spec order, numbered net1, net2, ...) and the
// index of the guest's PRIMARY interface within that slice (-1 if the primary is
// not NAD-bearing — e.g. node-local primary). Exposed within the package so an OVN
// backend can augment the primary entry (mac + ipam-claim-reference) before the
// annotation is marshaled.
func buildMultusEntries(guest *swiftv1alpha1.SwiftGuest) ([]multusNetworkEntry, int) {
	var entries []multusNetworkEntry
	primaryIdx := -1
	multusIdx := 1 // net1, net2, etc.
	for _, iface := range guest.Spec.Interfaces {
		if iface.NetworkRef == nil {
			continue
		}
		ns := iface.NetworkRef.Namespace
		if ns == "" {
			ns = guest.Namespace
		}
		if iface.Primary {
			primaryIdx = len(entries)
		}
		entries = append(entries, multusNetworkEntry{
			Name:      iface.NetworkRef.Name,
			Namespace: ns,
			Interface: fmt.Sprintf("net%d", multusIdx),
		})
		multusIdx++
	}
	return entries, primaryIdx
}

// BuildMultusAnnotation builds the k8s.v1.cni.cncf.io/networks JSON annotation
// from the SwiftGuest's interfaces. Returns empty string if no secondary NICs.
func BuildMultusAnnotation(guest *swiftv1alpha1.SwiftGuest) string {
	entries, _ := buildMultusEntries(guest)
	if len(entries) == 0 {
		return ""
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(data)
}
