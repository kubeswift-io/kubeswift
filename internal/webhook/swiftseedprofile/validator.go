package swiftseedprofile

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	seedv1alpha1 "github.com/kubeswift-io/kubeswift/api/seed/v1alpha1"
)

// Validator validates SwiftSeedProfile resources.
type Validator struct{}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	sp, ok := obj.(*seedv1alpha1.SwiftSeedProfile)
	if !ok {
		return nil, fmt.Errorf("expected SwiftSeedProfile, got %T", obj)
	}
	return nil, validateSwiftSeedProfile(sp)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	sp, ok := newObj.(*seedv1alpha1.SwiftSeedProfile)
	if !ok {
		return nil, fmt.Errorf("expected SwiftSeedProfile, got %T", newObj)
	}
	return nil, validateSwiftSeedProfile(sp)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwiftSeedProfile(sp *seedv1alpha1.SwiftSeedProfile) error {
	spec := &sp.Spec
	if spec.Datasource != seedv1alpha1.DatasourceNoCloud {
		return fmt.Errorf("spec.datasource must be NoCloud for MVP, got %q", spec.Datasource)
	}
	if spec.Datasource == seedv1alpha1.DatasourceNoCloud {
		hasUserData := spec.UserData != "" || spec.UserDataFrom != nil
		if !hasUserData {
			return fmt.Errorf("spec.userData or spec.userDataFrom is required when datasource is NoCloud")
		}
	}
	return nil
}
