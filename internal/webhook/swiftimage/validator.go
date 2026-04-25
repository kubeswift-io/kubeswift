package swiftimage

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

// Validator validates SwiftImage resources.
type Validator struct{}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	img, ok := obj.(*imagev1alpha1.SwiftImage)
	if !ok {
		return nil, fmt.Errorf("expected SwiftImage, got %T", obj)
	}
	return nil, validateSwiftImage(img)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	img, ok := newObj.(*imagev1alpha1.SwiftImage)
	if !ok {
		return nil, fmt.Errorf("expected SwiftImage, got %T", newObj)
	}
	oldImg, ok := oldObj.(*imagev1alpha1.SwiftImage)
	if !ok {
		return nil, fmt.Errorf("expected SwiftImage, got %T", oldObj)
	}
	return nil, validateSwiftImageUpdate(oldImg, img)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwiftImage(img *imagev1alpha1.SwiftImage) error {
	src := &img.Spec.Source
	n := 0
	if src.HTTP != nil {
		n++
		if src.HTTP.URL == "" {
			return fmt.Errorf("spec.source.http.url is required when http source is specified")
		}
	}
	if src.PVCClone != nil {
		n++
		if src.PVCClone.Name == "" {
			return fmt.Errorf("spec.source.pvcClone.name is required when pvcClone source is specified")
		}
	}
	if src.Upload != nil {
		n++
	}
	if n == 0 {
		return fmt.Errorf("spec.source: exactly one of http, pvcClone, or upload must be specified")
	}
	if n > 1 {
		return fmt.Errorf("spec.source: only one of http, pvcClone, or upload may be specified")
	}
	if img.Spec.Format == "" {
		return fmt.Errorf("spec.format is required (raw or qcow2)")
	}
	if err := validateCloneStrategy(img); err != nil {
		return err
	}
	return nil
}

// validateCloneStrategy enforces the rules around cloneStrategy /
// volumeSnapshotClassName / cloneStorageClassName described in
// docs/images/clone-strategies.md.
//
//   - cloneStrategy "" (default) is treated as "copy" and accepts no
//     snapshot fields.
//   - cloneStrategy "snapshot" requires volumeSnapshotClassName.
//   - volumeSnapshotClassName is meaningful only with cloneStrategy=snapshot.
func validateCloneStrategy(img *imagev1alpha1.SwiftImage) error {
	strategy := img.Spec.CloneStrategy
	switch strategy {
	case "", imagev1alpha1.CloneStrategyCopy:
		if img.Spec.VolumeSnapshotClassName != "" {
			return fmt.Errorf("spec.volumeSnapshotClassName is only valid when spec.cloneStrategy is 'snapshot'")
		}
	case imagev1alpha1.CloneStrategySnapshot:
		if img.Spec.VolumeSnapshotClassName == "" {
			return fmt.Errorf("spec.volumeSnapshotClassName is required when spec.cloneStrategy is 'snapshot'")
		}
	default:
		return fmt.Errorf("spec.cloneStrategy: unsupported value %q (allowed: copy, snapshot)", strategy)
	}
	return nil
}

func validateSwiftImageUpdate(oldImg, img *imagev1alpha1.SwiftImage) error {
	if err := validateSwiftImage(img); err != nil {
		return err
	}
	if oldImg.Status.Phase == imagev1alpha1.SwiftImagePhaseReady {
		if oldImg.Spec.Source != img.Spec.Source || oldImg.Spec.Format != img.Spec.Format {
			return fmt.Errorf("SwiftImage spec is immutable when status.phase is Ready")
		}
	}
	// CloneStrategy is immutable after import has progressed past Pending.
	// Switching strategies on a partially imported image leaves the prepared
	// PVC in an ambiguous state (no clone seed for snapshot path, no
	// guarantee of size match for copy path).
	if oldImg.Spec.CloneStrategy != img.Spec.CloneStrategy &&
		oldImg.Status.Phase != "" &&
		oldImg.Status.Phase != imagev1alpha1.SwiftImagePhasePending {
		return fmt.Errorf("spec.cloneStrategy is immutable once import has started (current phase=%s)", oldImg.Status.Phase)
	}
	return nil
}
