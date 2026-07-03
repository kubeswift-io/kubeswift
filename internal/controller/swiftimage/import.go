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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

const (
	importJobNamePrefix   = "swiftimage-import-"
	importPVCNamePrefix   = "swiftimage-import-"
	importVolumeMountPath = "/data"
	importOutputFile      = "image.raw"
	importSourceFile      = "source.img"
)

// importScript returns a shell script that downloads and converts the image.
// When sourceFormat is qcow2, converts to raw (CH does not support qcow2 compressed blocks).
// The raw image stays at the original cloud image size (compact template).
// Per-guest disk sizing happens during the root disk clone step in the SwiftGuest controller.
// Patches GRUB to add console=ttyS0 for serial console (firmware boot uses disk's cmdline).
// Supports Ubuntu, Debian, Fedora, Rocky Linux, and common layouts.
//
// osType gates the Linux-only GRUB/serial patch: "windows" skips it entirely
// (Windows has no GRUB — it boots bootmgfw from the ESP and manages serial via
// EMS/SAC in the operator-prepped image), keeping only the OS-agnostic
// qcow2->raw convert + size measurement. (Windows guest support — PR 3.)
func importScript(sourceURL, sourceFormat, osType string) string {
	base := importVolumeMountPath
	source := base + "/" + importSourceFile
	output := base + "/" + importOutputFile
	grubPatch := grubPatchBlock(osType)
	if sourceFormat == "qcow2" {
		return fmt.Sprintf("set -e\nOUTPUT=%q\napt-get update -qq && apt-get install -y -qq curl qemu-utils util-linux >/dev/null\ncurl -fsSL -o %q %q\nqemu-img convert -f qcow2 -O raw %q \"$OUTPUT\"%s\nstat -c %%s \"$OUTPUT\" > \"$OUTPUT.size\"\necho \"Image size: $(cat $OUTPUT.size) bytes\"", output, source, sourceURL, source, grubPatch)
	}
	return fmt.Sprintf("set -e\nOUTPUT=%q\napt-get update -qq && apt-get install -y -qq curl util-linux >/dev/null\ncurl -fsSL -o \"$OUTPUT\" %q%s\nstat -c %%s \"$OUTPUT\" > \"$OUTPUT.size\"\necho \"Image size: $(cat $OUTPUT.size) bytes\"", output, sourceURL, grubPatch)
}

// grubPatchBlock is the shell that loop-mounts a Linux disk image and injects
// console=ttyS0 into GRUB for the Cloud Hypervisor serial console (it reads the
// disk from $OUTPUT). Shared by the http and oci import scripts. Empty for
// Windows (no GRUB — it boots bootmgfw and manages serial via EMS/SAC).
func grubPatchBlock(osType string) string {
	if osType == string(imagev1alpha1.OSTypeWindows) {
		return ""
	}
	return `
patch_grub() {
  local mnt="$1"
  for grub in $(find "$mnt" \( -name "grub.cfg" -o -name "grub.conf" \) 2>/dev/null); do
    if [ ! -f "$grub" ]; then continue; fi
    if ! grep -q 'console=ttyS0' "$grub"; then
      sed -i 's/\(linux[^ ]* .*\)/\1 console=ttyS0,115200n8 earlycon=uart8250,io,0x3f8,115200n8/' "$grub"
      echo "Patched $grub for serial console"
    fi
    # Always enable GRUB serial terminal (required for Cloud Hypervisor serial socket)
    if grep -q '^terminal_output console' "$grub"; then
      awk '
        /^terminal_input console/ { print "serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1"; print "terminal_input serial console"; print "terminal_output serial console"; next }
        /^terminal_output console/ { next }
        { print }
      ' "$grub" > "$grub.tmp" && mv "$grub.tmp" "$grub"
      echo "Patched $grub GRUB terminal for serial output"
    fi
  done
}
mkdir -p /mnt/disk

# Read GPT partition start LBAs using od (128 bytes per entry, start LBA at offset 32 in each entry).
# LBA 2 = byte 1024. We read up to 32 partition entries (32 * 128 = 4096 bytes).
# od: -A d (decimal addresses), -t u8 (unsigned 8-byte integers), skip 1024 bytes, read 4096 bytes.
GPT_OFFSETS=$(od -A d -t u8 -j 1024 -N 4096 "$OUTPUT" 2>/dev/null | awk '
  {
    addr = $1 + 0
    for (i = 2; i <= NF; i++) {
      byte_pos = addr + (i-2)*8
      rel = byte_pos - 1024
      if (rel >= 0 && rel % 128 == 32 && $i+0 == $i && $i > 2047 && $i < 1000000000) {
        print $i * 512
      }
    }
  }
' | sort -un)

# Also always try the known Ubuntu Noble offsets as fallback (covers MBR Linux/Linux LVM, GPT EFI+root)
FALLBACK_OFFSETS="1048576 116391936 5242880 104857600 140509184 536870912 1159725056"

for offset in $GPT_OFFSETS $FALLBACK_OFFSETS; do
  [ -z "$offset" ] || [ "$offset" = "0" ] && continue
  if mount -o loop,offset=$offset "$OUTPUT" /mnt/disk 2>/dev/null; then
    patch_grub /mnt/disk
    umount /mnt/disk
  fi
done
rmdir /mnt/disk 2>/dev/null || true
`
}

