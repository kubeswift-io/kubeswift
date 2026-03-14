package swiftguest

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Validator validates SwiftGuest resources.
type Validator struct{}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	g, ok := obj.(*swiftv1alpha1.SwiftGuest)
	if !ok {
		return nil, fmt.Errorf("expected SwiftGuest, got %T", obj)
	}
	return nil, validateSwiftGuest(g)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	g, ok := newObj.(*swiftv1alpha1.SwiftGuest)
	if !ok {
		return nil, fmt.Errorf("expected SwiftGuest, got %T", newObj)
	}
	return nil, validateSwiftGuest(g)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwiftGuest(g *swiftv1alpha1.SwiftGuest) error {
	spec := &g.Spec
	if spec.ImageRef.Name == "" {
		return fmt.Errorf("spec.imageRef.name is required")
	}
	if spec.GuestClassRef.Name == "" {
		return fmt.Errorf("spec.guestClassRef.name is required")
	}
	if spec.RunPolicy != "" && spec.RunPolicy != swiftv1alpha1.RunPolicyRunning && spec.RunPolicy != swiftv1alpha1.RunPolicyStopped {
		return fmt.Errorf("spec.runPolicy must be Running or Stopped, got %q", spec.RunPolicy)
	}
	return nil
}
