package swiftkernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
)

const (
	kernelHostBasePath = "/var/lib/kubeswift/kernels"
	orasImage          = "ghcr.io/oras-project/oras:v1.3.1"
)

// pullJobName returns a valid Kubernetes Job name for a SwiftKernel pull on a node.
// Dots in names are replaced with dashes. If the full name exceeds 63 characters,
// it is truncated to 40 characters plus "-" and the first 8 hex digits of sha256(skName+nodeName).
func pullJobName(skName, nodeName string) string {
	sk := strings.ReplaceAll(skName, ".", "-")
	nn := strings.ReplaceAll(nodeName, ".", "-")
	base := fmt.Sprintf("swiftkernel-pull-%s-%s", sk, nn)
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(skName + nodeName))
	suffix := hex.EncodeToString(sum[:4])
	return base[:40] + "-" + suffix
}

// pullScript returns a shell script that pulls OCI artifacts into destDir.
func pullScript(image, destDir string) string {
	return fmt.Sprintf(`set -e
mkdir -p %q
cd %q
oras pull %q
echo "Pull complete"
ls -lh .`,
		destDir, destDir, image)
}

// StartPullOnNode creates the pull Job for the SwiftKernel scheduled on the given node.
func (r *SwiftKernelReconciler) StartPullOnNode(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, nodeName string) error {
	if sk.Spec.OCIRef.Image == "" {
		return fmt.Errorf("spec.ociRef.image is required")
	}
	jobName := pullJobName(sk.Name, nodeName)
	destDir := kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name)
	script := pullScript(sk.Spec.OCIRef.Image, destDir)

	podSpec := corev1.PodSpec{
		NodeSelector: map[string]string{
			"kubeswift.io/kernel-node": "true",
			"kubernetes.io/hostname":   nodeName,
		},
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

// CheckNodePullStatus inspects the pull Job for a specific node.
func (r *SwiftKernelReconciler) CheckNodePullStatus(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, nodeName string) (phase kernelv1alpha1.SwiftKernelPhase, errMsg string, err error) {
	jobName := pullJobName(sk.Name, nodeName)
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: sk.Namespace, Name: jobName}, &job); err != nil {
		if errors.IsNotFound(err) {
			return kernelv1alpha1.SwiftKernelPhasePending, "", nil
		}
		return "", "", err
	}

	if job.Status.Succeeded > 0 {
		return kernelv1alpha1.SwiftKernelPhaseReady, "", nil
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
		return kernelv1alpha1.SwiftKernelPhaseFailed, msg, nil
	}
	return kernelv1alpha1.SwiftKernelPhasePulling, "", nil
}
