package swiftguest

import (
	"fmt"
	"regexp"
	"strings"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
	"github.com/kubeswift-io/kubeswift/internal/webhook/hostpath"
)

// restoreSnapshotDirSegment is the single directory name a restore snapshot
// path may carry after clonecommon.HostPathBase. The SwiftRestore and
// cloneFromSnapshot controllers only ever produce the snapshot's own
// node-local dir there — the local backend's hostPath (one safe segment, see
// swiftsnapshot.ValidateLocalHostPath) or S3LocalDir ("<ns>-<name>"). Both are
// a single [A-Za-z0-9._-] segment starting alphanumeric.
var restoreSnapshotDirSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateRestoreSnapshotPath rejects an active-restore snapshot path that is
// not clonecommon.HostPathBase followed by exactly one safe segment.
//
// The path comes from the snapshot.kubeswift.io/restore-snapshot-path
// ANNOTATION, not from spec, and BuildRestorePod mounts it into the privileged
// launcher as a hostPath. A tenant who can patch their own SwiftGuest could
// otherwise set active-restore + restore-snapshot-path=/ (or any node path) and
// mount it into a privileged pod — node root. checkHostPaths validates spec
// host paths but never saw this one, so it is enforced here at the same
// controller chokepoint.
func validateRestoreSnapshotPath(path string) error {
	if !strings.HasPrefix(path, clonecommon.HostPathBase) {
		return fmt.Errorf("restore snapshot path must be under %s (got %q)", clonecommon.HostPathBase, path)
	}
	seg := strings.TrimSuffix(strings.TrimPrefix(path, clonecommon.HostPathBase), "/")
	if seg == "" || !restoreSnapshotDirSegment.MatchString(seg) {
		return fmt.Errorf("restore snapshot path must be %s<name>, where <name> is a single path segment of [A-Za-z0-9._-] starting alphanumeric (got %q)", clonecommon.HostPathBase, path)
	}
	return nil
}

// checkHostPaths re-applies the host-path allowlist in the CONTROLLER, not only
// in the validating webhook.
//
// The webhook is the primary gate, but `webhook.enabled` defaults to false
// (it requires cert-manager), so on a default install nothing would enforce
// this at all. The same structural gap already bit the snapshot backend, whose
// hostPath prefix rule also lives only in its webhook — a comment there even
// says "the webhook ensures", which is not a control.
//
// Since the launcher is privileged, an unconfined host path is node-root. A
// guard that only runs in an optional component is not a guard, so it is
// enforced here too. Reconcile checks it before the root-disk clone: a guest
// with no launcher pod goes Failed with the violation on Resolved=False, and a
// launcher that already runs is left alone but not recreated. buildPod checks
// again as a backstop.
func checkHostPaths(guest *swiftv1alpha1.SwiftGuest, allowed []string) error {
	spec := &guest.Spec
	// Values written into Cloud Hypervisor's comma-separated device options:
	// a comma in one adds options of its author's choosing (a host disk).
	if err := hostpath.ValidateCHOptionValues(spec); err != nil {
		return err
	}
	for i := range spec.Filesystems {
		fs := &spec.Filesystems[i]
		if fs.Source.HostPath == nil {
			continue
		}
		if err := hostpath.Validate(
			fmt.Sprintf("spec.filesystems[%d].source.hostPath", i),
			*fs.Source.HostPath, allowed); err != nil {
			return err
		}
	}
	// vhost-user sockets: the pod builder mounts filepath.Dir(socket).
	for i := range spec.Interfaces {
		if sock := spec.Interfaces[i].Socket; sock != "" {
			if err := hostpath.ValidateDir(
				fmt.Sprintf("spec.interfaces[%d].socket", i), sock, allowed); err != nil {
				return err
			}
		}
	}
	for i := range spec.VhostUserDevices {
		if sock := spec.VhostUserDevices[i].Socket; sock != "" {
			if err := hostpath.ValidateDir(
				fmt.Sprintf("spec.vhostUserDevices[%d].socket", i), sock, allowed); err != nil {
				return err
			}
		}
	}
	// Restore-mode host path: comes from annotations (not spec) and is mounted
	// into the privileged restore launcher, so it is not covered by the
	// operator allowlist above and is constrained to the snapshot dir instead.
	if params, ok := RestoreParamsFromAnnotations(guest.Annotations); ok {
		if err := validateRestoreSnapshotPath(params.SnapshotPath); err != nil {
			return err
		}
	}
	return nil
}
