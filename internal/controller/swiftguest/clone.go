package swiftguest

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/snapshot/clonecommon"
)

// prepareCloneFromSnapshot prepares a SwiftGuest that boots via
// spec.cloneFromSnapshot (Snapshot Phase 4). It resolves the referenced
// SwiftSnapshot and the LIVE source guest, self-stamps the restore-receive
// annotations (so the existing annotation-driven restore path in
// buildBasePod/RestoreParamsFromAnnotations + the runtime intent fire
// unchanged), and returns an "effective" guest carrying the SOURCE guest's spec
// for the resolver — the clone guest itself has no imageRef. Only the spec is
// overlaid (for resolution); the real guest keeps its identity
// (name/namespace/annotations) and is used everywhere else.
//
// Returns (effective, failReason, requeue, err):
//   - effective != nil  → resolve THIS instead of the real guest.
//   - failReason != ""  → set Resolved=False / phase=Failed (terminal).
//   - requeue           → snapshot not Ready yet; re-reconcile.
//
// PR 3a handles Tier B (local, same-node) snapshots. Tier C (s3) needs the
// download path (PR 3b); the source guest must be live (the snapshot's
// CapturedGuestSpec is validation-only — cross-cluster/source-gone clones are a
// future enhancement).
func (r *SwiftGuestReconciler) prepareCloneFromSnapshot(
	ctx context.Context, guest *swiftv1alpha1.SwiftGuest,
) (*swiftv1alpha1.SwiftGuest, string, bool, error) {
	src := guest.Spec.CloneFromSnapshot

	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: src.SnapshotRef.Name, Namespace: guest.Namespace}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "SwiftSnapshot " + src.SnapshotRef.Name + " not found", false, nil
		}
		return nil, "", false, err
	}
	if snap.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady {
		// Transient — the snapshot may still be Capturing/Uploading.
		return nil, "", true, nil
	}

	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendLocal:
		if snap.Status.NodeName == "" || snap.Spec.Backend.Local == nil || snap.Spec.Backend.Local.HostPath == "" {
			return nil, "SwiftSnapshot " + snap.Name + " is missing status.nodeName or backend.local.hostPath", false, nil
		}
	case snapshotv1alpha1.SnapshotBackendS3:
		return nil, "cloneFromSnapshot from an s3 (Tier C) snapshot is not yet implemented (Snapshot Phase 4 PR 3b)", false, nil
	default:
		return nil, "cloneFromSnapshot requires a memory snapshot (backend.type: local); got " + string(snap.Spec.Backend.Type), false, nil
	}

	// The clone needs the source guest's full spec (image/seed/class) to build
	// the launcher pod — a fresh disk from the source image plus the restored
	// memory. The snapshot's CapturedGuestSpec is insufficient (CPU/mem/image
	// name only), so the source guest must still exist.
	var source swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: snap.Spec.GuestRef.Name, Namespace: guest.Namespace}, &source); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "source SwiftGuest " + snap.Spec.GuestRef.Name + " no longer exists; cloneFromSnapshot needs the source spec", false, nil
		}
		return nil, "", false, err
	}

	// Self-stamp the clone-mode restore annotations (mirrors what the
	// SwiftRestore controller stamps on its clone targets) if not already set.
	annos := cloneRestoreAnnotations(guest, &snap, &source)
	if !cloneAnnotationsMatch(guest.Annotations, annos) {
		patched := guest.DeepCopy()
		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}
		for k, v := range annos {
			patched.Annotations[k] = v
		}
		if err := r.Patch(ctx, patched, client.MergeFrom(guest)); err != nil {
			return nil, "", false, err
		}
		// Reflect the stamp in-memory so the rest of this reconcile sees it.
		guest.Annotations = patched.Annotations
	}

	// Effective guest: the real guest's identity (name/namespace/annotations/
	// status) with the SOURCE guest's spec, for the resolver only. runPolicy is
	// clone-owned (it governs the clone's lifecycle via rg.Lifecycle), so keep
	// the clone's rather than inheriting the source's.
	effective := guest.DeepCopy()
	clonePolicy := guest.Spec.RunPolicy
	effective.Spec = source.Spec
	effective.Spec.RunPolicy = clonePolicy
	return effective, "", false, nil
}

// cloneRestoreAnnotations builds the clone-mode restore-receive annotations for
// a cloneFromSnapshot guest (Tier B local). MAC rewrites + runtime-dir prefixes
// use the clonecommon primitives shared with the SwiftRestore clone path.
func cloneRestoreAnnotations(
	guest *swiftv1alpha1.SwiftGuest,
	snap *snapshotv1alpha1.SwiftSnapshot,
	source *swiftv1alpha1.SwiftGuest,
) map[string]string {
	annos := map[string]string{
		AnnotationActiveRestore:               snap.Name,
		AnnotationRestoreSnapshotPath:         snap.Spec.Backend.Local.HostPath,
		AnnotationRestoreNodeName:             snap.Status.NodeName,
		AnnotationRestoreMode:                 RestoreModeClone,
		AnnotationRestoreMACRewrites:          clonecommon.ComputeMACRewrites(guest.Namespace, guest.Name, source),
		AnnotationRestoreRuntimeDirFromPrefix: clonecommon.RuntimeDirPrefix(source.Namespace, source.Name),
		AnnotationRestoreRuntimeDirToPrefix:   clonecommon.RuntimeDirPrefix(guest.Namespace, guest.Name),
		AnnotationRestoreNullifyHostMAC:       "true",
	}
	if cloneRegenIncludesNonMAC(guest.Spec.CloneFromSnapshot) {
		annos[AnnotationRestoreAppendCmdlineMarker] = "true"
	}
	return annos
}

// cloneRegenIncludesNonMAC reports whether the clone requests regeneration of a
// non-MAC identity item (hostname/machineId/sshHostKeys) — these fire on the
// clone's first reboot via the seed bootcmd. An empty Regenerate defaults to
// all four. macAddresses is handled separately (always forced via MAC rewrites).
func cloneRegenIncludesNonMAC(src *swiftv1alpha1.CloneFromSnapshotSource) bool {
	if src == nil || len(src.Regenerate) == 0 {
		return true
	}
	for _, item := range src.Regenerate {
		switch item {
		case swiftv1alpha1.CloneRegenHostname, swiftv1alpha1.CloneRegenMachineID, swiftv1alpha1.CloneRegenSSHHostKeys:
			return true
		}
	}
	return false
}

// cloneAnnotationsMatch reports whether every want key is already present in
// have with the same value (so the stamp is idempotent — no re-patch).
func cloneAnnotationsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
