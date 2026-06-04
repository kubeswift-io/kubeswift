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
	hasImage := spec.ImageRef != nil && spec.ImageRef.Name != ""
	hasKernel := spec.KernelRef != nil && spec.KernelRef.Name != ""
	hasClone := spec.CloneFromSnapshot != nil

	// Exactly one boot source: imageRef, kernelRef, or cloneFromSnapshot.
	sources := 0
	for _, s := range []bool{hasImage, hasKernel, hasClone} {
		if s {
			sources++
		}
	}
	if sources != 1 {
		return fmt.Errorf("exactly one of spec.imageRef, spec.kernelRef, or spec.cloneFromSnapshot must be set")
	}

	if hasClone {
		if spec.CloneFromSnapshot.SnapshotRef.Name == "" {
			return fmt.Errorf("spec.cloneFromSnapshot.snapshotRef.name is required")
		}
		// VFIO/GPU state cannot be CH-restored (Phase 0 Constraint #1), the same
		// rule the includeMemory+VFIO snapshot path enforces.
		if spec.GPUProfileRef != nil {
			return fmt.Errorf("spec.cloneFromSnapshot is mutually exclusive with spec.gpuProfileRef (VFIO state cannot be restored)")
		}
	}

	// guestClassRef is required by the CRD schema (a non-pointer struct field),
	// so it is required for every boot source — including clones, which ignore
	// it for resources (the resumed VM's CPU/memory come from the snapshot) but
	// must still set it to satisfy admission. Keeping the webhook aligned with
	// the CRD avoids a confusing "webhook says optional, apiserver rejects" gap.
	if spec.GuestClassRef.Name == "" {
		return fmt.Errorf("spec.guestClassRef.name is required")
	}
	validPolicies := map[swiftv1alpha1.RunPolicy]bool{
		swiftv1alpha1.RunPolicyRunning:          true,
		swiftv1alpha1.RunPolicyStopped:          true,
		swiftv1alpha1.RunPolicyRestartOnFailure: true,
		swiftv1alpha1.RunPolicyAlways:           true,
	}
	if spec.RunPolicy != "" && !validPolicies[spec.RunPolicy] {
		return fmt.Errorf("spec.runPolicy must be Running, Stopped, RestartOnFailure, or Always, got %q", spec.RunPolicy)
	}
	return nil
}
