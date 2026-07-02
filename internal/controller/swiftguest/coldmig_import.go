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
	"github.com/kubeswift-io/kubeswift/internal/runtimeintent"
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
		j := buildDiskFromOCIJob(guest, &snap, r.SnapshotORASImage, node, jobName, cloneName, snap.Status.OCI.Disk.ManifestDigest, block)
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
			// Root disk is materialized. A full-state snapshot may also carry
			// secondary data disks (v1.1); materialize + attach them under the
			// captured names and OVERRIDE rg.DataDisks so the launcher wires the
			// clone-owned PVCs — not the source's. Only report the root disk done
			// once every data disk is ready (an err here is the transient requeue
			// signal the caller already treats as progress).
			if err := r.ensureCloneDataDisks(ctx, guest, &snap, rg, node); err != nil {
				return true, nil, err
			}
			return true, &RootDiskCloneResult{PVCName: cloneName, NeedsGrowInit: false}, nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true, nil, fmt.Errorf("full-state clone disk download failed: %s", c.Message)
		}
	}
	return true, nil, fmt.Errorf("full-state clone disk download in progress")
}

// buildDiskFromOCIJob runs snapshot-oras --mode=download-image to fill a clone
// disk PVC from an oci disk artifact (pinned by the given manifest digest — the
// root disk's or a data disk's). A single PVC attaches inside the Job pod, so
// the same in-pod path (DiskRootDevicePath for Block, DisksRootPath/image.raw
// for Filesystem) serves any disk; only the PVC + digest differ. Node-pinned so
// the PVC attaches on the clone's node. Runs as root to write the raw disk.
func buildDiskFromOCIJob(guest *swiftv1alpha1.SwiftGuest, snap *snapshotv1alpha1.SwiftSnapshot, image, node, jobName, pvcName, digest string, block bool) *batchv1.Job {
	oci := snap.Spec.Backend.OCI
	diskPath := DisksRootPath + "/image.raw"
	if block {
		diskPath = DiskRootDevicePath
	}
	args := []string{
		"--mode=download-image",
		"--file=" + diskPath,
		"--repository=" + oci.Repository,
		"--digest=" + digest,
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

// ensureCloneDataDisks materializes a full-state clone's secondary data disks
// (v1.1) from the snapshot's per-disk oci artifacts (status.oci.dataDisks) and
// OVERRIDES rg.DataDisks so the launcher attaches the clone-owned PVCs — not the
// source's (a same-source clone would otherwise resolve to the SOURCE's data-disk
// PVCs, which are frozen/RWO-conflicting). Each captured disk becomes a
// RestoreSeeded clone-owned PVC filled by a node-pinned download Job under the
// SAME disk name (so CH's name-derived --disk path coheres with the restored
// config.json). Returns nil only when every data disk is Bound + its Job Complete
// and rg.DataDisks has been set; any other return is the transient requeue signal.
//
// Only blank / attachAsDisk disks are ever captured (the capture side rejects
// image-backed data disks), so materializing them fresh is always correct.
func (r *SwiftGuestReconciler) ensureCloneDataDisks(
	ctx context.Context, guest *swiftv1alpha1.SwiftGuest, snap *snapshotv1alpha1.SwiftSnapshot,
	rg *resolved.ResolvedGuest, node string,
) error {
	if snap.Status.OCI == nil || len(snap.Status.OCI.DataDisks) == 0 {
		return nil // root-disk-only full-state snapshot (v1) — nothing to do.
	}
	// Captured shape (size/block) keyed by name.
	shape := map[string]snapshotv1alpha1.CapturedDataDisk{}
	if snap.Status.GuestSpec != nil {
		for _, cd := range snap.Status.GuestSpec.DataDisks {
			shape[cd.Name] = cd
		}
	}

	// Pass 1: ensure every clone-owned PVC + its download Job exists. Creating
	// all up-front lets the per-disk downloads run concurrently.
	allCreated := true
	for _, art := range snap.Status.OCI.DataDisks {
		cd := shape[art.Name]
		pvcName := resolved.BlankDataDiskPVCName(guest.Name, art.Name)

		var pvc corev1.PersistentVolumeClaim
		perr := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: guest.Namespace}, &pvc)
		if apierrors.IsNotFound(perr) {
			size := resource.MustParse("1Gi")
			if cd.Size != "" {
				if q, err := resource.ParseQuantity(cd.Size); err == nil {
					size = q
				}
			}
			mode := corev1.PersistentVolumeFilesystem
			if cd.Block {
				mode = corev1.PersistentVolumeBlock
			}
			p := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: guest.Namespace,
					Labels: map[string]string{
						"swift.kubeswift.io/guest": guest.Name,
						"swift.kubeswift.io/role":  "data-disk",
						RestoreSeededLabel:         "true",
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(guest, swiftGuestGVK)},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					VolumeMode:  &mode,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: size},
					},
				},
			}
			if err := r.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("create clone data-disk PVC %s: %w", pvcName, err)
			}
			allCreated = false
			continue
		}
		if perr != nil {
			return perr
		}

		jobName := pvcName + "-oci-dl"
		var job batchv1.Job
		jerr := r.Get(ctx, client.ObjectKey{Name: jobName, Namespace: guest.Namespace}, &job)
		if apierrors.IsNotFound(jerr) {
			j := buildDiskFromOCIJob(guest, snap, r.SnapshotORASImage, node, jobName, pvcName, art.ManifestDigest, cd.Block)
			if err := controllerutil.SetControllerReference(guest, j, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, j); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			allCreated = false
			continue
		}
		if jerr != nil {
			return jerr
		}
	}
	if !allCreated {
		return fmt.Errorf("materializing %d data disk(s) from oci", len(snap.Status.OCI.DataDisks))
	}

	// Pass 2: gate on all Bound + Complete, then build the override.
	var disks []resolved.ResolvedDataDisk
	for _, art := range snap.Status.OCI.DataDisks {
		cd := shape[art.Name]
		pvcName := resolved.BlankDataDiskPVCName(guest.Name, art.Name)

		var pvc corev1.PersistentVolumeClaim
		if err := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: guest.Namespace}, &pvc); err != nil {
			return err
		}
		var job batchv1.Job
		if err := r.Get(ctx, client.ObjectKey{Name: pvcName + "-oci-dl", Namespace: guest.Namespace}, &job); err != nil {
			return err
		}
		done := false
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return fmt.Errorf("clone data-disk %s download failed: %s", art.Name, c.Message)
			}
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				done = true
			}
		}
		if !done || pvc.Status.Phase != corev1.ClaimBound {
			return fmt.Errorf("clone data-disk %s not yet ready", art.Name)
		}

		rd := resolved.ResolvedDataDisk{
			Name:    art.Name,
			PVCName: pvcName,
			Block:   cd.Block,
			Format:  "raw",
			Ready:   true,
		}
		if cd.Block {
			rd.HostPath = runtimeintent.DataDiskDevicePath(art.Name)
		} else {
			rd.MountPath = runtimeintent.DataDiskDir(art.Name)
			rd.HostPath = rd.MountPath + "/" + runtimeintent.DataDiskImageFile
		}
		disks = append(disks, rd)
	}
	rg.DataDisks = disks
	return nil
}
