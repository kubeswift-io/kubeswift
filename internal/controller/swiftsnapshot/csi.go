package swiftsnapshot

import (
	"context"
	"fmt"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/names"
	"github.com/kubeswift-io/kubeswift/internal/rootdisk"
)

// VolumeSnapshotNamePrefix is prepended to the SwiftSnapshot name to derive
// the deterministic VolumeSnapshot name. Same-namespace by construction.
const VolumeSnapshotNamePrefix = "swift-snap-"

var swiftSnapshotGVK = schema.GroupVersionKind{
	Group:   "snapshot.kubeswift.io",
	Version: "v1alpha1",
	Kind:    "SwiftSnapshot",
}

// VolumeSnapshotName returns the deterministic VolumeSnapshot name for a
// SwiftSnapshot. Same-namespace by construction.
func VolumeSnapshotName(snapName string) string {
	return VolumeSnapshotNamePrefix + snapName
}

// ensureVolumeSnapshot creates (if missing) and returns the VolumeSnapshot
// backing this SwiftSnapshot. The returned bool is true once the snapshot
// is readyToUse; false means "still capturing, requeue".
//
// On hard failure (snapshot reports an error), returns errMsg with ready=false.
// The caller maps a non-empty errMsg to phase=Failed.
func (r *SwiftSnapshotReconciler) ensureVolumeSnapshot(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	sourcePVCName string,
) (ready bool, restoreSizeBytes int64, errMsg string, err error) {
	vsName := VolumeSnapshotName(snap.Name)

	var vs volumesnapshotv1.VolumeSnapshot
	getErr := r.Get(ctx, client.ObjectKey{Name: vsName, Namespace: snap.Namespace}, &vs)
	switch {
	case errors.IsNotFound(getErr):
		if createErr := r.createVolumeSnapshot(ctx, snap, vsName, sourcePVCName); createErr != nil {
			return false, 0, "", fmt.Errorf("create VolumeSnapshot: %w", createErr)
		}
		return false, 0, "", nil
	case getErr != nil:
		return false, 0, "", fmt.Errorf("get VolumeSnapshot: %w", getErr)
	}

	if vs.Status != nil && vs.Status.Error != nil && vs.Status.Error.Message != nil {
		return false, 0, *vs.Status.Error.Message, nil
	}
	if vs.Status == nil || vs.Status.ReadyToUse == nil || !*vs.Status.ReadyToUse {
		return false, 0, "", nil
	}
	if vs.Status.RestoreSize != nil {
		restoreSizeBytes = vs.Status.RestoreSize.Value()
	}
	return true, restoreSizeBytes, "", nil
}

// createVolumeSnapshot creates the snapshot.storage.k8s.io VolumeSnapshot
// owned by the SwiftSnapshot. ownerRef ensures GC when the SwiftSnapshot
// is deleted.
func (r *SwiftSnapshotReconciler) createVolumeSnapshot(
	ctx context.Context,
	snap *snapshotv1alpha1.SwiftSnapshot,
	vsName, sourcePVCName string,
) error {
	src := sourcePVCName
	vs := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vsName,
			Namespace: snap.Namespace,
			Labels: map[string]string{
				"snapshot.kubeswift.io/swift-snapshot": names.LabelValue(snap.Name),
				"snapshot.kubeswift.io/role":           "swift-snapshot",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(snap, swiftSnapshotGVK),
			},
		},
		Spec: volumesnapshotv1.VolumeSnapshotSpec{
			Source: volumesnapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &src,
			},
		},
	}
	if className := csiClassName(snap); className != "" {
		vs.Spec.VolumeSnapshotClassName = &className
	}
	if err := r.Create(ctx, vs); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// guestRootPVCName returns the name of the guest's current root-disk PVC (the
// guest controller's, never the shared SwiftImage PVC). An in-place restore may
// have swapped it for a later generation (#711). A guest that is gone resolves
// to the generation-0 name.
func (r *SwiftSnapshotReconciler) guestRootPVCName(ctx context.Context, namespace, guestName string) (string, error) {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: guestName, Namespace: namespace}, &guest); err != nil {
		if errors.IsNotFound(err) {
			return rootdisk.DefaultName(guestName), nil
		}
		return "", err
	}
	name, _, err := rootdisk.Claim(ctx, r.Client, &guest)
	return name, err
}

// guestRootPVC reads the per-guest root-disk clone PVC and returns nil if
// it doesn't exist yet (caller waits and retries).
func (r *SwiftSnapshotReconciler) guestRootPVC(ctx context.Context, namespace, guestName string) (*corev1.PersistentVolumeClaim, error) {
	name, err := r.guestRootPVCName(ctx, namespace, guestName)
	if err != nil {
		return nil, err
	}
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &pvc); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &pvc, nil
}

// csiClassName is the VolumeSnapshotClass the snapshot uses: the one recorded
// in status.location, else (a snapshot taken before storage locations
// existed) its spec's. Empty leaves the choice to the snapshotter's default.
func csiClassName(snap *snapshotv1alpha1.SwiftSnapshot) string {
	if l := snap.Status.Location; l != nil {
		return l.VolumeSnapshotClassName
	}
	if c := snap.Spec.Backend.CSIVolumeSnapshot; c != nil {
		return c.VolumeSnapshotClassName
	}
	return ""
}
