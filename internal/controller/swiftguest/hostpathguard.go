package swiftguest

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
	"github.com/kubeswift-io/kubeswift/internal/webhook/hostpath"
)

// restoreSnapshotDirSegment is the single directory name a restore snapshot
// path may carry after clonecommon.HostPathBase. The SwiftRestore and
// cloneFromSnapshot controllers only ever produce the snapshot's own
// node-local dir there (clonecommon.NodeDir): "<ns>_<name>" for a snapshot
// captured by this version, or what an earlier one used — the local
// backend's hostPath (one safe segment) or "<ns>-<name>". All are a single
// [A-Za-z0-9._-] segment starting alphanumeric.
var restoreSnapshotDirSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// restoreSnapshotOwnerViolation binds an active-restore snapshot path to the
// guest's own namespace. validateRestoreSnapshotPath only checks its shape,
// and every snapshot directory on a node has that shape, so a tenant who can
// annotate their own SwiftGuest could name another namespace's snapshot and
// boot its memory image — secrets and all — in their own VM.
//
// A directory this version derives ("<ns>_<name>", clonecommon.SnapshotDir)
// names its namespace. An older one does not ("<ns>-<name>" is ambiguous, and
// a local hostPath was any name), so it is accepted only if a SwiftSnapshot in
// the guest's namespace was captured into it. Only the restore launcher mounts
// the path. A violation is handled like a disallowed host path: a guest with
// no launcher fails, and a running launcher is left alone but not recreated.
// err is a failed lookup, to retry.
func (r *SwiftGuestReconciler) restoreSnapshotOwnerViolation(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (violation, err error) {
	params, ok := RestoreParamsFromAnnotations(guest.Annotations)
	if !ok {
		return nil, nil
	}
	dir := strings.TrimSuffix(params.SnapshotPath, "/")
	if clonecommon.SnapshotDirNamespaced(dir, guest.Namespace) {
		return nil, nil
	}
	var snaps snapshotv1alpha1.SwiftSnapshotList
	if err := r.List(ctx, &snaps, client.InNamespace(guest.Namespace)); err != nil {
		return nil, fmt.Errorf("list SwiftSnapshots to check the restore snapshot path: %w", err)
	}
	for i := range snaps.Items {
		if snaps.Items[i].Status.NodeName != "" && clonecommon.NodeDir(&snaps.Items[i]) == dir {
			return nil, nil
		}
	}
	return fmt.Errorf("restore snapshot path %q is not the directory of a SwiftSnapshot in namespace %s", params.SnapshotPath, guest.Namespace), nil
}

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
