// Tier C (s3 / object-storage export) support for SwiftSnapshot — Phase 3.
//
// The s3 backend reuses Tier B's node-local capture, then runs a node-pinned
// upload Job that pushes the captured artifacts to S3 via the snapshot-s3 image.
// This file builds that Job; the phase-machine wiring (Capturing -> Uploading ->
// Ready) lands in a follow-up so this controller change stays small.
package swiftsnapshot

import (
	"path"
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

const (
	// s3UploadMount is where the node-local snapshot dir is mounted (read-only)
	// inside the upload Job.
	s3UploadMount = "/snap"
	// s3UploadBackoffLimit bounds Job retries; the snapshot-s3 binary is
	// idempotent (skips already-uploaded objects), so a retry resumes.
	s3UploadBackoffLimit int32 = 4
)

// s3LocalDir is the node-local hostPath directory the s3 backend captures into
// (and the upload Job reads from). Derived deterministically — the s3 backend
// does not take an operator-supplied hostPath (unlike the local backend).
func s3LocalDir(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return filepath.Join(HostPathBaseDir, snap.Namespace+"-"+snap.Name)
}

// s3KeyPrefix is the object-key prefix for this snapshot:
// <prefix>/<namespace>/<name>.
func s3KeyPrefix(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return path.Join(snap.Spec.Backend.S3.Prefix, snap.Namespace, snap.Name)
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
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							RunAsNonRoot:             ptr.To(true),
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
