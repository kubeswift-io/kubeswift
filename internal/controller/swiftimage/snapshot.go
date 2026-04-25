package swiftimage

import (
	"context"
	"fmt"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

var swiftImageGVK = schema.GroupVersionKind{
	Group:   imagev1alpha1.GroupName,
	Version: imagev1alpha1.Version,
	Kind:    "SwiftImage",
}

const (
	// CloneSeedSnapshotSuffix is appended to the SwiftImage name to derive
	// the deterministic VolumeSnapshot name used as the clone seed.
	CloneSeedSnapshotSuffix = "-clone-seed"

	// ReasonSnapshotting is reported on the Ready condition while the
	// clone-seed VolumeSnapshot is being created or waiting for readiness.
	ReasonSnapshotting   = "Snapshotting"
	ReasonSnapshotReady  = "SnapshotReady"
	ReasonSnapshotFailed = "SnapshotFailed"
)

// CloneSeedSnapshotName returns the deterministic VolumeSnapshot name for
// a SwiftImage's clone seed.
func CloneSeedSnapshotName(imageName string) string {
	return imageName + CloneSeedSnapshotSuffix
}

// EnsureCloneSeed reconciles the SwiftImage's clone-seed VolumeSnapshot.
// Returns (ready, sourceSizeBytes, err):
//   - ready=true when the snapshot exists, readyToUse=true; sourceSizeBytes
//     reflects the source PVC's storage request (this is the size the clone
//     PVC must request — Longhorn refuses different-size dataSource clones).
//   - ready=false with err=nil means the snapshot is being created or is
//     not yet ready; caller should requeue.
//   - err non-nil for hard failures (missing source PVC, missing
//     volumeSnapshotClassName, etc.).
//
// Same-namespace constraint: the snapshot is created in the SwiftImage's
// own namespace, never elsewhere — see Phase 0 spike finding §6a.
func (r *SwiftImageReconciler) EnsureCloneSeed(ctx context.Context, img *imagev1alpha1.SwiftImage) (bool, int64, error) {
	if img.Spec.CloneStrategy != imagev1alpha1.CloneStrategySnapshot {
		return false, 0, fmt.Errorf("EnsureCloneSeed called for non-snapshot strategy %q", img.Spec.CloneStrategy)
	}
	if img.Spec.VolumeSnapshotClassName == "" {
		return false, 0, fmt.Errorf("spec.volumeSnapshotClassName is required when cloneStrategy=snapshot")
	}
	if img.Status.PreparedArtifact == nil || img.Status.PreparedArtifact.PVCRef == nil {
		return false, 0, fmt.Errorf("preparedArtifact.pvcRef not yet set; cannot snapshot")
	}

	// Resolve source PVC size — this is the size the snapshot will record
	// as restoreSize and the size every clone PVC must request.
	srcRef := img.Status.PreparedArtifact.PVCRef
	srcNS := srcRef.Namespace
	if srcNS == "" {
		srcNS = img.Namespace
	}
	if srcNS != img.Namespace {
		// Defensive: never let the source PVC live outside the SwiftImage's
		// namespace. Phase 0 §6a — cross-namespace dataSource silently fails.
		return false, 0, fmt.Errorf("prepared PVC must be in same namespace as SwiftImage: pvc=%s/%s image=%s", srcNS, srcRef.Name, img.Namespace)
	}
	var srcPVC corev1.PersistentVolumeClaim
	if err := r.Get(ctx, client.ObjectKey{Name: srcRef.Name, Namespace: srcNS}, &srcPVC); err != nil {
		return false, 0, fmt.Errorf("get source PVC %s/%s: %w", srcNS, srcRef.Name, err)
	}
	sourceSize, ok := srcPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	if !ok {
		return false, 0, fmt.Errorf("source PVC %s/%s has no storage request", srcNS, srcRef.Name)
	}
	sourceSizeBytes := sourceSize.Value()

	// Look up (or create) the seed VolumeSnapshot.
	snapName := CloneSeedSnapshotName(img.Name)
	var snap volumesnapshotv1.VolumeSnapshot
	err := r.Get(ctx, client.ObjectKey{Name: snapName, Namespace: img.Namespace}, &snap)
	switch {
	case errors.IsNotFound(err):
		if createErr := r.createCloneSeedSnapshot(ctx, img, snapName); createErr != nil {
			return false, sourceSizeBytes, fmt.Errorf("create clone-seed snapshot: %w", createErr)
		}
		return false, sourceSizeBytes, nil
	case err != nil:
		return false, sourceSizeBytes, fmt.Errorf("get clone-seed snapshot: %w", err)
	}

	// Snapshot exists. Check readiness.
	if snap.Status == nil || snap.Status.ReadyToUse == nil || !*snap.Status.ReadyToUse {
		return false, sourceSizeBytes, nil
	}
	return true, sourceSizeBytes, nil
}

// createCloneSeedSnapshot creates a VolumeSnapshot of the SwiftImage's
// prepared PVC. The snapshot lives in the SwiftImage's namespace and is
// owned by the SwiftImage so it is garbage-collected when the SwiftImage
// is deleted (after the clone-seed-protected finalizer is removed by the
// reconciler's deletion path — see commit 6).
func (r *SwiftImageReconciler) createCloneSeedSnapshot(ctx context.Context, img *imagev1alpha1.SwiftImage, name string) error {
	srcPVCName := img.Status.PreparedArtifact.PVCRef.Name
	className := img.Spec.VolumeSnapshotClassName
	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: img.Namespace,
			Labels: map[string]string{
				"image.kubeswift.io/swift-image": img.Name,
				"image.kubeswift.io/role":        "clone-seed",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(img, swiftImageGVK),
			},
		},
		Spec: volumesnapshotv1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &className,
			Source: volumesnapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &srcPVCName,
			},
		},
	}
	if err := r.Create(ctx, snap); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
