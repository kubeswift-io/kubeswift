package swiftguest

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/runtime/schema"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/resolved"
)

var swiftGuestGVK = schema.GroupVersionKind{
	Group:   "swift.kubeswift.io",
	Version: "v1alpha1",
	Kind:    "SwiftGuest",
}

const (
	RootDiskPVCPrefix       = "swiftguest-root-"
	CloneJobPrefix          = "swiftguest-rootclone-"
	CloneJobImage           = "ubuntu:22.04"
	ConditionRootDiskCloned = "RootDiskCloned"
)

// RootDiskCloneName returns the deterministic clone PVC name for a guest.
func RootDiskCloneName(guestName string) string {
	return RootDiskPVCPrefix + guestName
}

// CloneJobName returns the deterministic clone Job name for a guest.
func CloneJobName(guestName string) string {
	return CloneJobPrefix + guestName
}

// EnsureRootDiskClone ensures a per-guest root disk PVC exists and is populated.
// Returns the clone PVC name when ready, or an error if still in progress.
func (r *SwiftGuestReconciler) EnsureRootDiskClone(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
) (string, error) {
	cloneName := RootDiskCloneName(guest.Name)
	jobName := CloneJobName(guest.Name)
	sourcePVC := rg.PreparedImage.PVCName
	targetSize := rg.RootDisk.Size

	if sourcePVC == "" {
		return "", fmt.Errorf("SwiftImage has no prepared PVC")
	}
	if targetSize.IsZero() {
		targetSize = resource.MustParse("40Gi")
	}

	// Check if clone PVC exists.
	var existingPVC corev1.PersistentVolumeClaim
	pvcErr := r.Get(ctx, client.ObjectKey{Name: cloneName, Namespace: guest.Namespace}, &existingPVC)
	pvcExists := pvcErr == nil
	if pvcErr != nil && !errors.IsNotFound(pvcErr) {
		return "", pvcErr
	}

	// Check if clone Job exists.
	var existingJob batchv1.Job
	jobErr := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: guest.Namespace}, &existingJob)
	jobExists := jobErr == nil
	if jobErr != nil && !errors.IsNotFound(jobErr) {
		return "", jobErr
	}

	if pvcExists {
		// Verify owner matches current guest.
		if !metav1.IsControlledBy(&existingPVC, guest) {
			// Orphaned PVC from a previous guest with the same name.
			if err := r.Delete(ctx, &existingPVC); err != nil && !errors.IsNotFound(err) {
				return "", err
			}
			if jobExists {
				_ = r.Delete(ctx, &existingJob)
			}
			return "", fmt.Errorf("deleted orphaned clone PVC %s, will recreate", cloneName)
		}

		if existingPVC.Status.Phase != corev1.ClaimBound {
			return "", fmt.Errorf("clone PVC %s not yet Bound (phase=%s)", cloneName, existingPVC.Status.Phase)
		}

		// PVC is Bound. Check Job status.
		if jobExists {
			if isJobComplete(&existingJob) {
				return cloneName, nil // Ready
			}
			if isJobFailed(&existingJob) {
				return "", fmt.Errorf("clone Job %s failed", jobName)
			}
			return "", fmt.Errorf("clone Job %s in progress", jobName)
		}

		// PVC Bound but no Job -- create the clone Job.
		if err := r.createCloneJob(ctx, guest, jobName, sourcePVC, cloneName, targetSize); err != nil {
			return "", err
		}
		return "", fmt.Errorf("clone Job %s created, waiting for completion", jobName)
	}

	// No PVC -- create it.
	// Read source PVC to get storage class.
	var srcPVC corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: sourcePVC, Namespace: guest.Namespace}, &srcPVC); err != nil {
		return "", fmt.Errorf("get source PVC %s: %w", sourcePVC, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloneName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
				"swift.kubeswift.io/role":  "root-disk",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: srcPVC.Spec.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: targetSize,
				},
			},
		},
	}

	// Set owner reference so the PVC is garbage collected with the guest.
	pvc.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(guest, swiftGuestGVK),
	}

	if err := r.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			return "", fmt.Errorf("clone PVC %s being created", cloneName)
		}
		return "", fmt.Errorf("create clone PVC: %w", err)
	}

	return "", fmt.Errorf("clone PVC %s created, waiting for Bound", cloneName)
}

func (r *SwiftGuestReconciler) createCloneJob(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	jobName, sourcePVC, clonePVC string,
	targetSize resource.Quantity,
) error {
	targetBytes := targetSize.Value()

	script := fmt.Sprintf(`set -e
echo "Cloning root disk from %s to %s (%d bytes)"
cp /src/image.raw /dst/image.raw
apt-get update -qq && apt-get install -y -qq qemu-utils gdisk >/dev/null 2>&1
qemu-img resize -f raw /dst/image.raw %d
sgdisk -e /dst/image.raw
sync
echo "Clone complete: $(stat -c %%s /dst/image.raw) bytes"`, sourcePVC, clonePVC, targetBytes, targetBytes)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
				"swift.kubeswift.io/role":  "root-disk-clone",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(guest, swiftGuestGVK),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:    "clone",
						Image:   CloneJobImage,
						Command: []string{"/bin/sh", "-c", script},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "src", MountPath: "/src", ReadOnly: true},
							{Name: "dst", MountPath: "/dst"},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "src",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: sourcePVC,
									ReadOnly:  true,
								},
							},
						},
						{
							Name: "dst",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: clonePVC,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := r.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create clone Job: %w", err)
	}
	return nil
}

func isJobComplete(job *batchv1.Job) bool {
	return job.Status.Succeeded > 0
}

func isJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
