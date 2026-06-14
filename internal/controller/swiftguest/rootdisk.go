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

	// RestoreSeededLabel marks a per-guest root-disk PVC that was
	// pre-populated by SwiftRestore from a VolumeSnapshot. When set to
	// "true", the SwiftGuest controller skips both the Copy Job and the
	// snapshot-strategy expand-and-wait — the PVC is treated as
	// authoritative once Bound.
	RestoreSeededLabel = "swift.kubeswift.io/restore-seeded"
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
		return r.ensureRootDiskCloneFromSnapshot(ctx, guest, rg, sourcePVC, cloneName, targetSize, seed)
	}

	// Legacy Copy Job path. Behavior preserved byte-for-byte EXCEPT for the
	// new resolved-storage-spec inputs (accessMode, volumeMode,
	// storageClassName) flowing into PVC creation. Defaults preserve the
	// pre-PR-32 behaviour: RWO + Filesystem + class-of-source-image.
	return r.ensureRootDiskCloneFromCopy(ctx, guest, rg, sourcePVC, cloneName, targetSize)
}

// ensureRootDiskCloneFromCopy is the legacy copy path. Behaviour is
// byte-identical to the pre-Phase-1 implementation EXCEPT that PVC
// creation now sources accessMode/volumeMode/storageClassName from the
// resolved storage spec (rg.Storage). Default-resolved values preserve
// the pre-PR-32 behaviour: RWO + Filesystem + class-of-source-image.
func (r *SwiftGuestReconciler) ensureRootDiskCloneFromCopy(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
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
		// Restore-seeded PVCs are pre-populated from a VolumeSnapshot by
		// SwiftRestore. They carry RestoreSeededLabel=true and are owned
		// by the SwiftRestore (not by this SwiftGuest), which means the
		// orphan check below would otherwise delete the snapshot data.
		// Check the label FIRST and short-circuit before the orphan
		// branch fires. Skip the Copy Job entirely — its cp from the
		// source SwiftImage's PVC would overwrite the restore data.
		// Wait for Bound, then hand the PVC straight to the launcher pod.
		if existingPVC.Labels[RestoreSeededLabel] == "true" {
			if existingPVC.Status.Phase != corev1.ClaimBound {
				return nil, fmt.Errorf("restore-seeded PVC %s not yet Bound (phase=%s)", cloneName, existingPVC.Status.Phase)
			}
			if jobExists {
				// Defensive — a stale Copy Job from a previous run could
				// still be lingering. Delete it.
				_ = r.Delete(ctx, &existingJob)
			}
			return &RootDiskCloneResult{PVCName: cloneName, NeedsGrowInit: false}, nil
		}
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
		if err := r.createCloneJob(ctx, guest, rg, jobName, sourcePVC, cloneName, targetSize); err != nil {
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
			AccessModes:      resolvedAccessModes(rg),
			VolumeMode:       resolvedVolumeMode(rg),
			StorageClassName: resolvedStorageClassName(rg, &srcPVC),
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
	rg *resolved.ResolvedGuest,
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
					"swift.kubeswift.io/guest":          guest.Name,
					"swift.kubeswift.io/role":           "root-disk",
					"swift.kubeswift.io/clone-strategy": "snapshot",
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(guest, swiftGuestGVK),
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      resolvedAccessModes(rg),
				VolumeMode:       resolvedVolumeMode(rg),
				StorageClassName: resolvedStorageClassName(rg, &srcPVC),
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

// CloneJobBlockDevicePath is the in-Job device path for a Block-mode
// destination PVC. Only meaningful inside the Copy Job's pod namespace —
// the launcher pod uses its own constant (DiskRootDevicePath in pod.go).
// The two are deliberately distinct: Copy Job device naming is internal
// to a one-shot Job and need not align with the launcher's branding.
const CloneJobBlockDevicePath = "/dev/dst-block"

// createCloneJob creates the per-guest root-disk Copy Job. Branches on
// the resolved storage volumeMode (PR #32 surface):
//
//   - Filesystem (default): byte-identical to pre-W9 behaviour. Mounts
//     the destination PVC as a filesystem path and runs
//     `cp + qemu-img resize + sgdisk -e + sync`.
//   - Block: mounts the destination PVC via VolumeDevices as a raw block
//     device and runs `qemu-img convert + sgdisk -e + sync`. No
//     `qemu-img resize` (no-op on block devices: size is fixed at PVC
//     provision time by the storage layer; including it as a no-op
//     invites future confusion). No `cp` (cp can't write to a raw
//     device; qemu-img convert handles the bulk transfer atomically and
//     supports sparse-aware writes).
//
// The source PVC mount is unchanged (read-only filesystem mount) for
// both branches — SwiftImage import PVCs stay on Filesystem per W9
// scoping question (a)'s default. SwiftImage.spec.storage for direct
// Block imports is deferred.
// launcherTargetNode returns the node the guest's launcher pod will run on, when
// it is deterministically known: an explicitly pinned guest (spec.nodeName —
// also set for GPU and migration guests), or a clone/restore whose root disk
// must live on the node where its snapshot resides (AnnotationRestoreNodeName,
// stamped before EnsureRootDiskClone runs). It returns "" for an unpinned guest,
// whose launcher node is not decided until the scheduler places the pod.
//
// Used to co-locate the rootclone Job with the launcher: the RWO root PVC is
// then populated on the launcher's node and never has to detach + reattach
// across nodes (a ~26s Multi-Attach delay, cluster-observed on node-pinned
// clones whose unpinned Job landed on a different node than the pinned launcher).
func launcherTargetNode(guest *swiftv1alpha1.SwiftGuest) string {
	if guest.Spec.NodeName != "" {
		return guest.Spec.NodeName
	}
	return guest.Annotations[AnnotationRestoreNodeName]
}

func (r *SwiftGuestReconciler) createCloneJob(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
	jobName, sourcePVC, clonePVC string,
	targetSize resource.Quantity,
) error {
	targetBytes := targetSize.Value()
	isBlock := rg.Storage.VolumeMode == "Block"

	var script string
	var volumeMounts []corev1.VolumeMount
	var volumeDevices []corev1.VolumeDevice

	if isBlock {
		// Block destination. qemu-img convert writes the raw image to
		// the block device atomically. sgdisk -e operates byte-level
		// through the block device's standard read/write interface,
		// which works natively on block devices (W9 scoping question
		// (c) — verified on cluster). No qemu-img resize: block devices
		// are pre-sized at the PVC's requested capacity by the storage
		// layer, so resize would be a no-op and including it would
		// invite future confusion about whether the resize step was
		// load-bearing.
		script = fmt.Sprintf(`set -e
echo "Cloning root disk from %s to %s (%d bytes, Block mode -> %s)"
apt-get update -qq && apt-get install -y -qq qemu-utils gdisk >/dev/null 2>&1
qemu-img convert -f raw -O raw /src/image.raw %s
sgdisk -e %s
sync
echo "Clone complete (Block mode)"`,
			sourcePVC, clonePVC, targetBytes, CloneJobBlockDevicePath,
			CloneJobBlockDevicePath, CloneJobBlockDevicePath)
		volumeMounts = []corev1.VolumeMount{
			{Name: "src", MountPath: "/src", ReadOnly: true},
		}
		volumeDevices = []corev1.VolumeDevice{
			{Name: "dst", DevicePath: CloneJobBlockDevicePath},
		}
	} else {
		// Filesystem destination — byte-identical to pre-W9 behaviour.
		// Reviewers: this branch is the regression contract; any change
		// that alters the rendered Job for Filesystem-mode guests is a
		// regression and the smoke test will catch it.
		script = fmt.Sprintf(`set -e
echo "Cloning root disk from %s to %s (%d bytes)"
cp /src/image.raw /dst/image.raw
apt-get update -qq && apt-get install -y -qq qemu-utils gdisk >/dev/null 2>&1
qemu-img resize -f raw /dst/image.raw %d
sgdisk -e /dst/image.raw
sync
echo "Clone complete: $(stat -c %%s /dst/image.raw) bytes"`,
			sourcePVC, clonePVC, targetBytes, targetBytes)
		volumeMounts = []corev1.VolumeMount{
			{Name: "src", MountPath: "/src", ReadOnly: true},
			{Name: "dst", MountPath: "/dst"},
		}
	}

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
						Name:          "clone",
						Image:         CloneJobImage,
						Command:       []string{"/bin/sh", "-c", script},
						VolumeMounts:  volumeMounts,
						VolumeDevices: volumeDevices,
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

	// Co-locate the clone Job with the launcher when its node is known, so the
	// RWO root PVC is populated on the launcher's node and doesn't bounce across
	// nodes (matches the launcher's kubernetes.io/hostname nodeSelector).
	if node := launcherTargetNode(guest); node != "" {
		job.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
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
