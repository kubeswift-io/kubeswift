package swiftimage

import (
	"context"
	"fmt"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/imageref"
)

// CloneSeedFinalizer is placed on both the SwiftImage and the seed
// VolumeSnapshot when cloneStrategy=snapshot. The finalizer blocks
// deletion until no SwiftGuests in the SwiftImage's namespace still
// reference the image. This is load-bearing for true copy-on-write CSI
// drivers (Rook Ceph, EBS) where deleting the snapshot mid-clone would
// corrupt active clones, and defensive for full-copy drivers like
// Longhorn where the same operation is non-disruptive but the operator
// experience benefits from a uniform safety guarantee.
const CloneSeedFinalizer = "kubeswift.io/clone-seed-protected"

// EnsureCloneSeedFinalizers makes sure the SwiftImage and its seed
// VolumeSnapshot both carry CloneSeedFinalizer when the image uses the
// snapshot strategy. Idempotent — safe to call from every reconcile.
func (r *SwiftImageReconciler) EnsureCloneSeedFinalizers(ctx context.Context, img *imagev1alpha1.SwiftImage) error {
	if img.Spec.CloneStrategy != imagev1alpha1.CloneStrategySnapshot {
		return nil
	}
	if !controllerutil.ContainsFinalizer(img, CloneSeedFinalizer) {
		controllerutil.AddFinalizer(img, CloneSeedFinalizer)
		if err := r.Update(ctx, img); err != nil {
			return fmt.Errorf("add finalizer to SwiftImage: %w", err)
		}
	}
	// Add finalizer to the seed snapshot only if it already exists.
	// (It is created later in the state machine; the snapshot.go path
	// running ahead of finalizer setup is acceptable because deletion
	// blocks via the SwiftImage finalizer in any case.)
	snapName := CloneSeedSnapshotName(img.Name)
	var snap volumesnapshotv1.VolumeSnapshot
	if err := r.Get(ctx, client.ObjectKey{Name: snapName, Namespace: img.Namespace}, &snap); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get clone-seed snapshot for finalizer: %w", err)
	}
	if !controllerutil.ContainsFinalizer(&snap, CloneSeedFinalizer) {
		controllerutil.AddFinalizer(&snap, CloneSeedFinalizer)
		if err := r.Update(ctx, &snap); err != nil {
			return fmt.Errorf("add finalizer to clone-seed snapshot: %w", err)
		}
	}
	return nil
}

// HandleCloneSeedDeletion drives the SwiftImage's deletion path when
// CloneSeedFinalizer is present. Returns canRemove=true once it is safe
// to drop the finalizers and let GC reap the resources.
//
// Safety condition: no SwiftGuests in the SwiftImage's namespace still
// reference this image. The same condition gates removal of the seed
// VolumeSnapshot's finalizer.
func (r *SwiftImageReconciler) HandleCloneSeedDeletion(ctx context.Context, img *imagev1alpha1.SwiftImage) (canRemove bool, blockingNames []string, err error) {
	guests, err := imageref.ListGuestsReferencingImage(ctx, r.Client, img)
	if err != nil {
		return false, nil, fmt.Errorf("list dependent SwiftGuests: %w", err)
	}
	if len(guests) > 0 {
		names := make([]string, 0, len(guests))
		for i := range guests {
			names = append(names, guests[i].Name)
		}
		return false, names, nil
	}

	// Safe to release. Remove the snapshot finalizer first; the SwiftImage's
	// own finalizer is removed by the caller after this function returns.
	snapName := CloneSeedSnapshotName(img.Name)
	var snap volumesnapshotv1.VolumeSnapshot
	if getErr := r.Get(ctx, client.ObjectKey{Name: snapName, Namespace: img.Namespace}, &snap); getErr == nil {
		if controllerutil.ContainsFinalizer(&snap, CloneSeedFinalizer) {
			controllerutil.RemoveFinalizer(&snap, CloneSeedFinalizer)
			if updErr := r.Update(ctx, &snap); updErr != nil {
				return false, nil, fmt.Errorf("remove finalizer from clone-seed snapshot: %w", updErr)
			}
		}
	} else if !errors.IsNotFound(getErr) {
		return false, nil, fmt.Errorf("get clone-seed snapshot during deletion: %w", getErr)
	}
	return true, nil, nil
}
