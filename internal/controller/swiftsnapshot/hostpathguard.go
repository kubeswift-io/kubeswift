package swiftsnapshot

import (
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftsnapshotwebhook "github.com/kubeswift-io/kubeswift/internal/webhook/swiftsnapshot"
)

// checkLocalHostPath re-enforces the local-backend hostPath rule in the
// CONTROLLER, not just the webhook.
//
// Why the duplication: `webhook.enabled` defaults to false (it needs
// cert-manager), so on a default install the webhook rules are advisory. The
// SwiftGuest host-path allowlist has been controller-enforced since #441 for
// exactly this reason; the snapshot one was left behind and is the same
// primitive — the capture directory is mounted as a hostPath volume into the
// upload/cleanup Job (see s3.go), which runs privileged on the node.
//
// Calls the SAME validator the webhook uses (ValidateLocalHostPath) rather than
// reimplementing it, so the controller-enforced rule and the advisory webhook
// rule cannot drift.
//
// Only before the capture begins (no status.nodeName). The spec's hostPath is
// not read after that: a capture's directory is the one recorded when it began
// (clonecommon.NodeDir). A snapshot an earlier version captured into a
// directory its author chose therefore keeps working — its restores and its
// cleanup use that recorded directory, which the shape check in pathSubdir
// still guards.
func checkLocalHostPath(snap *snapshotv1alpha1.SwiftSnapshot) error {
	if snap.Spec.Backend.Type != snapshotv1alpha1.SnapshotBackendLocal ||
		snap.Spec.Backend.Local == nil || snap.Status.NodeName != "" {
		return nil
	}
	return swiftsnapshotwebhook.ValidateLocalHostPath(snap.Namespace, snap.Name, snap.Spec.Backend.Local.HostPath)
}
