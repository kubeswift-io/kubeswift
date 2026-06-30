package swiftrestore

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	swiftguestctrl "github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

// rootPVCName returns the per-guest root-disk clone PVC name. Mirrors
// internal/controller/swiftguest.RootDiskCloneName.
func rootPVCName(guestName string) string {
	return swiftguestctrl.RootDiskPVCPrefix + guestName
}

// SnapshotHandle decodes a SwiftSnapshot disk Handle ("<ns>/<vs-name>") into
// (namespace, volumeSnapshotName). Returns ok=false if the handle is malformed.
func SnapshotHandle(handle string) (namespace, name string, ok bool) {
	parts := strings.SplitN(handle, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// findRootDisk returns the "root" disk entry from a SwiftSnapshot status.
func findRootDisk(status *snapshotv1alpha1.SwiftSnapshotStatus) *snapshotv1alpha1.SnapshotDiskRef {
	for i := range status.Disks {
		if status.Disks[i].Role == "root" {
			return &status.Disks[i]
		}
	}
	return nil
}

// ensureRestorePVC creates (idempotent) the per-guest root-disk PVC for
// the target SwiftGuest, sourced from the SwiftSnapshot's VolumeSnapshot.
//
// The PVC is labeled RestoreSeededLabel=true so the SwiftGuest controller's
// EnsureRootDiskClone path skips the Copy Job and treats the bound PVC as
// authoritative.
//
// Same-namespace by construction: SwiftRestore, SwiftSnapshot, the
// VolumeSnapshot, the source SwiftGuest, and the target SwiftGuest all live
// in the same namespace (the validation webhook rejects cross-namespace
// references — Phase 0 §6a).
func (r *SwiftRestoreReconciler) ensureRestorePVC(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	pvcName, vsName, storageClassName string,
	sizeBytes int64,
) error {
	var existing corev1.PersistentVolumeClaim
	getErr := r.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: restore.Namespace}, &existing)
	if getErr == nil {
		// Already exists — assume previous reconcile created it.
		return nil
	}
	if !errors.IsNotFound(getErr) {
		return fmt.Errorf("get restore PVC: %w", getErr)
	}

	apiGroup := "snapshot.storage.k8s.io"
	size := *resource.NewQuantity(sizeBytes, resource.BinarySI)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: restore.Namespace,
			Labels: map[string]string{
				swiftguestctrl.RestoreSeededLabel:     "true",
				"snapshot.kubeswift.io/swift-restore": restore.Name,
				"swift.kubeswift.io/role":             "root-disk",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(restore, swiftRestoreGVK),
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptrString(storageClassName),
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     vsName,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: size,
				},
			},
		},
	}
	if err := r.Create(ctx, pvc); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create restore PVC: %w", err)
	}
	return nil
}

// ensureTargetGuest creates (idempotent) the target SwiftGuest from a copy
// of the source SwiftGuest's spec.
//
// ResumeAfterRestore=true (default): inherits source guest's runPolicy.
// ResumeAfterRestore=false: forces target.runPolicy=Stopped so the operator
// can inspect the restored disk before booting it.
func (r *SwiftRestoreReconciler) ensureTargetGuest(
	ctx context.Context,
	restore *snapshotv1alpha1.SwiftRestore,
	source *swiftv1alpha1.SwiftGuest,
) (*swiftv1alpha1.SwiftGuest, error) {
	var existing swiftv1alpha1.SwiftGuest
	getErr := r.Get(ctx, client.ObjectKey{Name: restore.Spec.TargetGuest.Name, Namespace: restore.Namespace}, &existing)
	if getErr == nil {
		return &existing, nil
	}
	if !errors.IsNotFound(getErr) {
		return nil, fmt.Errorf("get target SwiftGuest: %w", getErr)
	}

	target := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restore.Spec.TargetGuest.Name,
			Namespace: restore.Namespace,
			Labels: map[string]string{
				"snapshot.kubeswift.io/swift-restore": restore.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(restore, swiftRestoreGVK),
			},
		},
		Spec: source.Spec,
	}
	if !restore.Spec.ResumeAfterRestore {
		target.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	}

	if err := r.Create(ctx, target); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create target SwiftGuest: %w", err)
	}

	// Re-fetch in case AlreadyExists raced.
	if err := r.Get(ctx, client.ObjectKey{Name: target.Name, Namespace: target.Namespace}, &existing); err != nil {
		return nil, err
	}
	return &existing, nil
}

// sourceStorageClass returns the source per-guest PVC's StorageClassName,
// or an error if the PVC is missing / has no class set. This determines the
// class used for the restored PVC.
func (r *SwiftRestoreReconciler) sourceStorageClass(ctx context.Context, namespace, sourceGuestName string) (string, error) {
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: rootPVCName(sourceGuestName), Namespace: namespace}, &pvc); err != nil {
		return "", fmt.Errorf("get source per-guest PVC: %w", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return "", fmt.Errorf("source PVC %s has no storage class", pvc.Name)
	}
	return *pvc.Spec.StorageClassName, nil
}

func ptrString(s string) *string { return &s }

var swiftRestoreGVK = schema.GroupVersionKind{
	Group:   "snapshot.kubeswift.io",
	Version: "v1alpha1",
	Kind:    "SwiftRestore",
}
