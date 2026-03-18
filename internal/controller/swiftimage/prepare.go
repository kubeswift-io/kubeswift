package swiftimage

import (
	"context"

	"k8s.io/apimachinery/pkg/api/resource"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

// PrepareResult holds the outcome of preparation.
type PrepareResult struct {
	PVCRef   *imagev1alpha1.PVCObjectReference
	Format   imagev1alpha1.DiskFormat
	Size     *resource.Quantity
	SizeHint int64
	Success  bool
	Error    string
}

// Prepare converts the image to runtime format if needed. Target format is raw when spec.format is qcow2.
// When spec.format is raw, no conversion; use artifact as-is.
// SizeHint is used when the converter returns size 0 (e.g. pass-through).
func (r *SwiftImageReconciler) Prepare(ctx context.Context, img *imagev1alpha1.SwiftImage, sourcePath string, pvcRef *imagev1alpha1.PVCObjectReference, sizeHint int64) (*PrepareResult, error) {
	if pvcRef == nil {
		pvcRef = &imagev1alpha1.PVCObjectReference{
			Name:      importPVCNamePrefix + img.Name,
			Namespace: img.Namespace,
		}
	}
	sourceFormat := img.Spec.Format
	targetFormat := imagev1alpha1.DiskFormatRaw
	if sourceFormat == imagev1alpha1.DiskFormatRaw {
		targetFormat = imagev1alpha1.DiskFormatRaw
	}

	converter := r.Converter
	if converter == nil {
		converter = StubConverter{}
	}

	preparedPath, size, err := converter.Prepare(ctx, sourcePath, sourceFormat, targetFormat)
	if err != nil {
		return &PrepareResult{Success: false, Error: err.Error()}, nil
	}

	_ = preparedPath
	if size == 0 && sizeHint > 0 {
		size = sizeHint
	}
	q := resource.NewQuantity(size, resource.BinarySI)
	return &PrepareResult{
		PVCRef:  pvcRef,
		Format:  targetFormat,
		Size:    q,
		Success: true,
	}, nil
}
