package swiftguestpool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// computeTemplateHash produces a stable hash of the template spec.
// Only the spec is hashed -- metadata changes (labels, annotations)
// do not trigger a rolling update.
func computeTemplateHash(template *swiftv1alpha1.SwiftGuestTemplateSpec) string {
	data, err := json.Marshal(template.Spec)
	if err != nil {
		return "unknown"
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:8])
}
