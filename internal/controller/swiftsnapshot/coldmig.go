// Cold / suspended-state migration (P4) — the disk half of an includeDisk oci
// snapshot. A full-state snapshot pairs the memory artifact (the normal oci
// capture) with a chunked artifact of the guest's disk, so the pair can resume
// in another cluster. Coherence is capture-then-terminate: the guest is paused
// and memory-snapshotted with ResumeAfterSnapshot=false (set in
// handlePendingLocal), so it stays paused; this file then TERMINATES the launcher
// to release the RWO root PVC (frozen at the snapshot instant) and chunks the
// released disk to the registry via snapshot-oras --mode=upload-image (P3).
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
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
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
// handleUploadingOCI preserves it. Sequence: STOP the source guest → terminate the
// (paused) launcher to release the root PVC → chunk the released disk to oci →
// stamp status.OCI.Disk. Returns (done, errMsg, err); done=false means requeue.
func (r *SwiftSnapshotReconciler) handleFullStateDiskCapture(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, status *snapshotv1alpha1.SwiftSnapshotStatus) (bool, string, error) {
	if r.SnapshotORASImage == "" {
		return false, "snapshot-oras image not configured (set " + SnapshotORASImageEnv + ")", nil
	}
	// 0. Stop the source guest FIRST — flip runPolicy=Stopped so the SwiftGuest
	//    controller does NOT recreate the launcher after we delete it in step 1.
	//    The stop-guard is reactive: recreation happens while runPolicy=Running, so
	//    a bare Delete without the Stopped flip lets the launcher resurrect — a
	//    split-brain with the resumed clone AND a coherency race between the
	//    disk-chunk Job (reads image.raw ro) and the resurrected guest's CH
	//    (writes it rw). This is the "terminate the source" of capture-then-
	//    terminate (design §2.1/§5): a full-state capture is a migration, so the
	//    source stays down (the operator deletes it once the clone is up). Mirrors
	//    the Live Migration Phase 1 combined runPolicy=Stopped-before-Delete rule.
	if err := r.stopSourceGuest(ctx, snap.Namespace, snap.Spec.GuestRef.Name); err != nil {
		return false, "", err
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

	// 2. Launcher gone → ensure the node-pinned chunk Jobs: the root disk plus
	//    one per captured data disk (v1.1 — blank + attachAsDisk; every PVC is
	//    frozen at the snapshot instant and released by the termination above).
	type chunkTarget struct {
		dataName string // "" = the root disk
		jobName  string
		tag      string
		pvcName  string
		block    bool
	}
	rootBlock, berr := r.rootDiskIsBlock(ctx, snap)
	if berr != nil {
		return false, "", berr
	}
	targets := []chunkTarget{{
		jobName: diskChunkJobName(snap),
		tag:     ociDiskTag(snap),
		pvcName: rootDiskPVCName(snap.Spec.GuestRef.Name),
		block:   rootBlock,
	}}
	if status.GuestSpec != nil {
		for _, dd := range status.GuestSpec.DataDisks {
			targets = append(targets, chunkTarget{
				dataName: dd.Name,
				jobName:  diskChunkJobName(snap) + "-" + dd.Name,
				tag:      ociDiskTag(snap) + "-" + dd.Name,
				pvcName:  dd.PVCName,
				block:    dd.Block,
			})
		}
	}

	allComplete := true
	for _, t := range targets {
		var job batchv1.Job
		jerr := r.Get(ctx, client.ObjectKey{Name: t.jobName, Namespace: snap.Namespace}, &job)
		if apierrors.IsNotFound(jerr) {
			j := buildChunkJob(snap, r.SnapshotORASImage, status.NodeName, t.jobName, t.tag, t.pvcName, t.block)
			if serr := ctrl.SetControllerReference(snap, j, r.Scheme); serr != nil {
				return false, "", serr
			}
			if cerr := r.Create(ctx, j); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				return false, "", cerr
			}
			allComplete = false
			continue
		}
		if jerr != nil {
			return false, "", jerr
		}
		complete := false
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				complete = true
			}
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				disk := "root disk"
				if t.dataName != "" {
					disk = "data disk " + t.dataName
				}
				return false, "OCI chunk Job for the " + disk + " failed: " + c.Message, nil
			}
		}
		if !complete {
			allComplete = false
		}
	}
	if !allComplete {
		return false, "", nil // still chunking
	}

	// 3. All chunk Jobs Complete → stamp the artifacts atomically (the
	//    controller's Uploading guard keys on status.oci.disk, so nothing is
	//    stamped until every disk is in the registry).
	if status.OCI == nil {
		status.OCI = &snapshotv1alpha1.OCISnapshotStatus{}
	}
	status.OCI.Disk = &snapshotv1alpha1.OCIDiskArtifact{Reference: ociDiskReference(snap)}
	if rep, ok, rerr := clonecommon.JobTransferReport(ctx, r.Client, snap.Namespace, diskChunkJobName(snap)); rerr == nil && ok {
		status.OCI.Disk.ManifestDigest = rep.ManifestDigest
		status.OCI.Disk.PushedBytes = rep.TotalBytes
	}
	status.OCI.DataDisks = nil
	for _, t := range targets {
		if t.dataName == "" {
			continue
		}
		art := snapshotv1alpha1.OCIDataDiskArtifact{
			Name:      t.dataName,
			Reference: snap.Spec.Backend.OCI.Repository + ":" + t.tag,
		}
		if rep, ok, rerr := clonecommon.JobTransferReport(ctx, r.Client, snap.Namespace, t.jobName); rerr == nil && ok {
			art.ManifestDigest = rep.ManifestDigest
			art.PushedBytes = rep.TotalBytes
		}
		status.OCI.DataDisks = append(status.OCI.DataDisks, art)
	}
	return true, "", nil
}

