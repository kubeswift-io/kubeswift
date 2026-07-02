// Cold / suspended-state migration (P4) — the disk half of an includeDisk oci
// snapshot. A full-state snapshot pairs the memory artifact (the normal oci
// capture) with a chunked artifact of the guest's disk, so the pair can resume
// in another cluster. Coherence is capture-then-terminate: the guest is paused
// and memory-snapshotted with ResumeAfterSnapshot=false (set in
// handlePendingLocal), so it stays paused; this file then TERMINATES the launcher
// to release the RWO root PVC (frozen at the snapshot instant) and chunks the
// released disk to the registry via snapshot-oras --mode=upload-image (P3).
// See docs/design/oras-cold-migration.md.
package swiftsnapshot

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/snapshot/clonecommon"
)

const (
	// Root-disk PVC name + in-launcher disk paths, mirrored from
	// internal/controller/swiftguest (a local copy avoids a controller import
	// cycle; these are stable on-disk contract paths).
	rootDiskPVCPrefix  = "swiftguest-root-"
	rootDiskDevicePath = "/dev/kubeswift-root" // Block root disk (raw device)
	// diskChunkMount is where the chunk Job mounts a Filesystem root PVC (the disk
	// is image.raw under it).
	diskChunkMount = "/rootdisk"
)

func rootDiskPVCName(guestName string) string { return rootDiskPVCPrefix + guestName }

// ociDiskTag is the disk artifact's tag — the memory tag + "-disk" so both
// artifacts share the repository but are distinct manifests.
func ociDiskTag(snap *snapshotv1alpha1.SwiftSnapshot) string { return ociTag(snap) + "-disk" }

func ociDiskReference(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return snap.Spec.Backend.OCI.Repository + ":" + ociDiskTag(snap)
}

func diskChunkJobName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	return snap.Name + "-oci-disk"
}

// rootDiskIsBlock reports whether the guest's root PVC is Block-mode (raw device)
// vs Filesystem (image.raw file). Defaults to Filesystem when the PVC or its
// volumeMode is absent.
func (r *SwiftSnapshotReconciler) rootDiskIsBlock(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (bool, error) {
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: rootDiskPVCName(snap.Spec.GuestRef.Name), Namespace: snap.Namespace}, &pvc)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock, nil
}

// handleFullStateDiskCapture is the capture-then-terminate disk half. It runs in
// the Uploading phase BEFORE the memory push, so status.OCI.Disk is set first and
// handleUploadingOCI preserves it. Sequence: terminate the (paused) launcher to
// release the root PVC → chunk the released disk to oci → stamp status.OCI.Disk.
// Returns (done, errMsg, err); done=false means requeue.
func (r *SwiftSnapshotReconciler) handleFullStateDiskCapture(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) (bool, string, error) {
	if r.SnapshotORASImage == "" {
		return false, "snapshot-oras image not configured (set " + SnapshotORASImageEnv + ")", nil
	}
	// 1. Terminate the launcher so the RWO root PVC releases (frozen at the
	//    snapshot instant — the VM was captured with ResumeAfterSnapshot=false and
	//    never resumed). Idempotent: re-Delete while it drains, requeue until gone.
	pod, err := r.findLauncherPod(ctx, snap.Namespace, snap.Spec.GuestRef.Name)
	if err != nil {
		return false, "", err
	}
	if pod != nil {
		grace := int64(0)
		if delErr := r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &grace}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return false, "", delErr
		}
		return false, "", nil // requeue: wait for the launcher to be gone (PVC released)
	}

	// 2. Launcher gone → ensure the node-pinned disk-chunk Job.
	var job batchv1.Job
	jerr := r.Get(ctx, client.ObjectKey{Name: diskChunkJobName(snap), Namespace: snap.Namespace}, &job)
	if apierrors.IsNotFound(jerr) {
		block, berr := r.rootDiskIsBlock(ctx, snap)
		if berr != nil {
			return false, "", berr
		}
		j := buildDiskChunkJob(snap, r.SnapshotORASImage, status.NodeName, block)
		if serr := ctrl.SetControllerReference(snap, j, r.Scheme); serr != nil {
			return false, "", serr
		}
		if cerr := r.Create(ctx, j); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
			return false, "", cerr
		}
		return false, "", nil
	}
	if jerr != nil {
		return false, "", jerr
	}

	// 3. Job status.
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			if status.OCI == nil {
				status.OCI = &snapshotv1alpha1.OCISnapshotStatus{}
			}
			status.OCI.Disk = &snapshotv1alpha1.OCIDiskArtifact{Reference: ociDiskReference(snap)}
			if rep, ok, rerr := clonecommon.JobTransferReport(ctx, r.Client, snap.Namespace, diskChunkJobName(snap)); rerr == nil && ok {
				status.OCI.Disk.ManifestDigest = rep.ManifestDigest
				status.OCI.Disk.PushedBytes = rep.TotalBytes
			}
			return true, "", nil
		}
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return false, "OCI disk chunk Job failed: " + c.Message, nil
		}
	}
	return false, "", nil // still chunking
}

// buildDiskChunkJob constructs the node-pinned Job that chunks the released root
// PVC to the registry. Block root disks attach via volumeDevices at
// rootDiskDevicePath; Filesystem root disks mount at diskChunkMount and the disk
// is image.raw. Pinned to the capture node (the disk's node) so the RWO PVC
// re-attaches locally. Runs as root to read the raw disk. The caller sets the
// ownerRef.
func buildDiskChunkJob(snap *snapshotv1alpha1.SwiftSnapshot, image, captureNode string, block bool) *batchv1.Job {
	oci := snap.Spec.Backend.OCI
	// Filesystem root: the PVC mounts at diskChunkMount, the disk is image.raw.
	// Block root: the raw device attaches at rootDiskDevicePath.
	diskPath := diskChunkMount + "/image.raw"
	if block {
		diskPath = rootDiskDevicePath
	}
	args := []string{
		"--mode=upload-image",
		"--file=" + diskPath,
		"--repository=" + oci.Repository,
		"--tag=" + ociDiskTag(snap),
	}
	if oci.Insecure {
		args = append(args, "--insecure")
	}

	container := corev1.Container{
		Name:  "chunk",
		Image: image,
		Args:  args,
		// Root to read the raw disk (device or 0644 image); otherwise maximally
		// constrained — upload-image streams chunks to the registry, no disk temp.
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			RunAsUser:                ptr.To(int64(0)),
			RunAsNonRoot:             ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
	rootVol := corev1.Volume{
		Name: "rootdisk",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: rootDiskPVCName(snap.Spec.GuestRef.Name)},
		},
	}
	if block {
		container.VolumeDevices = []corev1.VolumeDevice{{Name: "rootdisk", DevicePath: rootDiskDevicePath}}
	} else {
		container.VolumeMounts = []corev1.VolumeMount{{Name: "rootdisk", MountPath: diskChunkMount, ReadOnly: true}}
	}
	volumes := []corev1.Volume{rootVol}

	// Registry credentials (dockerconfigjson), mirroring buildOCIPushJob.
	if oci.CredentialsSecretRef != nil && oci.CredentialsSecretRef.Name != "" {
		container.Env = append(container.Env, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: ociAuthMount})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "oras-auth", MountPath: ociAuthMount, ReadOnly: true})
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
			Name:      diskChunkJobName(snap),
			Namespace: snap.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kubeswift",
				"app.kubernetes.io/component": "snapshot-oci-disk",
				"kubeswift.io/swiftsnapshot":  snap.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(ociPushBackoffLimit),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeName:      captureNode,
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
					Volumes:       volumes,
				},
			},
		},
	}
}
