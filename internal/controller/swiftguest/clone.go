package swiftguest

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
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

	// Resolve the on-node snapshot directory + the node the clone runs on.
	// Tier B (local): the snapshot already lives on its capture node. Tier C
	// (s3): a per-guest download Job pulls the artifacts into the chosen target
	// node's cache first (mirrors the SwiftRestore download path); the clone
	// then boots once the cache is populated.
	var snapshotPath, node string
	switch snap.Spec.Backend.Type {
	case snapshotv1alpha1.SnapshotBackendLocal:
		if snap.Status.NodeName == "" || snap.Spec.Backend.Local == nil || snap.Spec.Backend.Local.HostPath == "" {
			return nil, "SwiftSnapshot " + snap.Name + " is missing status.nodeName or backend.local.hostPath", false, nil
		}
		snapshotPath, node = snap.Spec.Backend.Local.HostPath, snap.Status.NodeName
	case snapshotv1alpha1.SnapshotBackendS3:
		node = src.TargetNode
		if node == "" {
			return nil, "cloneFromSnapshot from a Tier C (s3) snapshot requires spec.cloneFromSnapshot.targetNode", false, nil
		}
		if snap.Status.S3 == nil || snap.Status.S3.Location == "" {
			return nil, "SwiftSnapshot " + snap.Name + " has no status.s3 — its upload is not complete", false, nil
		}
		done, failReason, derr := r.ensureCloneDownloadJob(ctx, guest, &snap, node)
		if derr != nil {
			return nil, "", false, derr
		}
		if failReason != "" {
			return nil, failReason, false, nil
		}
		if !done {
			// Still downloading the snapshot artifacts onto the target node.
			return nil, "", true, nil
		}
		snapshotPath = clonecommon.S3LocalDir(&snap)
	default:
		return nil, "cloneFromSnapshot requires a memory snapshot (backend.type: local or s3); got " + string(snap.Spec.Backend.Type), false, nil
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
	annos := cloneRestoreAnnotations(guest, &snap, &source, snapshotPath, node)
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
	snapshotPath, node string,
) map[string]string {
	annos := map[string]string{
		AnnotationActiveRestore:               snap.Name,
		AnnotationRestoreSnapshotPath:         snapshotPath,
		AnnotationRestoreNodeName:             node,
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

// cloneDownloadJobName is the deterministic name of a cloneFromSnapshot guest's
// Tier C download Job.
func cloneDownloadJobName(guest *swiftv1alpha1.SwiftGuest) string {
	return guest.Name + "-clone-download"
}

// ensureCloneDownloadJob creates (idempotently) a guest-owned, node-pinned
// download Job that pulls a Tier C snapshot's artifacts into the target node's
// cache, and reports whether it has completed. Returns (done, failReason, err):
//   - done=true       → the cache is populated; proceed to boot the clone.
//   - failReason != "" → terminal (image unset, or the Job failed).
//   - otherwise        → still downloading (caller requeues).
//
// The cache dir (clonecommon.S3LocalDir) is snapshot-keyed, so the snapshot-s3
// binary's checksum-aware idempotency means a re-download is a fast no-op when
// the artifacts are already present (e.g. a second clone on the same node). The
// Job itself is per-guest here; a SwiftGuestPool placing MANY replicas on ONE
// node would create concurrent same-path downloads — PR 4 (pool integration)
// should deduplicate the download per (node, snapshot) to avoid the concurrent-
// write race. For a single clone (or clones spread across nodes) this is correct.
func (r *SwiftGuestReconciler) ensureCloneDownloadJob(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	snap *snapshotv1alpha1.SwiftSnapshot,
	node string,
) (bool, string, error) {
	if r.SnapshotS3Image == "" {
		return false, "snapshot-s3 image not configured (set KUBESWIFT_SNAPSHOT_S3_IMAGE)", nil
	}
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: cloneDownloadJobName(guest), Namespace: guest.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		j := clonecommon.BuildDownloadJob(clonecommon.DownloadJobParams{
			Snapshot:    snap,
			Image:       r.SnapshotS3Image,
			Name:        cloneDownloadJobName(guest),
			Namespace:   guest.Namespace,
			Node:        node,
			Component:   "snapshot-s3-clone-download",
			ExtraLabels: map[string]string{"kubeswift.io/swiftguest": guest.Name},
		})
		if cerr := ctrl.SetControllerReference(guest, j, r.Scheme); cerr != nil {
			return false, "", cerr
		}
		if cerr := r.Create(ctx, j); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return false, "", cerr
		}
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true, "", nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, "snapshot download Job failed: " + c.Message, nil
		}
	}
	return false, "", nil
}
