// Tier C (s3 / object-storage) restore path for SwiftRestore — Phase 3.
//
// An s3-backed SwiftSnapshot stores its artifacts in object storage, so the
// restore is cross-node/cross-cluster portable: a node-pinned download Job
// pulls the artifacts from S3 into a node-local cache on the chosen target
// node, then the EXISTING Tier B restore tail (materializeRestoreTarget) takes
// over unchanged — CH --restore from the node-local cache.
//
//	Pending -> Downloading -> Restoring -> Resuming -> Ready
//	           (download Job   (shared Tier B path: stamp/clone the target
//	            -> node cache)   SwiftGuest, restore-receive launcher, resume)
package swiftrestore

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/snapshot/clonecommon"
)

// s3DownloadMount aliases the shared mount path (referenced by tests + the
// node-local cache layout); the download Job itself is built by clonecommon.
const s3DownloadMount = clonecommon.DownloadMount

// hypervisorVersionBlocked runs the architect-risk-#3 CH version compatibility
// gate shared by the local and s3 restore paths. Returns true (and stamps
// Failed on status) when the restore must be blocked; false to proceed.
func (r *SwiftRestoreReconciler) hypervisorVersionBlocked(
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) bool {
	// Operator can override via SkipHypervisorVersionCheckAnnotation for
	// disaster-recovery scenarios where the cluster's CH was upgraded past the
	// snapshot's. Default policy: block on minor or major mismatch, allow patch
	// drift with a Warning, allow exact match.
	if restore.Annotations[SkipHypervisorVersionCheckAnnotation] == "true" || r.CurrentHypervisorVersion == "" {
		return false
	}
	switch CompareHypervisorVersions(snap.Status.HypervisorVersion, r.CurrentHypervisorVersion) {
	case VersionMajorMismatch:
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"hypervisor major-version mismatch: snapshot "+snap.Status.HypervisorVersion+
				" vs cluster "+r.CurrentHypervisorVersion+
				" (override with annotation "+SkipHypervisorVersionCheckAnnotation+"=true)")
		return true
	case VersionMinorMismatch:
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"hypervisor minor-version mismatch: snapshot "+snap.Status.HypervisorVersion+
				" vs cluster "+r.CurrentHypervisorVersion+
				" (override with annotation "+SkipHypervisorVersionCheckAnnotation+"=true)")
		return true
	case VersionPatchDrift, VersionExactMatch, VersionUnknown:
		// Proceed. Patch drift could be surfaced as a Warning condition in a
		// future iteration — the version fields on status are enough for now.
	}
	return false
}

// s3RestoreLocalDir is the node-local cache directory the download Job writes
// to (and CH --restore then reads). Derived deterministically from the
// snapshot's identity so it is stable across reconciles and matches the
// capture-side layout when the target happens to be the capture node.
func s3RestoreLocalDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return clonecommon.S3LocalDir(snap)
}

// s3RestoreKeyPrefix is the object-key prefix the artifacts live under, derived
// the same way the upload side derives it: <prefix>/<namespace>/<name>.
func s3RestoreKeyPrefix(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return clonecommon.S3KeyPrefix(snap)
}

// s3DownloadJobName is the deterministic name of the download Job.
func s3DownloadJobName(restore *snapshotv1alpha1.SwiftRestore) string {
	return restore.Name + "-s3-download"
}

// resolveS3RestoreNode picks the node the download Job + restore-receive
// launcher run on. spec.targetNode wins; otherwise (in-place restore) the
// target SwiftGuest's currently-assigned node is used. Returns (node, failMsg,
// err): a non-empty failMsg means the caller should mark the restore Failed; a
// non-nil err is transient.
func (r *SwiftRestoreReconciler) resolveS3RestoreNode(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
) (string, string, error) {
	if restore.Spec.TargetNode != "" {
		return restore.Spec.TargetNode, "", nil
	}
	var guest swiftv1alpha1.SwiftGuest
	err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &guest)
	if apierrors.IsNotFound(err) {
		return "", "s3 restore requires spec.targetNode: target SwiftGuest " + restore.Spec.TargetGuest.Name +
			" does not exist (clone/cross-node restore) and no spec.targetNode was set", nil
	}
	if err != nil {
		return "", "", err
	}
	if guest.Status.NodeName == "" {
		return "", "s3 restore: target SwiftGuest " + guest.Name +
			" has no assigned node yet; set spec.targetNode to pin the restore", nil
	}
	return guest.Status.NodeName, "", nil
}

