package swiftsnapshot

import (
	"fmt"
	"strings"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftsnapshotwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsnapshot"
)

// checkLocalHostPath re-enforces the local-backend hostPath rules in the
// CONTROLLER, not just the webhook.
//
// Why the duplication: `webhook.enabled` defaults to false (it needs
// cert-manager), so on a default install the webhook rules are advisory. The
// SwiftGuest host-path allowlist has been controller-enforced since #441 for
// exactly this reason; the snapshot one was left behind and is the same
// primitive — spec.backend.local.hostPath is mounted as a hostPath volume into
// the upload/cleanup Job (see s3.go), which runs privileged on the node.
//
// Deliberately mirrors validateLocalBackend rather than reimplementing it: the
// prefix constant is shared, so the two cannot drift on the value that matters.
func checkLocalHostPath(snap *snapshotv1alpha1.SwiftSnapshot) error {
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendLocal {
		return nil
	}
	if snap.Spec.Backend.Local == nil {
		return fmt.Errorf("spec.backend.local is required when spec.backend.type=local")
	}
	hp := snap.Spec.Backend.Local.HostPath
	if hp == "" {
		return fmt.Errorf("spec.backend.local.hostPath is required when spec.backend.type=local")
	}
	// Checked before the prefix test: a prefix test is a string comparison, so
	// "/var/lib/kubeswift/snapshots/../../etc" satisfies it.
	if strings.Contains(hp, "..") {
		return fmt.Errorf("spec.backend.local.hostPath must not contain '..' (got %q)", hp)
	}
	if !strings.HasPrefix(hp, swiftsnapshotwebhook.LocalBackendHostPathPrefix) {
		return fmt.Errorf("spec.backend.local.hostPath must be under %s (got %q)",
			swiftsnapshotwebhook.LocalBackendHostPathPrefix, hp)
	}
	return nil
}
