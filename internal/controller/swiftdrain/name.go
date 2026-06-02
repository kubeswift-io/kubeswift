package swiftdrain

import (
	"crypto/sha256"
	"encoding/hex"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// maxMigrationNameLen bounds the generated SwiftMigration name. The name
// becomes a pod LABEL VALUE (dst_pod.go LabelMigrationName), so it must be a
// valid label value (<=63 chars, RFC1123). Mirrors swiftkernel/pull.go's
// truncate-40 + sha256-8 naming discipline.
const maxMigrationNameLen = 63

// drainMigrationName returns the deterministic SwiftMigration name for
// draining guestName off nodeName. Deterministic per (guest, node) so the
// eviction API's 5s retries reuse the same migration object; a later drain
// of a DIFFERENT node yields a different name (the node is folded into the
// hash). Bounded to a valid label value.
func drainMigrationName(guestName, nodeName string) string {
	suffix := "-drain-" + shortHash(guestName+"|"+nodeName)
	if len(guestName)+len(suffix) <= maxMigrationNameLen {
		return guestName + suffix
	}
	// Guest name too long: truncate it so the whole name fits.
	keep := maxMigrationNameLen - len(suffix)
	if keep < 0 {
		keep = 0
	}
	return guestName[:keep] + suffix
}

// shortHash returns the first 8 hex digits of sha256(s).
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// drainMode maps a guest's drainPolicy to the SwiftMigration mode. Only
// migratable policies reach the drain controller (the eviction webhook denies
// Block / migration-disabled / VFIO without marking), so this handles the two
// migratable cases:
//   - Migrate (default): mode=auto — live where possible, offline otherwise.
//   - LiveMigrate: mode=live — live only (the operator opted out of downtime).
func drainMode(policy string) migrationv1alpha1.SwiftMigrationMode {
	if policy == swiftv1alpha1.DrainPolicyLiveMigrate {
		return migrationv1alpha1.SwiftMigrationModeLive
	}
	return migrationv1alpha1.SwiftMigrationModeAuto
}

// drainPolicyOf returns the guest's effective drain policy (default Migrate).
func drainPolicyOf(guest *swiftv1alpha1.SwiftGuest) string {
	if guest.Spec.Migration != nil && guest.Spec.Migration.DrainPolicy != "" {
		return guest.Spec.Migration.DrainPolicy
	}
	return swiftv1alpha1.DrainPolicyMigrate
}
