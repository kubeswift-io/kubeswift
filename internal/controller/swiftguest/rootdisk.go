package swiftguest

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// RootDiskCloneResult is the outcome of a successful EnsureRootDiskClone
// call. PVCName is always set; the other fields drive whether the launcher
// pod needs the clone-grow-init init container, and what arguments it
// should receive.
type RootDiskCloneResult struct {
	// PVCName is the bound clone PVC the launcher pod will mount.
	PVCName string
	// NeedsGrowInit is true when the snapshot path was used AND the
	// requested rootDisk.size is larger than the source snapshot's size.
	// In that case the launcher pod must run the clone-grow-init init
	// container before swiftletd starts. Always false for the legacy
	// Copy Job path.
	NeedsGrowInit bool
	// SourceSizeBytes is the size of the source snapshot (set only on the
	// snapshot path). The clone-grow-init container does NOT consume this;
	// it is informational, used by tests and debug.
	SourceSizeBytes int64
	// TargetSizeBytes is the SwiftGuestClass rootDisk.size. The
	// clone-grow-init container uses this as the target for qemu-img
	// resize. Always set on the snapshot path.
	TargetSizeBytes int64
}

// EnsureRootDiskClone ensures a per-guest root disk PVC exists, is bound
// at the SwiftGuestClass-driven size, and contains the source image data.
//
// Two paths:
//
//   - Copy Job path (legacy, default): used when the SwiftImage's
//     status.cloneSeed is nil (i.e. spec.cloneStrategy is "copy"). Behavior
//     is byte-identical to the pre-Phase-1 implementation.
//   - Snapshot path: used when status.cloneSeed.Kind = VolumeSnapshot. The
//     clone PVC is created at the snapshot's source size with
//     dataSource: VolumeSnapshot, then expanded via kubectl-patch to the
//     target size. The expand-and-wait gate (capacity == target before
//     returning success) is critical — if the launcher pod's
//     clone-grow-init runs qemu-img resize against an as-yet-unexpanded
//     PVC, the file grows beyond the underlying block device. See bug 45/46
//     history and Phase 0 spike §5.
//
// Returns nil result + error if not yet ready (errors are progress
// indicators in the existing pattern; the SwiftGuest controller treats
// them as "requeue and try again").
func (r *SwiftGuestReconciler) EnsureRootDiskClone(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
) (*RootDiskCloneResult, error) {
	cloneName := RootDiskCloneName(guest.Name)
	sourcePVC := rg.PreparedImage.PVCName
	targetSize := rg.RootDisk.Size

	if sourcePVC == "" {
		return nil, fmt.Errorf("SwiftImage has no prepared PVC")
	}
	if targetSize.IsZero() {
		targetSize = resource.MustParse("40Gi")
	}

	// Branch: snapshot path requires status.cloneSeed.Kind = VolumeSnapshot.
	if seed := rg.PreparedImage.CloneSeed; seed != nil && seed.Kind == "VolumeSnapshot" && seed.Name != "" {
		return r.ensureRootDiskCloneFromSnapshot(ctx, guest, sourcePVC, cloneName, targetSize, seed)
	}

	// Legacy Copy Job path. Behavior preserved byte-for-byte.
	return r.ensureRootDiskCloneFromCopy(ctx, guest, sourcePVC, cloneName, targetSize)
}

