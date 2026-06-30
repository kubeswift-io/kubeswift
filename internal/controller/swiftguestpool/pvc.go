package swiftguestpool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// pvcName returns the PVC name for a given volume claim template, pool, and index.
func pvcName(templateName, poolName string, index int) string {
	return fmt.Sprintf("%s-%s-%d", templateName, poolName, index)
}

// ensurePVC creates a PVC for the given volume claim template and index if it doesn't exist.
// The PVC is owned by the pool (not the SwiftGuest) so it survives VM replacement.
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
		return name, nil // already exists
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

	if err := controllerutil.SetControllerReference(pool, pvc, r.Client.Scheme()); err != nil {
		return "", fmt.Errorf("set PVC owner reference: %w", err)
	}

	if err := r.Create(ctx, pvc); err != nil {
		return "", fmt.Errorf("create PVC %s: %w", name, err)
	}

	return name, nil
}
