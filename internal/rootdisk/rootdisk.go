// Package rootdisk names a SwiftGuest's root-disk PVC.
//
// A guest's root disk starts as the PVC swiftguest-root-<guest> (generation 0).
// An in-place disk restore (#711) cannot rename a PVC, so it seeds a new one,
// swiftguest-root-<guest>-g<n>, and switches the guest to it. The PVCs
// themselves record which one is current, so nothing depends on a status field
// that a status rewrite or a backup without status could lose: the current root
// disk is the highest-generation PVC that the guest controls, carries the
// root-disk labels and is not being deleted, else the generation-0 name.
//
// Every place that needs an existing guest's root PVC resolves it here. A guest
// that has never had its disk restored in place resolves to the old name, so
// nothing changes for it.
package rootdisk

import (
	"context"
	"fmt"
	"strconv"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Prefix is the root-disk PVC name prefix.
	Prefix = "swiftguest-root-"
	// LabelGuest names the guest a root-disk PVC belongs to.
	LabelGuest = "swift.kubeswift.io/guest"
	// LabelRole is "root-disk" on a guest's root-disk PVC.
	LabelRole = "swift.kubeswift.io/role"
	// RoleRootDisk is LabelRole's value on a root-disk PVC.
	RoleRootDisk = "root-disk"
	// RoleRetired is LabelRole's value on a root-disk PVC an in-place restore
	// replaced but could not delete yet: VolumeSnapshots were taken from it,
	// and some CSI drivers (Longhorn) keep a snapshot inside its source
	// volume, so deleting the PVC would destroy them. The guest controller
	// deletes it once no VolumeSnapshot names it.
	RoleRetired = "root-disk-retired"
	// AnnotationGeneration is the generation (1, 2, ...) of a root-disk PVC an
	// in-place restore created. The generation-0 PVC does not carry it.
	AnnotationGeneration = "swift.kubeswift.io/root-disk-generation"
)

// DefaultName is the generation-0 root-disk PVC name of a guest.
func DefaultName(guestName string) string { return Prefix + guestName }

// GenerationName is the name of a guest's generation-n root-disk PVC.
func GenerationName(guestName string, generation int) string {
	if generation <= 0 {
		return DefaultName(guestName)
	}
	return fmt.Sprintf("%s%s-g%d", Prefix, guestName, generation)
}

// Generation returns the root-disk generation pvc holds for guest, or 0 when it
// is not one of guest's generation PVCs: wrong guest or role label, not
// controlled by guest (a PVC left by an earlier guest of the same name), being
// deleted, or no valid generation annotation.
func Generation(guest metav1.Object, pvc *corev1.PersistentVolumeClaim) int {
	if pvc.DeletionTimestamp != nil ||
		pvc.Labels[LabelGuest] != guest.GetName() ||
		pvc.Labels[LabelRole] != RoleRootDisk {
		return 0
	}
	owner := metav1.GetControllerOf(pvc)
	if owner == nil || owner.UID != guest.GetUID() || guest.GetUID() == "" {
		return 0
	}
	n, err := strconv.Atoi(pvc.Annotations[AnnotationGeneration])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Select picks guest's current root-disk PVC from pvcs (the namespace's PVCs, or
// any superset of the guest's): the highest generation, else generation 0.
func Select(guest metav1.Object, pvcs []corev1.PersistentVolumeClaim) (string, int) {
	best := 0
	name := DefaultName(guest.GetName())
	for i := range pvcs {
		if g := Generation(guest, &pvcs[i]); g > best {
			best, name = g, pvcs[i].Name
		}
	}
	return name, best
}

// Claim returns the name and generation of guest's current root-disk PVC. It
// lists the guest's labelled PVCs through r; pass an uncached reader where a
// stale answer would boot the wrong disk.
func Claim(ctx context.Context, r client.Reader, guest metav1.Object) (string, int, error) {
	var list corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &list, client.InNamespace(guest.GetNamespace()),
		client.MatchingLabels{LabelGuest: guest.GetName(), LabelRole: RoleRootDisk}); err != nil {
		return "", 0, fmt.Errorf("list root-disk PVCs of %s/%s: %w", guest.GetNamespace(), guest.GetName(), err)
	}
	name, gen := Select(guest, list.Items)
	return name, gen, nil
}

// SnapshotsOf returns the names of the VolumeSnapshots in namespace taken from
// PVC claim.
func SnapshotsOf(ctx context.Context, r client.Reader, namespace, claim string) ([]string, error) {
	var list volumesnapshotv1.VolumeSnapshotList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list VolumeSnapshots in %s: %w", namespace, err)
	}
	var out []string
	for i := range list.Items {
		if src := list.Items[i].Spec.Source.PersistentVolumeClaimName; src != nil && *src == claim {
			out = append(out, list.Items[i].Name)
		}
	}
	return out, nil
}
