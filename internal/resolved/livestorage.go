package resolved

import (
	"fmt"

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
