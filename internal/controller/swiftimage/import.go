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

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
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
// After conversion, patches GRUB to add console=ttyS0 for serial console (firmware boot uses disk's cmdline).
// Supports Ubuntu, Debian, Fedora, Rocky Linux, and common layouts. Uses fdisk to discover
// Linux/LVM partitions first, then falls back to known offsets. Patches both grub.cfg and grub.conf.
func importScript(sourceURL, sourceFormat string) string {
	base := importVolumeMountPath
	source := base + "/" + importSourceFile
	output := base + "/" + importOutputFile
	grubPatch := `
# Patch GRUB for serial console (firmware boot uses disk cmdline, not CH --cmdline)
patch_grub() {
  local mnt="$1"
  for grub in $(find "$mnt" \( -name "grub.cfg" -o -name "grub.conf" \) 2>/dev/null); do
    if [ -f "$grub" ] && ! grep -q 'console=ttyS0' "$grub"; then
      sed -i 's/\(linux[^ ]* .*\)/\1 console=ttyS0,115200n8/' "$grub"
      echo "Patched $grub for serial console"
    fi
  done
}
mkdir -p /mnt/disk
# Try ESP (1MB) - UEFI: /EFI/ubuntu/grub.cfg, /EFI/BOOT/grub.cfg
if mount -o loop,offset=1048576 "$OUTPUT" /mnt/disk 2>/dev/null; then
  patch_grub /mnt/disk
  umount /mnt/disk
fi
# Discover Linux partition offsets via fdisk (sector * 512), then fallback to known offsets
# Ubuntu root: sector 227328. Debian: 134MB. Fedora: 512MB. Rocky/RHEL: 1106MiB (root), 100MiB (generic).
OFFSETS=$(fdisk -l "$OUTPUT" 2>/dev/null | awk '/Linux filesystem|Linux LVM/ {print $2*512}' | sort -u)
if [ -z "$OFFSETS" ]; then
  OFFSETS="116391936 140509184 1159725056 104857600 1048576 5242880 536870912 537919488"
fi
for offset in $OFFSETS; do
  [ -z "$offset" ] || [ "$offset" = "0" ] && continue
  if mount -o loop,offset=$offset "$OUTPUT" /mnt/disk 2>/dev/null; then
    patch_grub /mnt/disk
    umount /mnt/disk
  fi
done
rmdir /mnt/disk 2>/dev/null || true
`
	if sourceFormat == "qcow2" {
		return fmt.Sprintf("set -e\nOUTPUT=%q\napt-get update -qq && apt-get install -y -qq curl qemu-utils util-linux >/dev/null\ncurl -fsSL -o %q %q\nqemu-img convert -f qcow2 -O raw %q \"$OUTPUT\"%s", output, source, sourceURL, source, grubPatch)
	}
	return fmt.Sprintf("set -e\nOUTPUT=%q\napt-get update -qq && apt-get install -y -qq curl util-linux >/dev/null\ncurl -fsSL -o \"$OUTPUT\" %q%s", output, sourceURL, grubPatch)
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

	// Import job: download and convert qcow2→raw when needed.
	// CH does not support qcow2 compressed blocks; runtime format is always raw.
	sourceURL := img.Spec.Source.HTTP.URL
	sourceFormat := string(img.Spec.Format)
	script := importScript(sourceURL, sourceFormat)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: img.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						// Required for mount -o loop when patching GRUB
						RunAsUser: ptr.To(int64(0)),
					},
					Containers: []corev1.Container{{
						Name:    "import",
						Image:   "ubuntu:22.04",
						Command: []string{"sh", "-c", script},
						SecurityContext: &corev1.SecurityContext{
							Privileged: ptr.To(true),
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