// ensureRootDiskCloneFromCopy is the legacy copy path (unchanged from
// pre-Phase-1 behavior).
func (r *SwiftGuestReconciler) ensureRootDiskCloneFromCopy(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	sourcePVC, cloneName string,
	targetSize resource.Quantity,
) (*RootDiskCloneResult, error) {
	jobName := CloneJobName(guest.Name)

	var existingPVC corev1.PersistentVolumeClaim
	pvcErr := r.Get(ctx, client.ObjectKey{Name: cloneName, Namespace: guest.Namespace}, &existingPVC)
	pvcExists := pvcErr == nil
	if pvcErr != nil && !errors.IsNotFound(pvcErr) {
		return nil, pvcErr
	}

	var existingJob batchv1.Job
	jobErr := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: guest.Namespace}, &existingJob)
	jobExists := jobErr == nil
	if jobErr != nil && !errors.IsNotFound(jobErr) {
		return nil, jobErr
	}

	if pvcExists {
		if !metav1.IsControlledBy(&existingPVC, guest) {
			if err := r.Delete(ctx, &existingPVC); err != nil && !errors.IsNotFound(err) {
				return nil, err
			}
			if jobExists {
				_ = r.Delete(ctx, &existingJob)
			}
			return nil, fmt.Errorf("deleted orphaned clone PVC %s, will recreate", cloneName)
		}
		if jobExists {
			if isJobComplete(&existingJob) {
				if existingPVC.Status.Phase != corev1.ClaimBound {
					return nil, fmt.Errorf("clone PVC %s not yet Bound (phase=%s)", cloneName, existingPVC.Status.Phase)
				}
				return &RootDiskCloneResult{PVCName: cloneName, NeedsGrowInit: false}, nil
			}
			if isJobFailed(&existingJob) {
				return nil, fmt.Errorf("clone Job %s failed", jobName)
			}
			return nil, fmt.Errorf("clone Job %s in progress", jobName)
		}
		if err := r.createCloneJob(ctx, guest, jobName, sourcePVC, cloneName, targetSize); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("clone Job %s created, waiting for completion", jobName)
	}

	var srcPVC corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: sourcePVC, Namespace: guest.Namespace}, &srcPVC); err != nil {
		return nil, fmt.Errorf("get source PVC %s: %w", sourcePVC, err)
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
	pvc.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(guest, swiftGuestGVK),
	}

	if err := r.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("clone PVC %s being created", cloneName)
		}
		return nil, fmt.Errorf("create clone PVC: %w", err)
	}

	return nil, fmt.Errorf("clone PVC %s created, waiting for Bound", cloneName)
}

