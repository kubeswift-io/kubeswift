package swiftsnapshot

import (
	"fmt"

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
// Calls the SAME validator the webhook uses (ValidateLocalHostPath) rather than
// reimplementing it, so the controller-enforced rule and the advisory webhook
// rule cannot drift — the path is mounted into a privileged Job and handed to
// rm/remove_dir_all, so both must reject the shared root, shell metacharacters,
// nested paths and '..' identically.
func checkLocalHostPath(snap *snapshotv1alpha1.SwiftSnapshot) error {
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendLocal {
		return nil
	}
	if snap.Spec.Backend.Local == nil {
		return fmt.Errorf("spec.backend.local is required when spec.backend.type=local")
	}
	return swiftsnapshotwebhook.ValidateLocalHostPath(snap.Spec.Backend.Local.HostPath)
}