// ensureDownloadJob creates the node-pinned download Job (idempotent) owned by
// the SwiftRestore. Fails if the snapshot-s3 image is not configured.
func (r *SwiftRestoreReconciler) ensureDownloadJob(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	node string,
) error {
	if r.SnapshotS3Image == "" {
		return fmt.Errorf("snapshot-s3 image not configured (set KUBESWIFT_SNAPSHOT_S3_IMAGE)")
	}
	job := buildDownloadJob(restore, snap, r.SnapshotS3Image, node)
	if err := ctrl.SetControllerReference(restore, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// handlePendingS3 validates the uploaded snapshot, resolves the target node,
// and starts the download Job. Advances to Downloading.
func (r *SwiftRestoreReconciler) handlePendingS3(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	if r.hypervisorVersionBlocked(restore, snap, status) {
		return true, 0, nil
	}
	if snap.Spec.Backend.S3 == nil {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"SwiftSnapshot "+snap.Name+" has no backend.s3 — s3 restore requires the object-store config")
		return true, 0, nil
	}
	if snap.Status.S3 == nil || snap.Status.S3.Location == "" {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"SwiftSnapshot "+snap.Name+" has no status.s3 — s3 restore requires a completed upload")
		return true, 0, nil
	}
	node, failMsg, err := r.resolveS3RestoreNode(ctx, restore, snap)
	if err != nil {
		return false, 0, err
	}
	if failMsg != "" {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed, failMsg)
		return true, 0, nil
	}
	if err := r.ensureDownloadJob(ctx, restore, snap, node); err != nil {
		return false, 0, err
	}
	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseDownloading)
	setReadyCondition(status, metav1.ConditionFalse, ReasonDownloading,
		"downloading snapshot from "+snap.Status.S3.Location+" to node "+node)
	return true, 0, nil
}

// handleDownloading watches the download Job. On Complete it hands off to the
// shared Tier B restore tail (materializeRestoreTarget) with the node-local
// cache dir + resolved node; on Failed it returns an errMsg (caller -> Failed).
// Returns (advanced, requeue, errMsg, err) mirroring handleRestoringLocal.
func (r *SwiftRestoreReconciler) handleDownloading(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, string, error) {
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.SnapshotRef.Name, Namespace: restore.Namespace}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return false, 0, "source SwiftSnapshot " + restore.Spec.SnapshotRef.Name + " disappeared during download", nil
		}
		return false, 0, "", err
	}

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: s3DownloadJobName(restore), Namespace: restore.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		// Job missing (controller restarted before observing creation, or it
		// was deleted). Recreate — idempotent, and the binary resumes.
		node, failMsg, rerr := r.resolveS3RestoreNode(ctx, restore, &snap)
		if rerr != nil {
			return false, 0, "", rerr
		}
		if failMsg != "" {
			return false, 0, failMsg, nil
		}
		if cerr := r.ensureDownloadJob(ctx, restore, &snap, node); cerr != nil {
			return false, 0, "", cerr
		}
		return false, 5 * time.Second, "", nil
	}
	if err != nil {
		return false, 0, "", err
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			node, failMsg, rerr := r.resolveS3RestoreNode(ctx, restore, &snap)
			if rerr != nil {
				return false, 0, "", rerr
			}
			if failMsg != "" {
				return false, 0, failMsg, nil
			}
			// Hand off to the shared Tier B tail with the node-local cache.
			advanced, requeue, merr := r.materializeRestoreTarget(ctx, restore, &snap, status, s3RestoreLocalDir(&snap), node)
			return advanced, requeue, "", merr
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, 0, "S3 download Job failed: " + c.Message, nil
		}
	}
	return false, 5 * time.Second, "", nil // still downloading
}

// buildDownloadJob constructs the node-pinned download Job. It mounts the
// node-local restore cache read-write (DirectoryOrCreate — the dir may not
// exist on a fresh target node), takes S3 credentials from the snapshot's
// referenced Secret as the standard AWS env vars, and runs snapshot-s3
// --mode=download. Pinned to the resolved restore node. The caller sets the
// SwiftRestore ownerRef for GC.
func buildDownloadJob(restore *snapshotv1alpha1.SwiftRestore, snap *snapshotv1alpha1.SwiftSnapshot, image, node string) *batchv1.Job {
	return clonecommon.BuildDownloadJob(clonecommon.DownloadJobParams{
		Snapshot:    snap,
		Image:       image,
		Name:        s3DownloadJobName(restore),
		Namespace:   restore.Namespace,
		Node:        node,
		Component:   "snapshot-s3-download",
		ExtraLabels: map[string]string{"kubeswift.io/swiftrestore": restore.Name},
	})
}
