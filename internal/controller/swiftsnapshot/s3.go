// Tier C (s3 / object-storage export) support for SwiftSnapshot — Phase 3.
//
// The s3 backend reuses Tier B's node-local capture, then runs a node-pinned
// upload Job that pushes the captured artifacts to S3 via the snapshot-s3 image.
// This file builds that Job and drives the Capturing -> Uploading -> Ready
// transition (ensureUploadJob + handleUploading); the controller's phase switch
// routes the Uploading phase here.
package swiftsnapshot

import (
	"context"
	"fmt"
	"os"
	"path"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/snapshot/clonecommon"
)

// SnapshotS3ImageEnv overrides the snapshot-s3 uploader/downloader image used
// by the Tier C (s3) upload + download Jobs.
const SnapshotS3ImageEnv = "KUBESWIFT_SNAPSHOT_S3_IMAGE"

// SnapshotS3ImageDefault is the fallback when SnapshotS3ImageEnv is unset (the
// Helm chart overrides it with a version-pinned tag). Mirrors swiftguest's
// LauncherImage pattern so a chart-less deploy (make deploy / kustomize) still
// resolves a usable image rather than failing "image not configured".
const SnapshotS3ImageDefault = "ghcr.io/projectbeskar/kubeswift/snapshot-s3:latest"

// SnapshotS3Image returns the snapshot-s3 image, from SnapshotS3ImageEnv or
// SnapshotS3ImageDefault. Used to populate the SwiftSnapshot and SwiftRestore
// reconcilers' SnapshotS3Image field.
func SnapshotS3Image() string {
	if img := os.Getenv(SnapshotS3ImageEnv); img != "" {
		return img
	}
	return SnapshotS3ImageDefault
}

// ensureUploadJob creates the node-pinned upload Job (idempotent) owned by the
// SwiftSnapshot. Fails if the snapshot-s3 image is not configured.
func (r *SwiftSnapshotReconciler) ensureUploadJob(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) error {
	if r.SnapshotS3Image == "" {
		return fmt.Errorf("snapshot-s3 image not configured (set KUBESWIFT_SNAPSHOT_S3_IMAGE)")
	}
	job := buildUploadJob(snap, r.SnapshotS3Image, status.NodeName)
	if err := ctrl.SetControllerReference(snap, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create s3 upload Job: %w", err)
	}
	return nil
}

// handleUploading watches the upload Job. On Complete it stamps status.S3 and
// transitions to Ready; on Failed it returns an errMsg (caller -> Failed).
// Returns (ready, errMsg, err) following handleCapturing's contract.
func (r *SwiftSnapshotReconciler) handleUploading(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) (bool, string, error) {
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: s3UploadJobName(snap), Namespace: snap.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		// Job missing (e.g. controller restarted before observing creation, or
		// it was deleted). Recreate — idempotent, and the binary resumes.
		if cerr := r.ensureUploadJob(ctx, snap, status); cerr != nil {
			return false, "", cerr
		}
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			now := metav1.Now()
			status.S3 = &snapshotv1alpha1.S3SnapshotStatus{
				Location:   s3Location(snap),
				UploadedAt: &now,
			}
			setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseReady)
			setReadyCondition(status, metav1.ConditionTrue, ReasonSnapshotReady,
				"snapshot uploaded to "+s3Location(snap))
			return true, "", nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, "S3 upload Job failed: " + c.Message, nil
		}
	}
	return false, "", nil // still uploading
}

const (
	// s3UploadMount is where the node-local snapshot dir is mounted (read-only)
	// inside the upload Job.
	s3UploadMount = "/snap"
	// s3UploadBackoffLimit bounds Job retries; the snapshot-s3 binary is
	// idempotent (skips already-uploaded objects), so a retry resumes.
	s3UploadBackoffLimit int32 = 4
)

// captureDestDir is the node-local directory the launcher captures the snapshot
// into. The local backend uses the operator-supplied hostPath; the s3 backend
// uses a controller-derived dir the upload Job then reads.
func captureDestDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if snap.Spec.Backend.Type == snapshotv1alpha1.SnapshotBackendS3 {
		return s3LocalDir(snap)
	}
	if snap.Spec.Backend.Local != nil {
		return snap.Spec.Backend.Local.HostPath
	}
	return ""
}

