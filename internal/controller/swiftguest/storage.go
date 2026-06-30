package swiftguest

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// resolvedAccessModes returns the AccessModes slice for controller-
// created PVCs from the resolved storage spec. Always exactly one entry;
// AccessModes is a list-typed PVC field but KubeSwift's per-guest PVCs
// have a single mode each. Empty/legacy specs default to ReadWriteOnce.
func resolvedAccessModes(rg *resolved.ResolvedGuest) []corev1.PersistentVolumeAccessMode {
	mode := rg.Storage.AccessMode
	if mode == "" {
		mode = string(corev1.ReadWriteOnce)
	}
	return []corev1.PersistentVolumeAccessMode{corev1.PersistentVolumeAccessMode(mode)}
}

// resolvedVolumeMode returns the VolumeMode pointer for PVC spec from
// the resolved storage spec. PVC.Spec.VolumeMode is *PersistentVolumeMode;
// nil means "Filesystem" (Kubernetes default). We return a non-nil
// pointer when the resolved spec is non-empty to make the choice
// explicit in the PVC manifest, including for Filesystem (so future
// audits show the resolved value rather than relying on field-omission
// semantics).
func resolvedVolumeMode(rg *resolved.ResolvedGuest) *corev1.PersistentVolumeMode {
	mode := rg.Storage.VolumeMode
	if mode == "" {
		mode = string(corev1.PersistentVolumeFilesystem)
	}
	pvm := corev1.PersistentVolumeMode(mode)
	return &pvm
}

// resolvedStorageClassName returns the StorageClassName pointer for PVC
// spec. Resolution: explicit rg.Storage.StorageClassName wins; empty
// falls through to the source SwiftImage's PVC class (the pre-PR-32
// behaviour). The fallback is essential — without an explicit class
// override, per-guest clones must land on the same class as the source
// image (Longhorn refuses cross-class clones; see Phase 0 spike).
func resolvedStorageClassName(rg *resolved.ResolvedGuest, srcPVC *corev1.PersistentVolumeClaim) *string {
	if rg.Storage.StorageClassName != "" {
		s := rg.Storage.StorageClassName
		return &s
	}
	return srcPVC.Spec.StorageClassName
}

// LonghornCSIProvisioner is the StorageClass provisioner string the
// Longhorn CSI driver registers. We compare on this string to detect a
// Longhorn-managed StorageClass and only run the migratable-parameter
// check on those classes; non-Longhorn drivers (Ceph RBD, EBS, hostpath)
// pass through with StorageReady=True. Other CSI drivers will land their
// own per-driver checks once the storage architecture review picks the
// driver matrix.
const LonghornCSIProvisioner = "driver.longhorn.io"

// LonghornMigratableParameter is the StorageClass parameter Longhorn
// requires for KubeVirt-style RWX live migration. We do not auto-mutate
// the StorageClass — operators set it once at cluster setup; the
// controller surfaces a status condition when the chosen class lacks it.
const LonghornMigratableParameter = "migratable"

// checkStorageReady runs per-driver pre-flight checks against the
// resolved storage spec. Returns (reason, message, ok). The caller
// (controller.Reconcile) surfaces the result through SetStorageReadyCondition
// — best-effort, informational, NOT an admission gate.
//
// Today the only check fires for RWX+Block guests on a Longhorn class:
// the class must carry parameters.migratable="true" or the guest cannot
// be live-migrated even though it claims liveMigrationCapable. Non-
// Longhorn drivers and non-RWX guests pass through. The architecture
// review will define the driver matrix and may add Ceph RBD imageFeatures,
// EBS volume types, etc.
func (r *SwiftGuestReconciler) checkStorageReady(ctx context.Context, rg *resolved.ResolvedGuest) (reason, message string, ok bool) {
	if !rg.Storage.IsLiveMigrationCapable() {
		// Non-RWX or non-Block guests don't need the migratable check.
		return "", "", true
	}
	scName := rg.Storage.StorageClassName
	if scName == "" {
		// No explicit class. Fall-through resolves to the source
		// SwiftImage's class (rootdisk.go) — we don't have visibility into
		// that here at the resolved-spec layer. Don't false-alarm.
		return "", "Storage class falls through to source SwiftImage class; migratable check deferred to PVC bind", true
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, client.ObjectKey{Name: scName}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return "StorageClassNotFound",
				fmt.Sprintf("StorageClass %q not found; PVCs will fail to bind. Apply the class and the SwiftGuest will reconcile.", scName),
				false
		}
		return "StorageClassLookupFailed",
			fmt.Sprintf("look up StorageClass %q: %v", scName, err),
			false
	}
	if sc.Provisioner != LonghornCSIProvisioner {
		// Non-Longhorn driver — pass through. Future per-driver checks
		// land here.
		return "", "", true
	}
	if sc.Parameters[LonghornMigratableParameter] != "true" {
		return "LonghornNotMigratable",
			fmt.Sprintf(
				"Longhorn StorageClass %q is missing parameters.migratable=\"true\"; "+
					"RWX+Block guests on this class will provision but cannot be live-migrated. "+
					"Cluster admin: apply a Longhorn StorageClass with migratable=true (see docs/design/storage-access-mode.md).",
				scName,
			),
			false
	}
	return "", "", true
}
