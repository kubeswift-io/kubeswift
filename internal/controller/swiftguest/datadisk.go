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

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
)

// blankFillJobName is the fill Job for a Filesystem-mode blank data disk.
func blankFillJobName(guestName, diskName string) string {
	return names.JobName(guestName, "-datafill-"+diskName)
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
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					ImagePullSecrets:             LauncherImagePullSecrets(),
					Containers: []corev1.Container{{
						Name:            "fill",
						Image:           CloneJobImage(),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/bin/sh", "-c", script},
						VolumeMounts:    []corev1.VolumeMount{{Name: "dst", MountPath: "/dst"}},
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

func imageDataDiskJobName(guestName, diskName string) string {
	return names.JobName(guestName, "-dataclone-"+diskName)
}

// EnsureImageDataDisks gives each image-backed data disk (dataDiskRef,
// dataDiskRefs[].imageRef) the guest's own copy: a guest-owned Block PVC,
// filled once from the SwiftImage's prepared PVC by a Job. The pod is gated
// on it like a blank data disk. The image's PVC is only read, by the Job.
//
// A guest whose launcher pod still attaches the image's PVC directly (created
// before this change) is left as it is: copying an image a running guest
// writes would capture a torn disk. It gets its copy, taken from the image at
// rest, when its launcher pod is next recreated.
func (r *SwiftGuestReconciler) EnsureImageDataDisks(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	rg *resolved.ResolvedGuest,
) error {
	var legacy *corev1.Pod
	for i := range rg.DataDisks {
		d := &rg.DataDisks[i]
		if d.SourcePVC == "" {
			continue
		}
		if legacy == nil {
			pod, err := LauncherPod(ctx, r.Client, guest)
			if err != nil {
				return err
			}
			legacy = &corev1.Pod{}
			if pod != nil {
				legacy = pod
			}
		}
		if podMountsPVC(legacy, d.SourcePVC) {
			continue
		}
		if err := r.ensureImageDataDisk(ctx, guest, d); err != nil {
			return err
		}
	}
	return nil
}

func podMountsPVC(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

func (r *SwiftGuestReconciler) ensureImageDataDisk(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	d *resolved.ResolvedDataDisk,
) error {
	if !d.Ready {
		return fmt.Errorf("data disk %s: SwiftImage not Ready", d.Name)
	}
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: d.PVCName, Namespace: guest.Namespace}, &pvc)
	switch {
	case errors.IsNotFound(err):
		size, err := r.imageDataDiskSize(ctx, guest.Namespace, d)
		if err != nil {
			return err
		}
		block := corev1.PersistentVolumeBlock
		newPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      d.PVCName,
				Namespace: guest.Namespace,
				Labels: map[string]string{
					"swift.kubeswift.io/guest": guest.Name,
					"swift.kubeswift.io/role":  "data-disk",
				},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				VolumeMode:  &block,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: size},
				},
			},
		}
		if err := r.Create(ctx, newPVC); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create data-disk PVC %s: %w", d.PVCName, err)
		}
	case err != nil:
		return err
	case !metav1.IsControlledBy(&pvc, guest):
		return fmt.Errorf("data-disk PVC %s exists but is not owned by guest %s", d.PVCName, guest.Name)
	}

	jobName := imageDataDiskJobName(guest.Name, d.Name)
	var job batchv1.Job
	err = r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: guest.Namespace}, &job)
	switch {
	case errors.IsNotFound(err):
		if err := r.createImageDataDiskJob(ctx, guest, d, jobName); err != nil {
			return err
		}
		return fmt.Errorf("data disk %s: copy Job %s created, waiting for completion", d.Name, jobName)
	case err != nil:
		return err
	}
	if isJobComplete(&job) {
		return nil
	}
	if isJobFailed(&job) {
		return fmt.Errorf("data disk %s: copy Job %s failed (delete the Job to retry)", d.Name, jobName)
	}
	return fmt.Errorf("data disk %s: copy Job %s in progress", d.Name, jobName)
}

// imageDataDiskSize is the copy's PVC size: the image size rounded up to a
// MiB, or the image PVC's request when the image size is not recorded.
func (r *SwiftGuestReconciler) imageDataDiskSize(ctx context.Context, ns string, d *resolved.ResolvedDataDisk) (resource.Quantity, error) {
	const mib = int64(1) << 20
	if d.SourceSize > 0 {
		return *resource.NewQuantity((d.SourceSize+mib-1)/mib*mib, resource.BinarySI), nil
	}
	var src corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: d.SourcePVC, Namespace: ns}, &src); err != nil {
		return resource.Quantity{}, fmt.Errorf("data disk %s: read image PVC %s: %w", d.Name, d.SourcePVC, err)
	}
	return src.Spec.Resources.Requests[corev1.ResourceStorage], nil
}

func (r *SwiftGuestReconciler) createImageDataDiskJob(
	ctx context.Context,
	guest *swiftv1alpha1.SwiftGuest,
	d *resolved.ResolvedDataDisk,
	jobName string,
) error {
	script := fmt.Sprintf(`set -e
echo "Copying data disk %s from image PVC %s"
qemu-img convert -f raw -O raw -n /src/%s /dev/dst
sync
echo "Copy complete"`, d.Name, d.SourcePVC, runtimeintent.DataDiskImageFile)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"swift.kubeswift.io/guest": guest.Name,
				"swift.kubeswift.io/role":  "data-disk-copy",
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					ImagePullSecrets:             LauncherImagePullSecrets(),
					Containers: []corev1.Container{{
						Name:            "copy",
						Image:           CloneJobImage(),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/bin/sh", "-c", script},
						VolumeMounts:    []corev1.VolumeMount{{Name: "src", MountPath: "/src", ReadOnly: true}},
						VolumeDevices:   []corev1.VolumeDevice{{Name: "dst", DevicePath: "/dev/dst"}},
					}},
					Volumes: []corev1.Volume{
						{Name: "src", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: d.SourcePVC, ReadOnly: true}}},
						{Name: "dst", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: d.PVCName}}},
					},
				},
			},
		},
	}
	if node := launcherTargetNode(guest); node != "" {
		job.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
	}
	if err := r.Create(ctx, job); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create data-disk copy Job: %w", err)
	}
	return nil
}