// s3LocalDir is the node-local hostPath directory the s3 backend captures into
// (and the upload Job reads from). Derived deterministically — the s3 backend
// does not take an operator-supplied hostPath (unlike the local backend).
func s3LocalDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return clonecommon.S3LocalDir(snap)
}

// s3KeyPrefix is the object-key prefix for this snapshot:
// <prefix>/<namespace>/<name>.
func s3KeyPrefix(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return clonecommon.S3KeyPrefix(snap)
}

// s3UploadJobName is the deterministic name of the upload Job.
func s3UploadJobName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return snap.Name + "-s3-upload"
}

// s3Location is the s3:// URI of this snapshot's prefix, recorded in status.
func s3Location(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return "s3://" + path.Join(snap.Spec.Backend.S3.Bucket, s3KeyPrefix(snap)) + "/"
}

// buildUploadJob constructs the node-pinned upload Job. It mounts the captured
// snapshot dir read-only, takes S3 credentials from the referenced Secret as
// the standard AWS env vars, and runs snapshot-s3 --mode=upload. Pinned to the
// capture node (status.NodeName) because the artifacts live in that node's
// hostPath. The caller sets the SwiftSnapshot ownerRef for GC.
func buildUploadJob(snap *snapshotv1alpha1.SwiftSnapshot, image, captureNode string) *batchv1.Job {
	s3 := snap.Spec.Backend.S3
	args := []string{
		"--mode=upload",
		"--dir=" + s3UploadMount,
		"--bucket=" + s3.Bucket,
		"--key-prefix=" + s3KeyPrefix(snap),
		"--snapshot=" + snap.Namespace + "/" + snap.Name,
	}
	if s3.Region != "" {
		args = append(args, "--region="+s3.Region)
	}
	if s3.Endpoint != "" {
		args = append(args, "--endpoint="+s3.Endpoint)
	}
	if s3.ForcePathStyle {
		args = append(args, "--path-style")
	}
	if s3.Insecure {
		args = append(args, "--insecure")
	}
	if snap.Spec.IncludeMemory {
		args = append(args, "--include-memory")
	}

	credName := ""
	if s3.CredentialsSecretRef != nil {
		credName = s3.CredentialsSecretRef.Name
	}
	secretEnv := func(envName, key string, optional bool) corev1.EnvVar {
		return corev1.EnvVar{
			Name: envName,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: credName},
					Key:                  key,
					Optional:             ptr.To(optional),
				},
			},
		}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s3UploadJobName(snap),
			Namespace: snap.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kubeswift",
				"app.kubernetes.io/component": "snapshot-s3-upload",
				"kubeswift.io/swiftsnapshot":  snap.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(s3UploadBackoffLimit),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      captureNode,
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:  "upload",
						Image: image,
						Args:  args,
						Env: []corev1.EnvVar{
							secretEnv("AWS_ACCESS_KEY_ID", "accessKeyId", false),
							secretEnv("AWS_SECRET_ACCESS_KEY", "secretAccessKey", false),
							secretEnv("AWS_SESSION_TOKEN", "sessionToken", true),
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "snapshot",
							MountPath: s3UploadMount,
							ReadOnly:  true,
						}},
						// Runs as root: the capture writes the snapshot artifacts
						// (config.json, state.json, memory-ranges) as root with
						// mode 0600 — they contain serialized guest RAM, so the
						// restrictive perms are deliberate — and a non-root upload
						// container cannot read them even via a read-only mount
						// (read-only constrains writes, not the file's own mode
						// bits). Mirrors the download Job. Otherwise maximally
						// constrained: drop ALL, no privilege escalation, read-only
						// rootfs; the mount exposes only the single snapshot dir.
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							RunAsUser:                ptr.To(int64(0)),
							RunAsNonRoot:             ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "snapshot",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: s3LocalDir(snap),
								Type: ptr.To(corev1.HostPathDirectory),
							},
						},
					}},
				},
			},
		},
	}
}
