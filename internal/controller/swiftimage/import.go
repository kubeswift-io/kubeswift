package swiftimage

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

const (
	importJobNamePrefix  = "swiftimage-import-"
	importPVCNamePrefix  = "swiftimage-import-"
	importVolumeMountPath = "/data"
	importOutputFile     = "image.raw"
)

// ImportResult holds the outcome of an import attempt.
type ImportResult struct {
	Phase   imagev1alpha1.SwiftImagePhase
	PVCRef  *imagev1alpha1.PVCObjectReference
	PVCPath string
	Error   string
}

// StartImport begins import for the given SwiftImage. Returns the next phase and any PVC reference.
func (r *SwiftImageReconciler) StartImport(ctx context.Context, img *imagev1alpha1.SwiftImage) (*ImportResult, error) {
	src := &img.Spec.Source
	switch {
	case src.HTTP != nil:
		return r.importHTTP(ctx, img)
	case src.PVCClone != nil:
		return r.importPVCClone(ctx, img)
	case src.Upload != nil:
		return &ImportResult{Phase: imagev1alpha1.SwiftImagePhasePending, Error: ReasonUploadNotImpl}, nil
	default:
		return &ImportResult{Phase: imagev1alpha1.SwiftImagePhaseFailed, Error: "no valid source specified"}, nil
	}
}

// importHTTP creates a PVC and Job to fetch the URL.
func (r *SwiftImageReconciler) importHTTP(ctx context.Context, img *imagev1alpha1.SwiftImage) (*ImportResult, error) {
	pvcName := importPVCNamePrefix + img.Name
	jobName := importJobNamePrefix + img.Name

	// Create PVC if not exists
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: img.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}
	if err := controllerutil.SetControllerReference(img, pvc, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	// Create Job if not exists
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: img.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{{
						Name:    "import",
						Image:   "curlimages/curl:latest",
						Command: []string{"sh", "-c", fmt.Sprintf("curl -L -o %s/%s %q", importVolumeMountPath, importOutputFile, img.Spec.Source.HTTP.URL)},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: importVolumeMountPath,
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
	if err := controllerutil.SetControllerReference(img, job, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	return &ImportResult{
		Phase:  imagev1alpha1.SwiftImagePhaseImporting,
		PVCRef: &imagev1alpha1.PVCObjectReference{Name: pvcName, Namespace: img.Namespace},
	}, nil
}

// importPVCClone creates a Job or uses CSI clone. Stub: returns Failed (not implemented).
func (r *SwiftImageReconciler) importPVCClone(ctx context.Context, img *imagev1alpha1.SwiftImage) (*ImportResult, error) {
	return &ImportResult{Phase: imagev1alpha1.SwiftImagePhaseFailed, Error: "pvcClone source not yet implemented"}, nil
}

// CheckImportStatus checks if an in-progress import (Job) has completed.
func (r *SwiftImageReconciler) CheckImportStatus(ctx context.Context, img *imagev1alpha1.SwiftImage) (imagev1alpha1.SwiftImagePhase, *imagev1alpha1.PVCObjectReference, string, error) {
	jobName := importJobNamePrefix + img.Name
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: img.Namespace, Name: jobName}, &job); err != nil {
		if errors.IsNotFound(err) {
			return imagev1alpha1.SwiftImagePhaseImporting, nil, "", nil
		}
		return imagev1alpha1.SwiftImagePhaseFailed, nil, err.Error(), nil
	}

	if job.Status.Succeeded > 0 {
		pvcRef := &imagev1alpha1.PVCObjectReference{
			Name:      importPVCNamePrefix + img.Name,
			Namespace: img.Namespace,
		}
		return imagev1alpha1.SwiftImagePhaseValidating, pvcRef, "", nil
	}
	if job.Status.Failed > 0 {
		msg := "import job failed"
		if len(job.Status.Conditions) > 0 {
			for _, c := range job.Status.Conditions {
				if c.Type == batchv1.JobFailed {
					msg = c.Message
					break
				}
			}
		}
		return imagev1alpha1.SwiftImagePhaseFailed, nil, msg, nil
	}
	return imagev1alpha1.SwiftImagePhaseImporting, nil, "", nil
}
