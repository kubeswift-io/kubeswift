package swiftimage

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

// Defaulter defaults SwiftImage fields.
type Defaulter struct{}

func (d *Defaulter) Default(ctx context.Context, obj runtime.Object) error {
	img, ok := obj.(*imagev1alpha1.SwiftImage)
	if !ok {
		return nil
	}
	if img.Spec.Format == "" {
		img.Spec.Format = imagev1alpha1.DiskFormatRaw
	}
	return nil
}
