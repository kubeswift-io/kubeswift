package swiftimage

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

// ValidateResult holds the outcome of validation.
type ValidateResult struct {
	OK    bool
	Path  string
	Size  int64
	Error string
}

// Validate verifies the image exists and size. Uses spec.format only; no guessing.
// For MVP: verifies import PVC exists; full file validation would require a Pod.
func (r *SwiftImageReconciler) Validate(ctx context.Context, img *imagev1alpha1.SwiftImage, pvcPath string) (*ValidateResult, error) {
	// Import PVC is at importPVCNamePrefix+img.Name
	pvcName := importPVCNamePrefix + img.Name
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: img.Namespace, Name: pvcName}, &pvc); err != nil {
		if errors.IsNotFound(err) {
			return &ValidateResult{OK: false, Error: "import PVC not found"}, nil
		}
		return nil, err
	}
	// Stub: assume valid; full validation would run a Pod to verify file
	return &ValidateResult{OK: true, Path: pvcPath, Size: 0}, nil
}