// hasImageBackedDataDisk reports whether the guest has an image-backed VM data
// disk (the legacy singular dataDiskRef, or dataDiskRefs[].imageRef). Those
// attach the SwiftImage's SHARED prepared PVC, which a full-state capture
// cannot chunk safely (v1.1 design §5.5).
func hasImageBackedDataDisk(guest *swiftv1alpha1.SwiftGuest) bool {
	if guest.Spec.DataDiskRef != nil {
		return true
	}
	for i := range guest.Spec.DataDiskRefs {
		if guest.Spec.DataDiskRefs[i].ImageRef != nil {
			return true
		}
	}
	return false
}

// capturedDataDisks freezes the launcher-sufficient shape of the source's VM
// data disks (v1.1): name + Block + size + the source-cluster PVC the chunk Job
// mounts. Blank disks take their size from the guest spec; attachAsDisk disks
// from the operator PVC's storage request. Image-backed disks never reach here
// (rejected in handlePendingLocal).
func (r *SwiftSnapshotReconciler) capturedDataDisks(ctx context.Context, guest *swiftv1alpha1.SwiftGuest, rg *resolved.ResolvedGuest) ([]snapshotv1alpha1.CapturedDataDisk, error) {
	if len(rg.DataDisks) == 0 {
		return nil, nil
	}
	blankSize := map[string]string{}
	for i := range guest.Spec.DataDiskRefs {
		d := &guest.Spec.DataDiskRefs[i]
		if d.Blank != nil {
			blankSize[d.Name] = d.Blank.Size.String()
		}
	}
	out := make([]snapshotv1alpha1.CapturedDataDisk, 0, len(rg.DataDisks))
	for _, dd := range rg.DataDisks {
		c := snapshotv1alpha1.CapturedDataDisk{
			Name:    dd.Name,
			Block:   dd.Block,
			PVCName: dd.PVCName,
		}
		if s, ok := blankSize[dd.Name]; ok {
			c.Size = s
		} else {
			var pvc corev1.PersistentVolumeClaim
			if err := r.Get(ctx, client.ObjectKey{Name: dd.PVCName, Namespace: guest.Namespace}, &pvc); err != nil {
				return nil, err
			}
			if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				c.Size = q.String()
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// stopSourceGuest patches the source SwiftGuest to runPolicy=Stopped (idempotent)
// so the SwiftGuest controller's stop-guard does not recreate the launcher after
// handleFullStateDiskCapture deletes it. runPolicy must flip to Stopped BEFORE the
// Delete (the guard is reactive — it prevents recreation, it does not stop a
// running pod), so this is step 0. A source-gone (NotFound) or already-Stopped
// guest is a no-op — the capture is re-entrant across requeues. The clone still
// resolves the source spec later (prepareCloneFromSnapshot needs the guest to
// exist, not to be running); the operator deletes the stopped source once the
// clone is up.
func (r *SwiftSnapshotReconciler) stopSourceGuest(ctx context.Context, namespace, guestName string) error {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: guestName, Namespace: namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // source already gone → nothing to stop
		}
		return err
	}
	if guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyStopped {
		return nil // already stopped (idempotent re-entry)
	}
	patch := client.MergeFrom(guest.DeepCopy())
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	return r.Patch(ctx, &guest, patch)
}

// buildChunkJob constructs a node-pinned Job that chunks one released PVC (the
// root disk, or a v1.1 data disk) to the registry as tag. Block PVCs attach via
// volumeDevices at rootDiskDevicePath; Filesystem PVCs mount at diskChunkMount
// and the disk is image.raw (the paths are Job-internal — only --file must
// match). Pinned to the capture node so the RWO PVC re-attaches locally. Runs
// as root to read the raw disk. The caller sets the ownerRef.
func buildChunkJob(snap *snapshotv1alpha1.SwiftSnapshot, image, captureNode, jobName, tag, pvcName string, block bool) *batchv1.Job {
	oci := snap.Spec.Backend.OCI
	// Filesystem: the PVC mounts at diskChunkMount, the disk is image.raw.
	// Block: the raw device attaches at rootDiskDevicePath.
	diskPath := diskChunkMount + "/image.raw"
	if block {
		diskPath = rootDiskDevicePath
	}
	args := []string{
		"--mode=upload-image",
		"--file=" + diskPath,
		"--repository=" + oci.Repository,
		"--tag=" + tag,
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
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
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
			Name:      jobName,
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
