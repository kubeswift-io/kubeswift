// oci (OCI registry / ORAS) backend support for SwiftSnapshot.
//
// The oci backend reuses Tier B's node-local capture, then runs a node-pinned
// push Job that packages the captured artifacts as an OCI artifact and pushes
// them to a registry via the snapshot-oras image. This file builds that Job and
// drives the Capturing -> Uploading -> Ready transition (ensureOCIPushJob +
// handleUploadingOCI); the controller's phase switch routes an oci-backend
// Uploading phase here. It mirrors s3.go, with two differences: registry
// credentials come from a mounted dockerconfigjson (not AWS env vars), and the
// artifact is content-addressed so status.oci pins it by manifest digest.
package swiftsnapshot

import (
	"context"
	"fmt"
	"os"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
)

// SnapshotORASImageEnv overrides the snapshot-oras uploader/downloader image
// used by the oci backend's push + download Jobs.
const SnapshotORASImageEnv = "KUBESWIFT_SNAPSHOT_ORAS_IMAGE"

// SnapshotORASImageDefault is the fallback when SnapshotORASImageEnv is unset
// (the Helm chart overrides it with a version-pinned tag). Mirrors
// SnapshotS3ImageDefault so a chart-less deploy still resolves a usable image.
const SnapshotORASImageDefault = "ghcr.io/kubeswift-io/kubeswift/snapshot-oras:latest"

// SnapshotORASImage returns the snapshot-oras image, from SnapshotORASImageEnv
// or SnapshotORASImageDefault.
func SnapshotORASImage() string {
	if img := os.Getenv(SnapshotORASImageEnv); img != "" {
		return img
	}
	return SnapshotORASImageDefault
}

const (
	// ociUploadMount is where the node-local snapshot dir is mounted (read-only)
	// inside the push Job.
	ociUploadMount = "/snap"
	// ociAuthMount is where the dockerconfigjson credential is mounted; DOCKER_CONFIG
	// points here so oras-go's credential store reads <dir>/config.json.
	ociAuthMount = "/oras-auth"
	// ociSignKeyMount is where the cosign signing key is mounted; snapshot-oras
	// --sign-key points at <dir>/cosign.key.
	ociSignKeyMount = "/oras-signing-key"
	// ociCosignHome is a writable dir for cosign's HOME/TMPDIR (the container
	// rootfs is read-only).
	ociCosignHome = "/cosign-home"
	// ociPushBackoffLimit bounds Job retries; snapshot-oras is idempotent (the
	// registry dedups already-present layers by digest), so a retry resumes.
	ociPushBackoffLimit int32 = 4
)

// ociLocalDir is the node-local hostPath directory the oci backend captures into
// (and the push Job reads from). Shares the backend-neutral snapshot cache path.
func ociLocalDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return clonecommon.S3LocalDir(snap)
}

// ociTag resolves the artifact tag: the operator-supplied tag, or a stable
// per-snapshot default of "<namespace>-<name>".
func ociTag(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if snap.Spec.Backend.OCI != nil && snap.Spec.Backend.OCI.Tag != "" {
		return snap.Spec.Backend.OCI.Tag
	}
	return snap.Namespace + "-" + snap.Name
}

// ociReference is the "repository:tag" recorded in status.
func ociReference(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return snap.Spec.Backend.OCI.Repository + ":" + ociTag(snap)
}

// ociSigningRequested reports whether the snapshot opted into cosign provenance
// signing via a signing-key Secret (P2).
func ociSigningRequested(snap *snapshotv1alpha1.SwiftSnapshot) bool {
	oci := snap.Spec.Backend.OCI
	return oci != nil && oci.SigningKeySecretRef != nil && oci.SigningKeySecretRef.Name != ""
}

// ociPushJobName is the deterministic name of the push Job.
func ociPushJobName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return snap.Name + "-oci-push"
}

// ensureOCIPushJob creates the node-pinned push Job (idempotent) owned by the
// SwiftSnapshot. Fails if the snapshot-oras image is not configured.
func (r *SwiftSnapshotReconciler) ensureOCIPushJob(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) error {
	if r.SnapshotORASImage == "" {
		return fmt.Errorf("snapshot-oras image not configured (set %s)", SnapshotORASImageEnv)
	}
	job := buildOCIPushJob(snap, r.SnapshotORASImage, status.NodeName)
	if err := ctrl.SetControllerReference(snap, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create oci push Job: %w", err)
	}
	return nil
}

