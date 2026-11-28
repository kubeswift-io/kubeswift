package swiftimagepublish

import (
	"sort"
	"strconv"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/oci"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
	"github.com/kubeswift-io/kubeswift/internal/version"
)

const (
	// LabelPublish names the SwiftImagePublish on the objects it creates.
	LabelPublish = "image.kubeswift.io/publish"

	diskMount      = "/disk"
	diskDevice     = "/dev/kubeswift-disk"
	authMount      = "/oras-auth"
	signKeyMount   = "/oras-signing-key"
	cosignHome     = "/tmp"
	publishUID     = int64(65532)
	diskGroup      = int64(6) // "disk": owns block device nodes
	jobBackoff     = int32(2)
	jobDeadlineSec = int64(6 * 60 * 60)
	jobTTLSec      = int32(24 * 60 * 60)
)

// Provenance annotations, on the manifest and (when signed) signed into the
// signature, where a SwiftArtifactVerificationPolicy can require them as
// claims. All are fixed for one request, so a re-run Job pushes the same
// manifest digest.
const (
	AnnotationPublish        = "io.kubeswift.publish.name"
	AnnotationNamespace      = "io.kubeswift.publish.namespace"
	AnnotationPublishUID     = "io.kubeswift.publish.uid"
	AnnotationSourceKind     = "io.kubeswift.source.kind"
	AnnotationSourceName     = "io.kubeswift.source.name"
	AnnotationSourceUID      = "io.kubeswift.source.uid"
	AnnotationVolumeSnapshot = "io.kubeswift.source.volumesnapshot"
	AnnotationVersion        = "io.kubeswift.version"
	// AnnotationCreated is set to the request's creation time. Left unset,
	// oras stamps the current time, and a Job retried after the tag was
	// pushed would push a different digest and fail with TagInUse.
	AnnotationCreated = ocispec.AnnotationCreated
)

func labels(pub *imagev1alpha1.SwiftImagePublish) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "kubeswift",
		"app.kubernetes.io/component": "image-publish",
		LabelPublish:                  names.LabelValue(pub.Name),
	}
}

// jobName is the publish Job's name.
func jobName(pub *imagev1alpha1.SwiftImagePublish) string {
	return names.JobName(pub.Name, "-publish")
}

// provenance is the annotation set the image carries.
func provenance(pub *imagev1alpha1.SwiftImagePublish, src *imagev1alpha1.ImagePublishSourceStatus) map[string]string {
	a := map[string]string{
		AnnotationPublish:    pub.Name,
		AnnotationNamespace:  pub.Namespace,
		AnnotationPublishUID: string(pub.UID),
		AnnotationSourceKind: src.Kind,
		AnnotationSourceName: src.Name,
		AnnotationVersion:    version.Version,
		AnnotationCreated:    pub.CreationTimestamp.UTC().Format(time.RFC3339),
	}
	if src.UID != "" {
		a[AnnotationSourceUID] = src.UID
	}
	if src.VolumeSnapshot != "" {
		a[AnnotationVolumeSnapshot] = src.VolumeSnapshot
	}
	return a
}

// buildJob is the publish Job: snapshot-oras --mode=upload-image reading the
// source volume read-only and pushing it in the golden-image format. It
// needs no node access, so it runs hardened: non-root, no capabilities, a
// read-only root filesystem, no service account token, no host path, and a
// deadline.
func buildJob(pub *imagev1alpha1.SwiftImagePublish, image string) *batchv1.Job {
	dst := pub.Status.Destination
	src := pub.Status.Source
	file := diskMount + "/image.raw"
	if src.Block {
		file = diskDevice
	}
	args := []string{
		"--mode=upload-image",
		"--file=" + file,
		"--repository=" + dst.Repository,
		"--tag=" + pub.Spec.Destination.Tag,
		"--chunk-size-mib=" + strconv.Itoa(oci.DefaultChunkSizeMiB(src.SizeBytes)),
		"--os-type=" + string(src.OSType),
	}
	if !pub.Spec.Destination.OverwriteTag {
		args = append(args, "--immutable-tag")
	}
	if dst.Insecure {
		args = append(args, "--insecure")
	}
	ann := provenance(pub, src)
	keys := make([]string, 0, len(ann))
	for k := range ann {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--annotation="+k+"="+ann[k])
	}

	c := corev1.Container{
		Name:  "publish",
		Image: image,
		Args:  args,
		// The root filesystem is read-only: cosign and the registry CA bundle
		// write their temporary files to an emptyDir.
		Env: append(clonecommon.RegistryCAEnvVars(dst.CABundle),
			corev1.EnvVar{Name: "HOME", Value: cosignHome},
			corev1.EnvVar{Name: "TMPDIR", Value: cosignHome}),
		VolumeMounts:             []corev1.VolumeMount{{Name: "tmp", MountPath: cosignHome}},
		TerminationMessagePolicy: clonecommon.TransferTerminationPolicy,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To(publishUID),
			RunAsGroup:               ptr.To(publishUID),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    mustQuantity("100m"),
				corev1.ResourceMemory: mustQuantity("128Mi"),
			},
			Limits: corev1.ResourceList{
				// One chunk buffer plus a zero buffer, up to 256 MiB each.
				corev1.ResourceMemory: mustQuantity("768Mi"),
			},
		},
	}
	vols := []corev1.Volume{
		{
			Name: "disk",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: src.PVCName, ReadOnly: true,
			}},
		},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	podSC := &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if src.Block {
		c.VolumeDevices = []corev1.VolumeDevice{{Name: "disk", DevicePath: diskDevice}}
		// A block device node is root:disk 0660; reading it needs the group,
		// not root.
		podSC.SupplementalGroups = []int64{diskGroup}
	} else {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "disk", MountPath: diskMount, ReadOnly: true})
	}
	if dst.CredentialsSecretName != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: authMount})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "oras-auth", MountPath: authMount, ReadOnly: true})
		vols = append(vols, corev1.Volume{Name: "oras-auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: dst.CredentialsSecretName,
			Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
		}}})
	}
	// Signing is not optional when the destination has a key.
	if dst.SigningKeySecretName != "" {
		c.Args = append(c.Args, "--sign-key="+signKeyMount+"/cosign.key")
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "signing-key", MountPath: signKeyMount, ReadOnly: true})
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "COSIGN_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: dst.SigningKeySecretName},
				Key:                  "cosign.password",
				Optional:             ptr.To(true),
			}}})
		vols = append(vols,
			corev1.Volume{Name: "signing-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: dst.SigningKeySecretName,
				Items:      []corev1.KeyToPath{{Key: "cosign.key", Path: "cosign.key"}},
			}}})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(pub),
			Namespace: pub.Namespace,
			Labels:    labels(pub),
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(jobBackoff),
			ActiveDeadlineSeconds:   ptr.To(jobDeadlineSec),
			TTLSecondsAfterFinished: ptr.To(jobTTLSec),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(pub)},
				Spec: corev1.PodSpec{
					RestartPolicy:                clonecommon.TransferRestartPolicy,
					AutomountServiceAccountToken: ptr.To(false),
					EnableServiceLinks:           ptr.To(false),
					SecurityContext:              podSC,
					Containers:                   []corev1.Container{c},
					Volumes:                      vols,
				},
			},
		},
	}
}
