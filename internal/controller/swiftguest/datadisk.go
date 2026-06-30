package swiftguest

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// blankFillJobName is the fill Job for a Filesystem-mode blank data disk.
func blankFillJobName(guestName, diskName string) string {
	return guestName + "-datafill-" + diskName
}

// EnsureBlankDataDisks provisions and gates the guest-owned PVCs backing the
// guest's blank data disks (spec.dataDiskRefs[].blank). It mirrors
// EnsureRootDiskClone's contract: it returns a non-nil error (the reconcile
// requeues) until every blank disk is ready, and nil once they all are.
//
//   - Block (the default): create a guest-owned Block PVC of the requested
//     size; ready once Bound. Nothing is written — the guest partitions and
//     formats the raw device.
//   - Filesystem (escape hatch): create a guest-owned Filesystem PVC, then run
//     a one-shot fill Job that truncates an empty image.raw of the requested
//     size into it (the same Filesystem-mount + image.raw runtime path the
//     image-backed data disk already uses); ready once the Job completes.
//
// image-backed and attached-PVC data disks reference PVCs that already exist
// (the SwiftImage's prepared PVC, or the operator's PVCRef) and are NOT handled
// here.
func (r *SwiftGuestReconciler) EnsureBlankDataDisks(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
) error {
	for i := range guest.Spec.DataDiskRefs {
		d := &guest.Spec.DataDiskRefs[i]
		if d.Blank == nil {
			continue
		}
		if err := r.ensureBlankDataDisk(ctx, guest, d); err != nil {
			return err
		}
	}
	return nil
}

func (r *SwiftGuestReconciler) ensureBlankDataDisk(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	d *swiftv1alpha1.DataDiskRef,
) error {
	pvcName := resolved.BlankDataDiskPVCName(guest.Name, d.Name)
	volumeMode := corev1.PersistentVolumeBlock
	if d.Blank.VolumeMode == corev1.PersistentVolumeFilesystem {
		volumeMode = corev1.PersistentVolumeFilesystem
	}

	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: guest.Namespace}, &pvc)
	switch {
	case errors.IsNotFound(err):
		newPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: guest.Namespace,
				Labels: map[string]string{
					"swift.kubeswift.io/guest": guest.Name,
					"swift.kubeswift.io/role":  "data-disk",
				},
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(guest, swiftGuestGVK),
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeMode:       &volumeMode,
				StorageClassName: d.Blank.StorageClassName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: d.Blank.Size},
				},
			},
		}
		if err := r.Create(ctx, newPVC); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create blank data-disk PVC %s: %w", pvcName, err)
		}
		return fmt.Errorf("blank data-disk PVC %s created, waiting for Bound", pvcName)
	case err != nil:
		return err
	}

	// A same-named PVC that this guest does not own is unexpected and may hold
	// unrelated data — never delete it (unlike the root-disk clone, blank disks
	// are operator data). Surface honestly and stop.
	if !metav1.IsControlledBy(&pvc, guest) {
		return fmt.Errorf("blank data-disk PVC %s exists but is not owned by guest %s", pvcName, guest.Name)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return fmt.Errorf("blank data-disk PVC %s not yet Bound (phase=%s)", pvcName, pvc.Status.Phase)
	}

	// Block disks are ready once Bound. Filesystem disks need image.raw.
	if volumeMode != corev1.PersistentVolumeFilesystem {
		return nil
	}
	return r.ensureBlankFillJob(ctx, guest, d, pvcName)
}

// ensureBlankFillJob runs (and gates on) a one-shot Job that truncates an
// empty image.raw of the requested size into a Filesystem-mode blank PVC.
func (r *SwiftGuestReconciler) ensureBlankFillJob(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	d *swiftv1alpha1.DataDiskRef,
	pvcName string,
) error {
	jobName := blankFillJobName(guest.Name, d.Name)

	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: guest.Namespace}, &job)
	switch {
	case errors.IsNotFound(err):
		if err := r.createBlankFillJob(ctx, guest, d, jobName, pvcName); err != nil {
			return err
		}
		return fmt.Errorf("blank data-disk fill Job %s created, waiting for completion", jobName)
	case err != nil:
		return err
	}
	if isJobComplete(&job) {
		return nil
	}
	if isJobFailed(&job) {
		return fmt.Errorf("blank data-disk fill Job %s failed", jobName)
	}
	return fmt.Errorf("blank data-disk fill Job %s in progress", jobName)
}

func (r *SwiftGuestReconciler) createBlankFillJob(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	d *swiftv1alpha1.DataDiskRef,
	jobName, pvcName string,
) error {
	bytes := d.Blank.Size.Value()
	// truncate creates a sparse file of the exact requested size; CH attaches
	// it as a raw disk and the guest formats it. No package install needed.
	script := fmt.Sprintf(`set -e
echo "Filling blank data disk %s (%d bytes)"
truncate -s %d /dst/image.raw
sync
echo "Fill complete: $(stat -c %%s /dst/image.raw) bytes"`, d.Name, bytes, bytes)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
				"swift.kubeswift.io/role":  "data-disk-fill",
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
						Name:         "fill",
						Image:        CloneJobImage,
						Command:      []string{"/bin/sh", "-c", script},
						VolumeMounts: []corev1.VolumeMount{{Name: "dst", MountPath: "/dst"}},
					}},
					Volumes: []corev1.Volume{{
						Name: "dst",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
						},
					}},
				},
			},
		},
	}
	// Co-locate the fill Job with the launcher (RWO PVC stays on the launcher's
	// node, no cross-node bounce) — same rationale as the root-disk clone Job.
	if node := launcherTargetNode(guest); node != "" {
		job.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	}

	if err := r.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create blank data-disk fill Job: %w", err)
	}
	return nil
}

// dataDiskStatuses builds the status.dataDisks echo for every resolved VM data
// disk (image-backed, blank, attached), reading each backing PVC's bind state.
func (r *SwiftGuestReconciler) dataDiskStatuses(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
) []swiftv1alpha1.DataDiskStatus {
	if len(rg.DataDisks) == 0 {
		return nil
	}
	out := make([]swiftv1alpha1.DataDiskStatus, 0, len(rg.DataDisks))
	for i := range rg.DataDisks {
		d := &rg.DataDisks[i]
		st := swiftv1alpha1.DataDiskStatus{
			Name:       d.Name,
			PVCName:    d.PVCName,
			VolumeMode: corev1.PersistentVolumeFilesystem,
		}
		if d.Block {
			st.VolumeMode = corev1.PersistentVolumeBlock
			st.DevicePath = d.HostPath
		}
		if d.PVCName != "" {
			var pvc corev1.PersistentVolumeClaim
			if err := r.Get(ctx, client.ObjectKey{Name: d.PVCName, Namespace: guest.Namespace}, &pvc); err == nil {
				st.Bound = pvc.Status.Phase == corev1.ClaimBound
			}
		}
		out = append(out, st)
	}
	return out
}