// handleUploadingOCI watches the push Job. On Complete it stamps status.OCI and
// transitions to Ready; on Failed it returns an errMsg (caller -> Failed).
// Returns (ready, errMsg, err) following handleUploading's contract.
func (r *SwiftSnapshotReconciler) handleUploadingOCI(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) (bool, string, error) {
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: ociPushJobName(snap), Namespace: snap.Namespace}, &job)
	if apierrors.IsNotFound(err) {
		// Job missing (controller restarted before observing creation, or it was
		// deleted). Recreate — idempotent, and the binary resumes.
		if cerr := r.ensureOCIPushJob(ctx, snap, status); cerr != nil {
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
			// Preserve the full-state disk ref (P4 includeDisk): the disk is
			// captured before this memory push, so status.OCI.Disk is already set
			// and must survive this (re)assignment of status.OCI.
			var disk *snapshotv1alpha1.OCIDiskArtifact
			if status.OCI != nil {
				disk = status.OCI.Disk
			}
			status.OCI = &snapshotv1alpha1.OCISnapshotStatus{
				Reference: ociReference(snap),
				PushedAt:  &now,
				// A completed push Job with signing requested is signed (strict:
				// a signing failure fails the Job). Robust against a missing byte
				// report (which would leave rep.Signed false).
				Signed: ociSigningRequested(snap),
				Disk:   disk,
			}
			// Read the push Job's byte report (best-effort; a missing report leaves
			// bytes/digest empty and is not a failure). status carries the registry
			// footprint + the pinned digest; the metric counts actual wire traffic.
			if rep, ok, rerr := clonecommon.JobTransferReport(ctx, r.Client, snap.Namespace, ociPushJobName(snap)); rerr == nil && ok {
				status.OCI.PushedBytes = rep.TotalBytes
				status.OCI.ManifestDigest = rep.ManifestDigest
				metrics.SnapshotUploadBytesTotal.Add(float64(rep.TransferredBytes))
			}
			setPhase(status, snapshotv1alpha1.SwiftSnapshotPhaseReady)
			setReadyCondition(status, metav1.ConditionTrue, ReasonSnapshotReady,
				"snapshot pushed to "+ociReference(snap))
			return true, "", nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, "OCI push Job failed: " + c.Message, nil
		}
	}
	return false, "", nil // still pushing
}

// buildOCIPushJob constructs the node-pinned push Job. It mounts the captured
// snapshot dir read-only and runs snapshot-oras --mode=upload. Pinned to the
// capture node (status.NodeName) because the artifacts live in that node's
// hostPath. Registry credentials, when configured, come from a dockerconfigjson
// Secret mounted at ociAuthMount with DOCKER_CONFIG pointed at it; an empty
// credentialsSecretRef means anonymous access. The caller sets the ownerRef.
func buildOCIPushJob(snap *snapshotv1alpha1.SwiftSnapshot, image, captureNode string) *batchv1.Job {
	oci := snap.Spec.Backend.OCI
	args := []string{
		"--mode=upload",
		"--dir=" + ociUploadMount,
		"--repository=" + oci.Repository,
		"--tag=" + ociTag(snap),
		"--snapshot=" + snap.Namespace + "/" + snap.Name,
	}
	if oci.Insecure {
		args = append(args, "--insecure")
	}
	if snap.Spec.IncludeMemory {
		args = append(args, "--include-memory")
	}

	volumes := []corev1.Volume{{
		Name: "snapshot",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: ociLocalDir(snap),
				Type: ptr.To(corev1.HostPathDirectory),
			},
		},
	}}
	mounts := []corev1.VolumeMount{{
		Name:      "snapshot",
		MountPath: ociUploadMount,
		ReadOnly:  true,
	}}
	var env []corev1.EnvVar
	if oci.CredentialsSecretRef != nil && oci.CredentialsSecretRef.Name != "" {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: ociAuthMount})
		mounts = append(mounts, corev1.VolumeMount{Name: "oras-auth", MountPath: ociAuthMount, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "oras-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: oci.CredentialsSecretRef.Name,
					// A kubernetes.io/dockerconfigjson Secret stores the auth under
					// .dockerconfigjson; oras-go expects a Docker config.json.
					Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
				},
			},
		})
	}

	// P2 provenance signing: mount the cosign key + password and tell
	// snapshot-oras to sign the pushed digest as an OCI referrer. cosign needs a
	// writable HOME/TMPDIR because the container rootfs is read-only.
	if ociSigningRequested(snap) {
		args = append(args, "--sign-key="+ociSignKeyMount+"/cosign.key")
		mounts = append(mounts,
			corev1.VolumeMount{Name: "oras-signing-key", MountPath: ociSignKeyMount, ReadOnly: true},
			corev1.VolumeMount{Name: "cosign-home", MountPath: ociCosignHome},
		)
		env = append(env,
			// Password for the encrypted key. Optional so a missing key fails
			// loudly inside cosign (strict) rather than blocking pod creation.
			corev1.EnvVar{Name: "COSIGN_PASSWORD", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: oci.SigningKeySecretRef.Name},
					Key:                  "cosign.password",
					Optional:             ptr.To(true),
				},
			}},
			corev1.EnvVar{Name: "HOME", Value: ociCosignHome},
			corev1.EnvVar{Name: "TMPDIR", Value: ociCosignHome},
		)
		volumes = append(volumes,
			corev1.Volume{
				Name: "oras-signing-key",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: oci.SigningKeySecretRef.Name,
						Items:      []corev1.KeyToPath{{Key: "cosign.key", Path: "cosign.key"}},
					},
				},
			},
			corev1.Volume{
				Name:         "cosign-home",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		)
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ociPushJobName(snap),
			Namespace: snap.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kubeswift",
				"app.kubernetes.io/component": "snapshot-oci-push",
				"kubeswift.io/swiftsnapshot":  snap.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(ociPushBackoffLimit),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      captureNode,
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:         "push",
						Image:        image,
						Args:         args,
						Env:          env,
						VolumeMounts: mounts,
						// Runs as root: the capture writes the artifacts (config.json,
						// state.json, memory-ranges) as root mode 0600 (serialized
						// guest RAM), so a non-root container cannot read them even via
						// a read-only mount. Mirrors buildUploadJob. Otherwise maximally
						// constrained: drop ALL, no privilege escalation, read-only
						// rootfs; the mounts expose only the snapshot dir + the
						// credential file.
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							RunAsUser:                ptr.To(int64(0)),
							RunAsNonRoot:             ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
}
