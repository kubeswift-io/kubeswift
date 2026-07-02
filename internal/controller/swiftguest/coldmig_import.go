// Cold / suspended-state migration (P4) — the import half. A FULL-STATE
// cloneFromSnapshot (the snapshot carries status.oci.disk, captured by the
// includeDisk capture-then-terminate flow) gets its root disk MATERIALIZED from
// that oci artifact — the source's frozen runtime disk — rather than cloned from
// the base SwiftImage. The materialized disk lands in a RestoreSeeded PVC, so
// EnsureRootDiskClone's copy path skips it, and the restore-receive launcher
// then CH --restore's the memory against it and resumes. See
// docs/design/oras-cold-migration.md.
package swiftguest

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// maybeRootDiskFromOCI handles the root disk for a FULL-STATE cloneFromSnapshot.
// Returns (handled, result, err): handled=false means it is NOT a full-state
// clone (no status.oci.disk) and EnsureRootDiskClone falls through to its normal
// image-clone paths. When handled, err is nil + result set once the disk is
// materialized and Bound; a non-nil err is the "requeue and retry" progress
// signal the caller already treats as transient.
func (r *SwiftGuestReconciler) maybeRootDiskFromOCI(
	ctx context.Context, guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest,
) (bool, *RootDiskCloneResult, error) {
	if guest.Spec.CloneFromSnapshot == nil {
		return false, nil, nil
	}
	var snap snapshotv1alpha1.SwiftSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.CloneFromSnapshot.SnapshotRef.Name, Namespace: guest.Namespace}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil, nil // prepareCloneFromSnapshot surfaces the missing snapshot
		}
		return true, nil, err
	}
	if snap.Status.OCI == nil || snap.Status.OCI.Disk == nil || snap.Status.OCI.Disk.ManifestDigest == "" {
		return false, nil, nil // not a full-state snapshot → normal clone path
	}
	if r.SnapshotORASImage == "" {
		return true, nil, fmt.Errorf("snapshot-oras image not configured for a full-state clone")
	}
	node := guest.Spec.CloneFromSnapshot.TargetNode // required for oci (validated in prepareCloneFromSnapshot)
	cloneName := RootDiskCloneName(guest.Name)
	targetSize := rg.RootDisk.Size
	if targetSize.IsZero() {
		targetSize = resource.MustParse("40Gi")
	}
	block := resolvedVolumeMode(rg) != nil && *resolvedVolumeMode(rg) == corev1.PersistentVolumeBlock

	// 1. Ensure the clone root PVC — RestoreSeeded so EnsureRootDiskClone's copy
	//    path skips it (line ~158) and hands it straight to the launcher.
	var pvc corev1.PersistentVolumeClaim
	pvcErr := r.Get(ctx, client.ObjectKey{Name: cloneName, Namespace: guest.Namespace}, &pvc)
	if apierrors.IsNotFound(pvcErr) {
		// Storage class from the source guest's root PVC (the source exists; this
		// matches its class when rg.Storage doesn't pin one — see
		// resolvedStorageClassName which dereferences the source PVC).
		var srcRoot corev1.PersistentVolumeClaim
		_ = r.Get(ctx, client.ObjectKey{Name: RootDiskCloneName(snap.Spec.GuestRef.Name), Namespace: guest.Namespace}, &srcRoot)
		p := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cloneName,
				Namespace: guest.Namespace,
				Labels: map[string]string{
					"swift.kubeswift.io/guest": guest.Name,
					"swift.kubeswift.io/role":  "root-disk",
					RestoreSeededLabel:         "true",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      resolvedAccessModes(rg),
				VolumeMode:       resolvedVolumeMode(rg),
				StorageClassName: resolvedStorageClassName(rg, &srcRoot),
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: targetSize},
				},
			},
		}
		p.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)}
		if err := r.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
			return true, nil, fmt.Errorf("create full-state clone PVC %s: %w", cloneName, err)
		}
		return true, nil, fmt.Errorf("full-state clone PVC %s created, waiting for the oci disk download", cloneName)
	}
	if pvcErr != nil {
		return true, nil, pvcErr
	}

	// 2. Ensure the node-pinned download Job that materializes the disk from oci.
	jobName := cloneName + "-oci-disk-dl"
	var job batchv1.Job
	jerr := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: guest.Namespace}, &job)
	if apierrors.IsNotFound(jerr) {
		j := buildRootDiskFromOCIJob(guest, &snap, r.SnapshotORASImage, node, jobName, cloneName, block)
		if err := controllerutil.SetControllerReference(guest, j, r.Scheme); err != nil {
			return true, nil, err
		}
		if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
			return true, nil, err
		}
		return true, nil, fmt.Errorf("materializing full-state clone disk from %s", snap.Status.OCI.Disk.Reference)
	}
	if jerr != nil {
		return true, nil, jerr
	}

	// 3. Job status.
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			if pvc.Status.Phase != corev1.ClaimBound {
				return true, nil, fmt.Errorf("full-state clone PVC %s not yet Bound", cloneName)
			}
			return true, &RootDiskCloneResult{PVCName: cloneName, NeedsGrowInit: false}, nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true, nil, fmt.Errorf("full-state clone disk download failed: %s", c.Message)
		}
	}
	return true, nil, fmt.Errorf("full-state clone disk download in progress")
}

// buildRootDiskFromOCIJob runs snapshot-oras --mode=download-image to fill the
// clone's root PVC from the snapshot's oci disk artifact (pinned by digest).
// Block PVCs attach via volumeDevices at DiskRootDevicePath; Filesystem PVCs
// mount at DisksRootPath and the disk is image.raw. Node-pinned so the PVC
// attaches on the clone's node. Runs as root to write the raw disk.
func buildRootDiskFromOCIJob(guest *swiftv1alpha1.SwiftGuest, snap *snapshotv1alpha1.SwiftSnapshot, image, node, jobName, pvcName string, block bool) *batchv1.Job {
	oci := snap.Spec.Backend.OCI
	diskPath := DisksRootPath + "/image.raw"
	if block {
		diskPath = DiskRootDevicePath
	}
	args := []string{
		"--mode=download-image",
		"--file=" + diskPath,
		"--repository=" + oci.Repository,
		"--digest=" + snap.Status.OCI.Disk.ManifestDigest,
	}
	if oci.Insecure {
		args = append(args, "--insecure")
	}

	container := corev1.Container{
		Name:  "download",
		Image: image,
		Args:  args,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			RunAsUser:                ptr.To(int64(0)),
			RunAsNonRoot:             ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	vol := corev1.Volume{
		Name:         "rootdisk",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName}},
	}
	if block {
		container.VolumeDevices = []corev1.VolumeDevice{{Name: "rootdisk", DevicePath: DiskRootDevicePath}}
	} else {
		container.VolumeMounts = []corev1.VolumeMount{{Name: "rootdisk", MountPath: DisksRootPath}}
	}
	volumes := []corev1.Volume{vol}

	if oci.CredentialsSecretRef != nil && oci.CredentialsSecretRef.Name != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/oras-auth"})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "oras-auth", MountPath: "/oras-auth", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "oras-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: oci.CredentialsSecretRef.Name,
					Items:      []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
				},
			},
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: guest.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kubeswift",
				"app.kubernetes.io/component": "coldmig-disk-download",
				"swift.kubeswift.io/guest":    guest.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(4)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      node,
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
					Volumes:       volumes,
				},
			},
		},
	}
}
