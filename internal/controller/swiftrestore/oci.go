// oci (OCI registry / ORAS) restore support for SwiftRestore.
//
// The oci backend pulls the snapshot's OCI artifact from the registry into a
// node-local cache via the snapshot-oras image, then the EXISTING Tier B restore
// tail (materializeRestoreTarget) takes over — identical to the s3 path, just a
// registry pull instead of an object-store download. The restore pulls by the
// captured manifest digest (status.oci.manifestDigest), so it materializes the
// exact artifact regardless of any later retag.
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

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
)

// ociDownloadJobName is the deterministic name of the download Job.
func ociDownloadJobName(restore *snapshotv1alpha1.SwiftRestore) string {
	return names.JobName(restore.Name, "-oci-download")
}

// downloadBackendIsOCI reports whether the restore's referenced snapshot uses
// the oci backend, so the Downloading phase picks the right download handler.
// Any lookup error returns false; the s3 handler then surfaces the real error.
func (r *SwiftRestoreReconciler) downloadBackendIsOCI(ctx context.Context, restore *snapshotv1alpha1.SwiftRestore) bool {
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: restore.Spec.SnapshotRef.Name, Namespace: restore.Namespace}, &snap); err != nil {
		return false
	}
	return snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendOCI
}

// handlePendingOCI validates the pushed snapshot, resolves the target node, and
// starts the download Job. Advances to Downloading.
func (r *SwiftRestoreReconciler) handlePendingOCI(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	status *snapshotv1alpha1.SwiftRestoreStatus,
) (bool, time.Duration, error) {
	if r.hypervisorVersionBlocked(restore, snap, status) {
		return true, 0, nil
	}
	if _, ok := clonecommon.SnapshotOCI(snap); !ok {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"SwiftSnapshot "+snap.Name+" records no registry (status.location or spec.backend.oci) — oci restore requires one")
		return true, 0, nil
	}
	if snap.Status.OCI == nil || snap.Status.OCI.ManifestDigest == "" {
		setPhase(status, snapshotv1alpha1.SwiftRestorePhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonRestoreFailed,
			"SwiftSnapshot "+snap.Name+" has no status.oci — oci restore requires a completed push")
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
	if err := r.ensureOCIDownloadJob(ctx, restore, snap, node); err != nil {
		return false, 0, err
	}
	setPhase(status, snapshotv1alpha1.SwiftRestorePhaseDownloading)
	setReadyCondition(status, metav1.ConditionFalse, ReasonDownloading,
		"pulling snapshot from "+snap.Status.OCI.Reference+" to node "+node)
	return true, 0, nil
}

// handleDownloadingOCI watches the download Job. On Complete it hands off to the
// shared Tier B restore tail (materializeRestoreTarget) with the node-local
// cache dir; on Failed it returns an errMsg. Mirrors handleDownloading.
func (r *SwiftRestoreReconciler) handleDownloadingOCI(
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
	err := r.Get(ctx, client.ObjectKey{Name: ociDownloadJobName(restore), Namespace: restore.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		node, failMsg, rerr := r.resolveS3RestoreNode(ctx, restore, &snap)
		if rerr != nil {
			return false, 0, "", rerr
		}
		if failMsg != "" {
			return false, 0, failMsg, nil
		}
		if cerr := r.ensureOCIDownloadJob(ctx, restore, &snap, node); cerr != nil {
			return false, 0, "", cerr
		}
		return false, 5 * time.Second, "", nil
	}
	if err != nil {
		return false, 0, "", err
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			if status.DownloadedBytes == 0 {
				if rep, ok, rerr := clonecommon.JobTransferReport(ctx, r.Client, restore.Namespace, ociDownloadJobName(restore)); rerr == nil && ok {
					status.DownloadedBytes = rep.TotalBytes
					metrics.RestoreDownloadBytesTotal.Add(float64(rep.TransferredBytes))
				}
			}
			node, failMsg, rerr := r.resolveS3RestoreNode(ctx, restore, &snap)
			if rerr != nil {
				return false, 0, "", rerr
			}
			if failMsg != "" {
				return false, 0, failMsg, nil
			}
			advanced, requeue, merr := r.materializeRestoreTarget(ctx, restore, &snap, status, s3RestoreLocalDir(&snap), node)
			return advanced, requeue, "", merr
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, 0, "OCI download Job failed: " + clonecommon.JobFailureMessage(ctx, r.Client, &job, c.Message), nil
		}
	}
	return false, 5 * time.Second, "", nil // still downloading
}

// ensureOCIDownloadJob creates the node-pinned download Job (idempotent) owned
// by the SwiftRestore. Fails if the snapshot-oras image is not configured.
func (r *SwiftRestoreReconciler) ensureOCIDownloadJob(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	snap *snapshotv1alpha1.SwiftSnapshot,
	node string,
) error {
	if r.SnapshotORASImage == "" {
		return fmt.Errorf("snapshot-oras image not configured (set KUBESWIFT_SNAPSHOT_ORAS_IMAGE)")
	}
	ca, err := clonecommon.TransferCA(ctx, r.Client, snap)
	if err != nil {
		return err
	}
	job := buildOCIDownloadJob(restore, snap, r.SnapshotORASImage, node, ca)
	if err := ctrl.SetControllerReference(restore, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create oci download Job: %w", err)
	}
	return nil
}

// buildOCIDownloadJob constructs the node-pinned download Job (pinned to the
// resolved restore node). Pulls by digest for the exact captured artifact;
// credentials, when configured, come from the dockerconfigjson Secret the
// snapshot recorded, read in the snapshot's namespace.
func buildOCIDownloadJob(restore *snapshotv1alpha1.SwiftRestore, snap *snapshotv1alpha1.SwiftSnapshot, image, node, caBundle string) *batchv1.Job {
	oci, _ := clonecommon.SnapshotOCI(snap)
	return clonecommon.BuildOCIDownloadJob(clonecommon.OCIDownloadJobParams{
		Snapshot:              snap,
		Repository:            oci.Repository,
		Tag:                   oci.Tag,
		Digest:                snap.Status.OCI.ManifestDigest,
		Insecure:              oci.Insecure,
		CredentialsSecretName: oci.CredentialsSecretName,
		CABundle:              caBundle,
		Image:                 image,
		Name:                  ociDownloadJobName(restore),
		Namespace:             restore.Namespace,
		Node:                  node,
		Component:             "snapshot-oci-download",
		ExtraLabels:           map[string]string{"kubeswift.io/swiftrestore": names.LabelValue(restore.Name)},
	})
}
