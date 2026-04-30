package v1alpha1

import corev1 "k8s.io/api/core/v1"

// StorageSpec selects per-PVC storage characteristics for the
// controller-managed PVCs of a SwiftGuest. Today that is the root-disk
// clone PVC; in future, any other guest-owned PVC the controller creates
// inherits the same defaults until it grows its own per-disk override.
//
// Operators declare it on SwiftGuestClass for the cluster default and may
// override it per-guest on SwiftGuestSpec. Resolution is per-field: each
// non-empty field on SwiftGuest wins, then the same field on
// SwiftGuestClass, then system defaults (ReadWriteOnce + Filesystem).
//
// Hard CRD-level rejection: ReadWriteMany + Filesystem is invalid.
// Filesystem RWX (e.g. Longhorn Generic RWX, NFS-based) is NOT live-
// migration-capable, but its name implies shareability that operators
// reasonably read as "live-migratable." Reject the combination at
// admission rather than silently advertise a capability we cannot
// deliver. This ships the live-migration semantic gap directly into the
// CRD schema, which kubectl-side dry-run catches offline.
//
// +kubebuilder:validation:XValidation:rule="!(self.accessMode == 'ReadWriteMany' && (!has(self.volumeMode) || self.volumeMode == 'Filesystem'))",message="accessMode=ReadWriteMany requires volumeMode=Block; Filesystem RWX is not live-migration-capable (see docs/design/storage-access-mode.md)"
type StorageSpec struct {
	// AccessMode is the PVC accessMode. Defaults to ReadWriteOnce.
	// ReadWriteOnce is security-conservative and matches Phase 1 offline
	// migration behaviour. ReadWriteMany unlocks Phase 3 live migration of
	// disk-boot guests but only in combination with VolumeMode=Block (see
	// the XValidation rule on this struct).
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	// +optional
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`

	// VolumeMode is the PVC volumeMode. Defaults to Filesystem. Block is
	// required for live-migration-capable RWX (KubeVirt model) and for
	// some CSI drivers' raw-device passthrough; the value flows to PVC
	// spec verbatim.
	// +kubebuilder:validation:Enum=Filesystem;Block
	// +optional
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`

	// StorageClassName names the StorageClass for controller-created
	// PVCs. Nil/unset falls through to SwiftGuestClass.spec.storage
	// .storageClassName, then to the source SwiftImage's PVC's class
	// (existing pre-PR-32 behaviour), then to the cluster default. Set
	// it explicitly when a specific class is required — e.g. a Longhorn
	// class with parameters.migratable="true" for RWX+Block guests.
	//
	// *string distinguishes "not set" (fall through) from the empty
	// string (explicit cluster-default selection). The explicit-empty
	// case is rare in practice; the distinction is preserved for
	// forward compatibility with operators who want to override the
	// class to the cluster default explicitly.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// ResolvedStorageStatus echoes the post-resolution storage spec onto
// SwiftGuest.status for operator visibility (kubectl describe, swiftctl).
//
// liveMigrationCapable is INTENTIONALLY not stored here. It is a pure
// function of the resolved spec (true iff AccessMode=ReadWriteMany and
// VolumeMode=Block); placing derived facts in status creates a write-
// back race with the SwiftMigration validating webhook during cluster
// restore, where the webhook would observe pre-reconcile false negatives
// and reject valid migrations. The webhook recomputes the value from
// the resolved spec at admission time; swiftctl describe and operator
// UIs do the same.
type ResolvedStorageStatus struct {
	// AccessMode is the resolved PVC accessMode actually used for
	// controller-created PVCs.
	// +optional
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
	// VolumeMode is the resolved PVC volumeMode actually used for
	// controller-created PVCs.
	// +optional
	VolumeMode corev1.PersistentVolumeMode `json:"volumeMode,omitempty"`
	// StorageClassName is the resolved StorageClass actually used. Empty
	// when resolution fell through to the source-image-class default.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// Storage system defaults. Used when both guest and class omit a field.
// The ResolvedStorage helper applies these in resolved/merge.go; tests
// in this package lock them in so a future change is a deliberate one.
const (
	DefaultStorageAccessMode = corev1.ReadWriteOnce
	DefaultStorageVolumeMode = corev1.PersistentVolumeFilesystem
)

// IsLiveMigrationCapable returns true iff the resolved storage spec
// permits live migration of disk-boot guests. The rule is RWX+Block —
// see KubeVirt's RWX-required-for-live-migration model and Longhorn's
// distinction between Generic (Filesystem, NFS-based, NOT migratable)
// and Migratable (Block, KubeVirt-style live-migration-ready) RWX.
//
// This is the canonical implementation. The SwiftMigration webhook,
// swiftctl describe, and any future migration-mode auto-selection logic
// MUST use this function rather than re-deriving the rule. Storing the
// value in CRD status is rejected as a write-back-race hazard; see
// ResolvedStorageStatus's doc comment.
func IsLiveMigrationCapable(s *ResolvedStorageStatus) bool {
	if s == nil {
		return false
	}
	return s.AccessMode == corev1.ReadWriteMany && s.VolumeMode == corev1.PersistentVolumeBlock
}
