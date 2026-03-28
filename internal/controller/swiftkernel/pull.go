package swiftkernel

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kernelv1alpha1 "github.com/projectbeskar/kubeswift/api/kernel/v1alpha1"
)

const (
	pullJobNamePrefix  = "swiftkernel-pull-"
	kernelHostBasePath = "/var/lib/kubeswift/kernels"
	orasImage          = "ghcr.io/oras-project/oras:v1.2.2"
)

func destDirFor(sk *kernelv1alpha1.SwiftKernel) string {
	return fmt.Sprintf("%s/%s-%s", kernelHostBasePath, sk.Namespace, sk.Name)
}

// pullScript returns a shell script that pulls OCI artifacts into destDir.
func pullScript(image, destDir string) string {
	return fmt.Sprintf(`set -e
mkdir -p %q
cd %q
oras pull %q --media-type application/vnd.kubeswift.kernel.binary,application/vnd.kubeswift.initramfs.binary
ls -lh
`, destDir, destDir, image)
}

// StartPull creates the pull Job for the SwiftKernel.
func (r *SwiftKernelReconciler) StartPull(ctx context.Context, sk *kernelv1alpha1.SwiftKernel) error {
	if sk.Spec.OCIRef.Image == "" {
		return fmt.Errorf("spec.ociRef.image is required")
	}
	jobName := pullJobNamePrefix + sk.Name
	destDir := destDirFor(sk)
	script := pullScript(sk.Spec.OCIRef.Image, destDir)

	podSpec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser: ptr.To(int64(0)),
		},
		Containers: []corev1.Container{{
			Name:    "pull",
			Image:   orasImage,
			Command: []string{"sh", "-c", script},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      "kernels",
				MountPath: kernelHostBasePath,
			}},
		}},
		Volumes: []corev1.Volume{{
			Name: "kernels",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: kernelHostBasePath,
					Type: ptr.To(corev1.HostPathDirectoryOrCreate),
				},
			},
		}},
	}
	if sk.Spec.OCIRef.PullSecret != "" {
		podSpec.ImagePullSecrets = []corev1.LocalObjectReference{
			{Name: sk.Spec.OCIRef.PullSecret},
		}
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: sk.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: podSpec,
			},
		},
	}
	if err := controllerutil.SetControllerReference(sk, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// CheckPullStatus returns whether the pull Job finished and the local path on success.
func (r *SwiftKernelReconciler) CheckPullStatus(ctx context.Context, sk *kernelv1alpha1.SwiftKernel) (done bool, localPath string, errMsg string, err error) {
	jobName := pullJobNamePrefix + sk.Name
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: sk.Namespace, Name: jobName}, &job); err != nil {
		if errors.IsNotFound(err) {
			return false, "", "", nil
		}
		return false, "", "", err
	}

	if job.Status.Succeeded > 0 {
		return true, destDirFor(sk), "", nil
	}
	if job.Status.Failed > 0 {
		msg := "pull job failed"
		if len(job.Status.Conditions) > 0 {
			for _, c := range job.Status.Conditions {
				if c.Type == batchv1.JobFailed {
					msg = c.Message
					break
				}
			}
		}
		return true, "", msg, nil
	}
	return false, "", "", nil
}
