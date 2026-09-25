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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kernelv1alpha1 "github.com/kubeswift-io/kubeswift/api/kernel/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
)

const (
	kernelHostBasePath = "/var/lib/kubeswift/kernels"
	orasImage          = "ghcr.io/oras-project/oras:v1.3.1"

	// pullKernelLabel names the SwiftKernel on its pull Jobs, so they can be
	// listed or deleted by selector: a Job's name ends in a hash.
	pullKernelLabel = "kubeswift.io/swiftkernel"
)

// pullJobName returns the name of the Job that pulls a SwiftKernel into
// destDir on a node.
//
// A succeeded pull Job is what marks a node Ready, so the name carries a hash
// of the node and of destDir: a Job that pulled into one directory must not
// stand for another. Named for the kernel and node alone, the Job that pulled
// into <namespace>-<name> went on reporting success after v0.14.1 moved
// kernels to <namespace>/<name>, no pull into the new directory ran, and
// guests booted against it empty (#658). A new directory now means a Job name
// the next reconcile does not find, so it pulls again, and the node reports
// Pulling until that Job succeeds.
//
// The hash also separates what the readable part cannot: kernel "a-b" on node
// "c" and kernel "a" on node "b-c", or nodes "n.1" and "n-1", shared one Job.
func pullJobName(skName, nodeName, destDir string) string {
	sk := strings.ReplaceAll(skName, ".", "-")
	nn := strings.ReplaceAll(nodeName, ".", "-")
	sum := sha256.Sum256([]byte(nodeName + "\x00" + destDir))
	return names.JobName("swiftkernel-pull-"+sk+"-"+nn, "-"+hex.EncodeToString(sum[:4]))
}

// pullImageEnv carries the user-supplied OCI reference to the pull container.
// It is NOT interpolated into the script: the shell does not re-evaluate the
// contents of a variable it expands, so a reference containing $(...) or
// backticks is inert. Interpolating it (even via %q, which escapes " and \ but
// NOT $ or `) put attacker-controlled command substitution inside a root
// container holding a node hostPath mount.
const pullImageEnv = "OCI_IMAGE"

// pullScript returns a shell script that pulls OCI artifacts into destDir.
// destDir is controller-derived (KernelLocalPath), not user input.
func pullScript(destDir string) string {
	return fmt.Sprintf(`set -e
mkdir -p %q
cd %q
oras pull "$%s"
echo "Pull complete"
ls -lh .`,
		destDir, destDir, pullImageEnv)
}

// StartPullOnNode creates the pull Job for the SwiftKernel scheduled on the given node.
func (r *SwiftKernelReconciler) StartPullOnNode(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, nodeName string) error {
	if sk.Spec.OCIRef.Image == "" {
		return fmt.Errorf("spec.ociRef.image is required")
	}
	destDir := kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name)
	jobName := pullJobName(sk.Name, nodeName, destDir)
	script := pullScript(destDir)

	podSpec := corev1.PodSpec{
		NodeSelector: map[string]string{
			"kubeswift.io/kernel-node": "true",
			corev1.LabelHostname:       nodeName,
		},
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser: ptr.To(int64(0)),
		},
		Containers: []corev1.Container{{
			Name:    "pull",
			Image:   orasImage,
			Command: []string{"sh", "-c", script},
			Env:     []corev1.EnvVar{{Name: pullImageEnv, Value: sk.Spec.OCIRef.Image}},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: sk.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kubeswift",
				"app.kubernetes.io/component": "swiftkernel-pull",
				pullKernelLabel:               names.LabelValue(sk.Name),
			},
		},
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

// CheckNodePullStatus inspects the pull Job for a specific node. Only the Job
// for the kernel's current directory counts: one that pulled elsewhere says
// nothing about the files there, so without it the node is Pending and the
// caller starts a pull.
func (r *SwiftKernelReconciler) CheckNodePullStatus(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, nodeName string) (phase kernelv1alpha1.SwiftKernelPhase, errMsg string, err error) {
	jobName := pullJobName(sk.Name, nodeName, kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name))
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
	// A failed pod is not a failed pull: the Job starts another, up to its
	// backoff limit. Failed is final for a SwiftKernel, so only the Job's own
	// Failed condition may put it there.
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			msg := c.Message
			if msg == "" {
				msg = "pull job failed"
			}
			return kernelv1alpha1.SwiftKernelPhaseFailed, msg, nil
		}
	}
	return kernelv1alpha1.SwiftKernelPhasePulling, "", nil
}

// deleteSupersededPullJobs deletes this kernel's pull Jobs for the given nodes
// whose name is not the node's current one: Jobs that pulled into a directory
// the kernel no longer uses, such as <namespace>-<name> before v0.14.1. They
// are matched by owner and node rather than by label, because releases before
// v0.15.0 created them without labels. Call it only once each node's current
// Job exists. Jobs for nodes that are no longer kernel nodes are left alone.
func (r *SwiftKernelReconciler) deleteSupersededPullJobs(ctx context.Context, sk *kernelv1alpha1.SwiftKernel, nodeNames []string) error {
	destDir := kernelv1alpha1.KernelLocalPath(sk.Namespace, sk.Name)
	current := make(map[string]string, len(nodeNames))
	for _, n := range nodeNames {
		current[n] = pullJobName(sk.Name, n, destDir)
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(sk.Namespace)); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !metav1.IsControlledBy(job, sk) {
			continue
		}
		name, ok := current[job.Spec.Template.Spec.NodeSelector[corev1.LabelHostname]]
		if !ok || job.Name == name {
			continue
		}
		// Background, so the Job's pods go with it: deleting a batch/v1 Job
		// orphans its pods by default.
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
