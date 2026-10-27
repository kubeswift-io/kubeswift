package resolved

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// LiveMigrationStorageError returns nil when the guest's resolved storage allows
// live migration, and an error naming why not otherwise.
//
// Kernel-boot guests (kernelRef set) have no root-disk PVC — the root
// filesystem is the initramfs, carried in the migrated memory — so there is no
// shared storage to coordinate and they pass. Disk-boot guests need
// ReadWriteMany + Block (the shared-storage rule; Longhorn's Migratable RWX):
// Filesystem RWX is not live-migration-capable, and RWO cannot attach on the
// destination while the source still holds it (Multi-Attach).
//
// Shared by the SwiftMigration admission webhook and the controller so the two
// cannot drift; class may be nil (MergeStorage then uses the defaults).
func LiveMigrationStorageError(guest *swiftv1alpha1.SwiftGuest, class *swiftv1alpha1.SwiftGuestClass) error {
	if guest.Spec.KernelRef != nil {
		return nil
	}
	storage := MergeStorage(guest, class)
	if storage.IsLiveMigrationCapable() {
		return nil
	}
	return fmt.Errorf(
		"SwiftGuest %q resolved storage is accessMode=%s volumeMode=%s; live migration requires accessMode=ReadWriteMany AND volumeMode=Block (Filesystem RWX is not live-migration-capable). "+
			"Set spec.storage on the SwiftGuest or its SwiftGuestClass to ReadWriteMany+Block, or use spec.mode=offline.",
		guest.Name, storage.AccessMode, storage.VolumeMode,
	)
}

// LiveMigrationVolumesError applies the live-migration rule to every volume
// the launcher pod mounts besides the root disk: data disks and virtio-fs
// filesystems backed by a PVC. During a live migration the source and the
// destination launcher hold each volume at once, on two nodes, so a
// ReadWriteOnce volume cannot attach on the destination (Multi-Attach) and the
// migration could only fail later, after its Preparing budget.
//
//   - A disk the VM writes to (every data disk) needs ReadWriteMany AND Block,
//     the root disk's rule. Blank and image-backed data disks are guest-owned
//     ReadWriteOnce PVCs today, so they do not qualify.
//   - A PVC mounted into the launcher as a directory (dataDiskRefs pvcRef
//     without attachAsDisk, filesystems pvcRef) needs ReadWriteMany.
//
// gate is non-nil, naming the first volume that does not qualify, when the
// guest cannot be live-migrated; err is a failed read the caller may retry. A
// PVC that does not exist is a gate (it cannot be shown to qualify). Shared by
// the SwiftMigration admission webhook and the controller so the two cannot
// drift.
func LiveMigrationVolumesError(ctx context.Context, c client.Reader, guest *swiftv1alpha1.SwiftGuest) (gate error, err error) {
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("SwiftGuest %q cannot be live-migrated: "+format+
			"; the source and destination hold every volume at once on two nodes. Use spec.mode=offline", append([]any{guest.Name}, a...)...)
	}
	check := func(what, pvcName string, needBlock bool) (error, error) {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: pvcName}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				return refuse("%s PVC %q does not exist", what, pvcName), nil
			}
			return nil, fmt.Errorf("get %s PVC %q: %w", what, pvcName, err)
		}
		modes := pvc.Status.AccessModes
		if len(modes) == 0 {
			modes = pvc.Spec.AccessModes
		}
		block := pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock
		switch {
		case !slices.Contains(modes, corev1.ReadWriteMany):
			return refuse("%s PVC %q is %v, not ReadWriteMany", what, pvcName, modes), nil
		case needBlock && !block:
			return refuse("%s PVC %q is volumeMode Filesystem; a disk needs ReadWriteMany+Block", what, pvcName), nil
		}
		return nil, nil
	}
	if guest.Spec.DataDiskRef != nil {
		if gate, err := check(`data disk "data"`, BlankDataDiskPVCName(guest.Name, "data"), true); gate != nil || err != nil {
			return gate, err
		}
	}
	for i := range guest.Spec.DataDiskRefs {
		d := &guest.Spec.DataDiskRefs[i]
		var gate, err error
		switch {
		case d.ImageRef != nil, d.Blank != nil:
			// The guest's own copy of an image-backed disk is named like a blank one.
			gate, err = check(fmt.Sprintf("data disk %q", d.Name), BlankDataDiskPVCName(guest.Name, d.Name), true)
		case d.PVCRef != nil:
			gate, err = check(fmt.Sprintf("data disk %q", d.Name), d.PVCRef.Name, d.AttachAsDisk)
		}
		if gate != nil || err != nil {
			return gate, err
		}
	}
	for i := range guest.Spec.Filesystems {
		f := &guest.Spec.Filesystems[i]
		if f.Source.PVCRef == nil {
			continue
		}
		if gate, err := check(fmt.Sprintf("filesystem %q", f.Name), f.Source.PVCRef.Name, false); gate != nil || err != nil {
			return gate, err
		}
	}
	return nil, nil
}
