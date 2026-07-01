package clonecommon

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

const (
	// DownloadMount is where the node-local cache is mounted (RW) inside the
	// download Job — the snapshot-s3 binary writes the artifacts here.
	DownloadMount = "/snap"
	// DownloadBackoffLimit bounds Job retries; the snapshot-s3 binary is
	// idempotent (skips already-downloaded objects, verifies checksums), so a
	// retry resumes.
	DownloadBackoffLimit int32 = 4
)

// DownloadJobParams parameterizes the node-pinned s3 download Job so both the
// SwiftRestore path and the SwiftGuest cloneFromSnapshot path can build it. The
// caller sets the ownerRef (SwiftRestore or SwiftGuest) after construction.
type DownloadJobParams struct {
	// Snapshot supplies the s3 backend config + the derived key prefix/cache dir.
	Snapshot *snapshotv1alpha1.SwiftSnapshot
	// Image is the snapshot-s3 uploader/downloader image.
	Image string
	// Name / Namespace of the Job.
	Name      string
	Namespace string
	// Node pins the Job (and thus the cache hostPath) to the restore/clone node.
	Node string
	// Component is the app.kubernetes.io/component label value.
	Component string
	// ExtraLabels are merged onto the standard labels (e.g. an owner-name label).
	ExtraLabels map[string]string
}

// BuildDownloadJob constructs the node-pinned download Job: it pulls the
// snapshot's artifacts from object storage into the node-local cache hostPath
// (S3LocalDir) and sha256-verifies them against the manifest. Credentials come
// from the snapshot's referenced Secret as the standard AWS env vars. Runs as
// root because it writes the kubelet-created root-owned cache hostPath; still
// drop ALL / no-priv-esc / ro-rootfs, and the mount exposes only the single
// snapshot dir.
func BuildDownloadJob(p DownloadJobParams) *batchv1.Job {
	snap := p.Snapshot
	s3 := snap.Spec.Backend.S3
	args := []string{
		"--mode=download",
		"--dir=" + DownloadMount,
		"--bucket=" + s3.Bucket,
		"--key-prefix=" + S3KeyPrefix(snap),
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

	labels := map[string]string{
		"app.kubernetes.io/name":      "kubeswift",
		"app.kubernetes.io/component": p.Component,
	}
	for k, v := range p.ExtraLabels {
		labels[k] = v
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(DownloadBackoffLimit),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      p.Node,
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:  "download",
						Image: p.Image,
						Args:  args,
						Env: []corev1.EnvVar{
							secretEnv("AWS_ACCESS_KEY_ID", "accessKeyId", false),
							secretEnv("AWS_SECRET_ACCESS_KEY", "secretAccessKey", false),
							secretEnv("AWS_SESSION_TOKEN", "sessionToken", true),
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "cache",
							MountPath: DownloadMount,
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							RunAsUser:                ptr.To(int64(0)),
							RunAsNonRoot:             ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "cache",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: S3LocalDir(snap),
								Type: ptr.To(corev1.HostPathDirectoryOrCreate),
							},
						},
					}},
				},
			},
		},
	}
}

// ociDownloadAuthMount is where the dockerconfigjson credential is mounted for
// the oci download Job; DOCKER_CONFIG points here so oras-go reads config.json.
const ociDownloadAuthMount = "/oras-auth"

// OCIDownloadJobParams parameterizes the node-pinned oci (ORAS) download Job for
// both the SwiftRestore and cloneFromSnapshot paths. The restore pulls by
// Digest (status.oci.manifestDigest) so it materializes the exact captured
// artifact; Tag is passed for the reference. The caller sets the ownerRef.
type OCIDownloadJobParams struct {
	// Snapshot supplies the node-local cache dir (S3LocalDir, backend-neutral).
	Snapshot *snapshotv1alpha1.SwiftSnapshot
	// Repository / Tag / Digest identify the artifact; Digest pins it.
	Repository string
	Tag        string
	Digest     string
	Insecure   bool
	// CredentialsSecretName is a dockerconfigjson Secret; "" = anonymous.
	CredentialsSecretName string
	// Image is the snapshot-oras uploader/downloader image.
	Image string
	// Name / Namespace / Node of the Job; Component is the component label.
	Name, Namespace, Node, Component string
	ExtraLabels                      map[string]string
}

// BuildOCIDownloadJob constructs the node-pinned oci download Job: it pulls the
// snapshot's OCI artifact from the registry into the node-local cache hostPath
// (S3LocalDir) — ORAS verifies every blob's digest against the manifest. Runs
// as root because it writes the kubelet-created root-owned cache hostPath; still
// drop ALL / no-priv-esc / ro-rootfs. Registry credentials, when configured,
// come from a dockerconfigjson Secret at DOCKER_CONFIG; else anonymous.
func BuildOCIDownloadJob(p OCIDownloadJobParams) *batchv1.Job {
	args := []string{
		"--mode=download",
		"--dir=" + DownloadMount,
		"--repository=" + p.Repository,
		"--tag=" + p.Tag,
	}
	if p.Digest != "" {
		args = append(args, "--digest="+p.Digest)
	}
	if p.Insecure {
		args = append(args, "--insecure")
	}

	volumes := []corev1.Volume{{
		Name: "cache",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: S3LocalDir(p.Snapshot),
				Type: ptr.To(corev1.HostPathDirectoryOrCreate),
			},
		},
	}}
	mounts := []corev1.VolumeMount{{Name: "cache", MountPath: DownloadMount}}
	var env []corev1.EnvVar
	if p.CredentialsSecretName != "" {
		env = append(env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: ociDownloadAuthMount})
		mounts = append(mounts, corev1.VolumeMount{Name: "oras-auth", MountPath: ociDownloadAuthMount, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "oras-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: p.CredentialsSecretName,
					Items:      []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
				},
			},
		})
	}

	labels := map[string]string{
		"app.kubernetes.io/name":      "kubeswift",
		"app.kubernetes.io/component": p.Component,
	}
	for k, v := range p.ExtraLabels {
		labels[k] = v
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(DownloadBackoffLimit),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      p.Node,
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:         "download",
						Image:        p.Image,
						Args:         args,
						Env:          env,
						VolumeMounts: mounts,
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