// importScriptOCI is the main-container script for a source.oci import. The
// puller init container has already materialized the disk at $OUTPUT
// (/data/image.raw); this skips the download and runs the same tail as the http
// path: convert-if-qcow2 (in place), size measurement, and the Linux GRUB/serial
// patch. Golden images are expected raw (raw-at-rest; §2.1) so the qcow2 branch
// is the rare escape hatch.
func importScriptOCI(sourceFormat, osType string) string {
	output := importVolumeMountPath + "/" + importOutputFile
	grubPatch := grubPatchBlock(osType)
	if sourceFormat == "qcow2" {
		return fmt.Sprintf("set -e\nOUTPUT=%q\napt-get update -qq && apt-get install -y -qq qemu-utils util-linux >/dev/null\nqemu-img convert -f qcow2 -O raw \"$OUTPUT\" \"$OUTPUT.tmp\"\nmv \"$OUTPUT.tmp\" \"$OUTPUT\"%s\nstat -c %%s \"$OUTPUT\" > \"$OUTPUT.size\"\necho \"Image size: $(cat $OUTPUT.size) bytes\"", output, grubPatch)
	}
	return fmt.Sprintf("set -e\nOUTPUT=%q\napt-get update -qq && apt-get install -y -qq util-linux >/dev/null%s\nstat -c %%s \"$OUTPUT\" > \"$OUTPUT.size\"\necho \"Image size: $(cat $OUTPUT.size) bytes\"", output, grubPatch)
}

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
	case src.OCI != nil:
		return r.importOCI(ctx, img)
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

	// PVC size: from spec.rootDisk.size if set, else default 10Gi
	storageReq := resource.MustParse("10Gi")
	if img.Spec.RootDisk != nil && img.Spec.RootDisk.Size != nil && !img.Spec.RootDisk.Size.IsZero() {
		storageReq = *img.Spec.RootDisk.Size
	}

	// Create PVC if not exists
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: img.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageReq},
			},
		},
	}
	if err := controllerutil.SetControllerReference(img, pvc, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	// Import job: download and convert qcow2→raw (compact template).
	// CH does not support qcow2 compressed blocks; runtime format is always raw.
	// Per-guest disk sizing happens during the root disk clone step.
	sourceURL := img.Spec.Source.HTTP.URL
	sourceFormat := string(img.Spec.Format)
	script := importScript(sourceURL, sourceFormat, string(img.Spec.OSType))
	// The import Job needs privileged ONLY for the loop-mount the Linux GRUB
	// serial-console patch performs. Windows skips that patch (no GRUB), so its
	// import runs unprivileged (Design Principle: no privileged unless required).
	// RunAsUser 0 is still needed either way for apt-get in the ubuntu image.
	privileged := img.Spec.OSType != imagev1alpha1.OSTypeWindows
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: img.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						// Root is needed for apt-get (and, on Linux, the loop-mount).
						RunAsUser: ptr.To(int64(0)),
					},
					Containers: []corev1.Container{{
						Name:    "import",
						Image:   "ubuntu:22.04",
						Command: []string{"sh", "-c", script},
						SecurityContext: &corev1.SecurityContext{
							// Privileged only for the Linux GRUB-patch loop-mount;
							// false for Windows imports (no patch, no mount).
							Privileged: ptr.To(privileged),
						},
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

// importOCI creates the import PVC and a Job whose puller init container
// (snapshot-oras --mode=download-image) reassembles the golden raw disk into the
// PVC, then the main container runs the shared resize/patch tail. Same PVC + Job
// name as importHTTP, so CheckImportStatus/Validate/Prepare are unchanged.
func (r *SwiftImageReconciler) importOCI(ctx context.Context, img *imagev1alpha1.SwiftImage) (*ImportResult, error) {
	if r.SnapshotORASImage == "" {
		return &ImportResult{Phase: imagev1alpha1.SwiftImagePhaseFailed, Error: "snapshot-oras image not configured for oci source"}, nil
	}
	pvcName := importPVCNamePrefix + img.Name
	jobName := importJobNamePrefix + img.Name

	storageReq := resource.MustParse("10Gi")
	if img.Spec.RootDisk != nil && img.Spec.RootDisk.Size != nil && !img.Spec.RootDisk.Size.IsZero() {
		storageReq = *img.Spec.RootDisk.Size
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: img.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storageReq},
			},
		},
	}
	if err := controllerutil.SetControllerReference(img, pvc, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return nil, err
	}

	oci := img.Spec.Source.OCI
	output := importVolumeMountPath + "/" + importOutputFile
	pullArgs := []string{
		"--mode=download-image",
		"--repository=" + oci.Repository,
		"--file=" + output,
	}
	if oci.Digest != "" {
		pullArgs = append(pullArgs, "--digest="+oci.Digest)
	} else {
		pullArgs = append(pullArgs, "--tag="+oci.Tag)
	}
	if oci.Insecure {
		pullArgs = append(pullArgs, "--insecure")
	}

	dataMount := corev1.VolumeMount{Name: "data", MountPath: importVolumeMountPath}
	volumes := []corev1.Volume{{
		Name: "data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
		},
	}}
	pullMounts := []corev1.VolumeMount{dataMount}
	var pullEnv []corev1.EnvVar
	if oci.CredentialsSecretRef != nil && oci.CredentialsSecretRef.Name != "" {
		pullEnv = append(pullEnv, corev1.EnvVar{Name: "DOCKER_CONFIG", Value: "/oras-auth"})
		pullMounts = append(pullMounts, corev1.VolumeMount{Name: "oras-auth", MountPath: "/oras-auth", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{
			Name: "oras-auth",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: oci.CredentialsSecretRef.Name,
					// A kubernetes.io/dockerconfigjson Secret stores the auth under
					// .dockerconfigjson; oras-go expects a Docker config.json.
					Items: []corev1.KeyToPath{{Key: ".dockerconfigjson", Path: "config.json"}},
				},
			},
		})
	}
	if oci.VerifyKeySecretRef != nil && oci.VerifyKeySecretRef.Name != "" {
		// cosign-verify the artifact before pulling. The public key is mounted
		// read-only at /verify-key/cosign.pub; cosign needs a writable HOME/TMPDIR
		// (the puller runs with a read-only rootfs), served by an emptyDir.
		pullArgs = append(pullArgs, "--verify-key=/verify-key/cosign.pub")
		pullEnv = append(pullEnv,
			corev1.EnvVar{Name: "HOME", Value: "/cosign-home"},
			corev1.EnvVar{Name: "TMPDIR", Value: "/cosign-home"},
		)
		pullMounts = append(pullMounts,
			corev1.VolumeMount{Name: "verify-key", MountPath: "/verify-key", ReadOnly: true},
			corev1.VolumeMount{Name: "cosign-home", MountPath: "/cosign-home"},
		)
		volumes = append(volumes,
			corev1.Volume{
				Name: "verify-key",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: oci.VerifyKeySecretRef.Name,
						Items:      []corev1.KeyToPath{{Key: "cosign.pub", Path: "cosign.pub"}},
					},
				},
			},
			corev1.Volume{Name: "cosign-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		)
	}

	script := importScriptOCI(string(img.Spec.Format), string(img.Spec.OSType))
	// Privileged only for the Linux GRUB-patch loop-mount (Windows skips it).
	privileged := img.Spec.OSType != imagev1alpha1.OSTypeWindows
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: img.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						// Root: the init container writes into the (root-owned) PVC
						// mount; the main container runs apt-get (+ Linux loop-mount).
						RunAsUser: ptr.To(int64(0)),
					},
					InitContainers: []corev1.Container{{
						Name:         "pull",
						Image:        r.SnapshotORASImage,
						Args:         pullArgs,
						Env:          pullEnv,
						VolumeMounts: pullMounts,
						// Minimal: writes only to the mounted PVC; no disk temp
						// (oras streams chunks into the sparse file).
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Containers: []corev1.Container{{
						Name:    "import",
						Image:   "ubuntu:22.04",
						Command: []string{"sh", "-c", script},
						SecurityContext: &corev1.SecurityContext{
							Privileged: ptr.To(privileged),
						},
						VolumeMounts: []corev1.VolumeMount{dataMount},
					}},
					Volumes: volumes,
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
