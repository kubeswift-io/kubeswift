package swiftguestpool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// pvcName returns the PVC name for a given volume claim template, pool, and index.
func pvcName(templateName, poolName string, index int) string {
	return fmt.Sprintf("%s-%s-%d", templateName, poolName, index)
}

// ensurePVC creates a PVC for the given volume claim template and index if it
// doesn't exist, and returns its name.
//
// The PVC is retained: it has no owner reference, so neither replacing the
// replica's SwiftGuest nor deleting the pool deletes the data on it (the
// StatefulSet default). It used to carry the pool as its controller, which
// had the garbage collector delete every replica's data disk along with the
// pool, while the pool guide told users to clean the PVCs up by hand.
// Existing PVCs have that owner reference removed here.
//
// An existing PVC is adopted only if it is labelled for this pool. The name
// alone does not identify it -- template "data-web" in pool "x" and template
// "data" in pool "web-x" both give "data-web-x-0" -- so without the check two
// pools' replicas could share one data disk.
func (r *SwiftGuestPoolReconciler) ensurePVC(
	ctx context.Context,
	pool *swiftv1alpha1.SwiftGuestPool,
	template swiftv1alpha1.PersistentVolumeClaimTemplate,
	index int,
) (string, error) {
	name := pvcName(template.Metadata.Name, pool.Name, index)

	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: pool.Namespace}, &existing)
	if err == nil {
		if owner := existing.Labels[swiftv1alpha1.LabelPoolName]; owner != pool.Name {
			return "", fmt.Errorf("PVC %s already exists but is labelled %s=%q, not %q: refusing to attach a claim this pool did not create (another pool whose PVC names collide with this one's, or a pre-existing claim)",
				name, swiftv1alpha1.LabelPoolName, owner, pool.Name)
		}
		return name, r.dropPoolOwnership(ctx, pool, &existing)
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	// Merge template labels with pool reference labels.
	labels := make(map[string]string)
	for k, v := range template.Metadata.Labels {
		labels[k] = v
	}
	labels[swiftv1alpha1.LabelPoolName] = pool.Name

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pool.Namespace,
			Labels:    labels,
		},
		Spec: *template.Spec.DeepCopy(),
	}

	if err := r.Create(ctx, pvc); err != nil {
		return "", fmt.Errorf("create PVC %s: %w", name, err)
	}

	return name, nil
}

// dropPoolOwnership removes the pool's owner reference from a PVC created
// before PVCs were retained, so deleting the pool no longer deletes it.
func (r *SwiftGuestPoolReconciler) dropPoolOwnership(
	ctx context.Context,
	pool *swiftv1alpha1.SwiftGuestPool,
	pvc *corev1.PersistentVolumeClaim,
) error {
	kept := pvc.OwnerReferences[:0:0]
	for _, ref := range pvc.OwnerReferences {
		if ref.UID != pool.UID {
			kept = append(kept, ref)
		}
	}
	if len(kept) == len(pvc.OwnerReferences) {
		return nil
	}
	patch := client.MergeFromWithOptions(pvc.DeepCopy(), client.MergeFromWithOptimisticLock{})
	pvc.OwnerReferences = kept
	if err := r.Patch(ctx, pvc, patch); err != nil {
		return fmt.Errorf("release PVC %s from pool ownership: %w", pvc.Name, err)
	}
	return nil
}
