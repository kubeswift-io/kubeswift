package swiftseedprofile

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"

	seedv1alpha1 "github.com/kubeswift-io/kubeswift/api/seed/v1alpha1"
)

// Defaulter defaults SwiftSeedProfile fields.
type Defaulter struct{}

func (d *Defaulter) Default(ctx context.Context, obj runtime.Object) error {
	sp, ok := obj.(*seedv1alpha1.SwiftSeedProfile)
	if !ok {
		return nil
	}
	if sp.Spec.Datasource == "" {
		sp.Spec.Datasource = seedv1alpha1.DatasourceNoCloud
	}
	return nil
}
