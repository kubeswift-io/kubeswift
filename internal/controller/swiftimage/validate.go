package swiftimage

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

const measureJobNamePrefix = "swiftimage-measure-"

// ValidateResult holds the outcome of validation.
type ValidateResult struct {
	OK    bool
	Path  string
	Size  int64
	Error string
}

// Validate verifies the image exists and measures its size via a measurement job.
func (r *SwiftImageReconciler) Validate(ctx context.Context, img *imagev1alpha1.SwiftImage, pvcPath string) (*ValidateResult, error) {
	if r.Clientset == nil {
		return &ValidateResult{OK: false, Error: "clientset not configured"}, nil
	}
	// Import PVC is at importPVCNamePrefix+img.Name
	pvcName := importPVCNamePrefix + img.Name
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: img.Namespace, Name: pvcName}, &pvc); err != nil {
		if errors.IsNotFound(err) {
			return &ValidateResult{OK: false, Error: "import PVC not found"}, nil
		}
		return nil, err
	}

	jobName := measureJobNamePrefix + img.Name
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: img.Namespace, Name: jobName}, &job); err != nil {
		if errors.IsNotFound(err) {
			// Create measurement job
			measureJob := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: img.Namespace},
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers: []corev1.Container{{
								Name:    "measure",
								Image:   "ubuntu:22.04",
								Command: []string{"sh", "-c", "cat /data/image.raw.size"},
								VolumeMounts: []corev1.VolumeMount{{
									Name:      "data",
									MountPath: "/data",
								}},
							}},
							Volumes: []corev1.Volume{{
								Name: "data",
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
								},
							}},
						},
					},
				},
			}
			if err := controllerutil.SetControllerReference(img, measureJob, r.Scheme); err != nil {
				return nil, err
			}
			if err := r.Create(ctx, measureJob); err != nil {
				return nil, err
			}
			return &ValidateResult{OK: false, Error: "measuring"}, nil
		}
		return nil, err
	}

	// Job exists: check status
	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return &ValidateResult{OK: false, Error: "measuring"}, nil
	}
	if job.Status.Failed > 0 {
		return &ValidateResult{OK: false, Error: "size measurement failed"}, nil
	}

	// Job succeeded: get logs from completed pod
	selector, err := labels.Parse("batch.kubernetes.io/job-name=" + jobName)
	if err != nil {
		return nil, err
	}
	podList, err := r.Clientset.CoreV1().Pods(img.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	var completedPod *corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Status.Phase == corev1.PodSucceeded {
			completedPod = p
			break
		}
	}
	if completedPod == nil {
		return &ValidateResult{OK: false, Error: "measuring"}, nil
	}

	stream, err := r.Clientset.CoreV1().Pods(img.Namespace).GetLogs(completedPod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	logBytes, err := io.ReadAll(stream)
	if err != nil {
		return nil, err
	}
	logStr := strings.TrimSpace(string(logBytes))
	if logStr == "" {
		return &ValidateResult{OK: false, Error: "size measurement produced empty output"}, nil
	}
	// Take first line in case of extra output
	scanner := bufio.NewScanner(strings.NewReader(logStr))
	if !scanner.Scan() {
		return &ValidateResult{OK: false, Error: "size measurement produced empty output"}, nil
	}
	firstLine := strings.TrimSpace(scanner.Text())
	size, err := strconv.ParseInt(firstLine, 10, 64)
	if err != nil {
		return &ValidateResult{OK: false, Error: "size measurement: invalid size value"}, nil
	}
	if size <= 0 {
		return &ValidateResult{OK: false, Error: "size measurement: size must be positive"}, nil
	}

	return &ValidateResult{OK: true, Size: size, Path: pvcPath}, nil
}
