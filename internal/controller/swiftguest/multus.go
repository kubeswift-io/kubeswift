package swiftguest

import (
	"encoding/json"
	"fmt"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// MultusAnnotationKey is the standard Multus annotation for requesting additional networks.
const MultusAnnotationKey = "k8s.v1.cni.cncf.io/networks"

// multusNetworkEntry represents a single entry in the Multus networks annotation JSON.
type multusNetworkEntry struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Interface string `json:"interface"`
}

// BuildMultusAnnotation builds the k8s.v1.cni.cncf.io/networks JSON annotation
// from the SwiftGuest's interfaces. Returns empty string if no secondary NICs.
func BuildMultusAnnotation(guest *swiftv1alpha1.SwiftGuest) string {
	if len(guest.Spec.Interfaces) == 0 {
		return ""
	}

	var entries []multusNetworkEntry
	multusIdx := 1 // net1, net2, etc.
	for _, iface := range guest.Spec.Interfaces {
		if iface.NetworkRef == nil {
			continue
		}
		ns := iface.NetworkRef.Namespace
		if ns == "" {
			ns = guest.Namespace
		}
		entries = append(entries, multusNetworkEntry{
			Name:      iface.NetworkRef.Name,
			Namespace: ns,
			Interface: fmt.Sprintf("net%d", multusIdx),
		})
		multusIdx++
	}

	if len(entries) == 0 {
		return ""
	}

	data, err := json.Marshal(entries)
	if err != nil {
		return ""
	}
	return string(data)
}