// ensureRootDiskCloneFromSnapshot drives the snapshot strategy:
//
//  1. Create the clone PVC at the source snapshot's size with
//     dataSource: VolumeSnapshot{cloneSeed.Name}.
//  2. Wait for Bound.
//  3. If targetSize > sourceSize, kubectl-patch the PVC to expand. Wait
//     until status.capacity reflects the target (Longhorn ~50s for
//     10->40 GiB, see Phase 0 §5).
//  4. Return success with NeedsGrowInit=true so the launcher pod's
//     clone-grow-init runs qemu-img resize + sgdisk -e at boot.
//
// The expand-and-wait gate is critical for correctness — if it returns
// success with status.capacity still at sourceSize, the launcher's
// qemu-img resize would write past the underlying block device end.
func (r *SwiftGuestReconciler) ensureRootDiskCloneFromSnapshot(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	sourcePVC, cloneName string,
	targetSize resource.Quantity,
	seed *resolved.PreparedCloneSeed,
) (*RootDiskCloneResult, error) {
	if seed.Namespace != guest.Namespace {
		// Defensive — the validation webhook also rejects this.
		// Phase 0 §6a: cross-namespace dataSourceRef silently fails.
		return nil, fmt.Errorf("clone seed must be in same namespace as SwiftGuest: seed=%s/%s guest=%s/%s",
			seed.Namespace, seed.Name, guest.Namespace, guest.Name)
	}
	if seed.SourceSizeBytes <= 0 {
		return nil, fmt.Errorf("clone seed has zero source size; SwiftImage status incomplete")
	}
	sourceSize := *resource.NewQuantity(seed.SourceSizeBytes, resource.BinarySI)
	targetBytes := targetSize.Value()
	needsExpand := targetSize.Cmp(sourceSize) > 0

	var existingPVC corev1.PersistentVolumeClaim
	pvcErr := r.Get(ctx, client.ObjectKey{Name: cloneName, Namespace: guest.Namespace}, &existingPVC)
	if pvcErr != nil && !errors.IsNotFound(pvcErr) {
		return nil, pvcErr
	}
	pvcExists := pvcErr == nil

	if !pvcExists {
		// Read source snapshot's source PVC for storage class.
		var srcPVC corev1.PersistentVolumeClaim
		if err := r.Get(ctx, client.ObjectKey{Name: sourcePVC, Namespace: guest.Namespace}, &srcPVC); err != nil {
			return nil, fmt.Errorf("get source PVC %s: %w", sourcePVC, err)
		}
		apiGroup := "snapshot.storage.k8s.io"
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cloneName,
				Namespace: guest.Namespace,
				Labels: map[string]string{
					"swift.kubeswift.io/guest":                 guest.Name,
					"swift.kubeswift.io/role":                  "root-disk",
					"swift.kubeswift.io/clone-strategy":        "snapshot",
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(guest, swiftGuestGVK),
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: srcPVC.Spec.StorageClassName,
				DataSource: &corev1.TypedLocalObjectReference{
					APIGroup: &apiGroup,
					Kind:     "VolumeSnapshot",
					Name:     seed.Name,
				},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						// MUST be source size — Longhorn refuses different-size
						// dataSource clones (Phase 0 §5). Expansion happens after
						// Bound via kubectl-patch.
						corev1.ResourceStorage: sourceSize,
					},
				},
			},
		}
		if err := r.Create(ctx, pvc); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("clone PVC %s being created", cloneName)
			}
			return nil, fmt.Errorf("create snapshot clone PVC: %w", err)
		}
		return nil, fmt.Errorf("snapshot clone PVC %s created, waiting for Bound", cloneName)
	}

	if !metav1.IsControlledBy(&existingPVC, guest) {
		if err := r.Delete(ctx, &existingPVC); err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
		return nil, fmt.Errorf("deleted orphaned clone PVC %s, will recreate", cloneName)
	}

	if existingPVC.Status.Phase != corev1.ClaimBound {
		return nil, fmt.Errorf("snapshot clone PVC %s not yet Bound (phase=%s)", cloneName, existingPVC.Status.Phase)
	}

	if !needsExpand {
		// target == source: no expand step. clone-grow-init still useful
		// (sgdisk -e fixes the GPT backup header at the new file end), but
		// qemu-img resize is a noop. The init container handles both
		// uniformly — set NeedsGrowInit=true regardless when we are on
		// the snapshot path; the init script runs cheap operations.
		return &RootDiskCloneResult{
			PVCName:         cloneName,
			NeedsGrowInit:   true,
			SourceSizeBytes: seed.SourceSizeBytes,
			TargetSizeBytes: targetBytes,
		}, nil
	}

	// Expand-and-wait gate. Patch storage request to target if not already
	// requested, then wait for status.capacity to reflect the new size
	// before returning success. Without this gate, the launcher pod's
	// clone-grow-init runs qemu-img resize against an underlying block
	// device that has not yet expanded.
	currentRequest, ok := existingPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok || currentRequest.Cmp(targetSize) < 0 {
		patch := client.MergeFrom(existingPVC.DeepCopy())
		if existingPVC.Spec.Resources.Requests == nil {
			existingPVC.Spec.Resources.Requests = corev1.ResourceList{}
		}
		existingPVC.Spec.Resources.Requests[corev1.ResourceStorage] = targetSize
		if err := r.Patch(ctx, &existingPVC, patch); err != nil {
			return nil, fmt.Errorf("patch clone PVC for expansion: %w", err)
		}
		return nil, fmt.Errorf("snapshot clone PVC %s patched to %s, waiting for capacity", cloneName, targetSize.String())
	}

	currentCapacity, ok := existingPVC.Status.Capacity[corev1.ResourceStorage]
	if !ok || currentCapacity.Cmp(targetSize) < 0 {
		// Still expanding (Longhorn ~50s for 10->40 GiB).
		return nil, fmt.Errorf("snapshot clone PVC %s expanding (capacity=%s, target=%s)", cloneName, currentCapacity.String(), targetSize.String())
	}

	// Capacity is at target — safe to schedule the launcher pod.
	return &RootDiskCloneResult{
		PVCName:         cloneName,
		NeedsGrowInit:   true,
		SourceSizeBytes: seed.SourceSizeBytes,
		TargetSizeBytes: targetBytes,
	}, nil
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
