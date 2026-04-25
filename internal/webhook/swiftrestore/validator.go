// Package swiftrestore contains the admission webhook validator for
// SwiftRestore. It enforces same-namespace references and the
// snapshotRef/targetGuest contracts.
package swiftrestore

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

// Validator validates SwiftRestore resources.
type Validator struct{}

func (v *Validator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	r, ok := obj.(*snapshotv1alpha1.SwiftRestore)
	if !ok {
		return nil, fmt.Errorf("expected SwiftRestore, got %T", obj)
	}
	return nil, validateSwiftRestore(r)
}

func (v *Validator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	r, ok := newObj.(*snapshotv1alpha1.SwiftRestore)
	if !ok {
		return nil, fmt.Errorf("expected SwiftRestore, got %T", newObj)
	}
	oldR, ok := oldObj.(*snapshotv1alpha1.SwiftRestore)
	if !ok {
		return nil, fmt.Errorf("expected SwiftRestore, got %T", oldObj)
	}
	if err := validateSwiftRestore(r); err != nil {
		return nil, err
	}
	// Spec is immutable: restores are one-shot operations. Re-running with
	// different inputs creates a new SwiftRestore rather than mutating one
	// in flight.
	if oldR.Spec.SnapshotRef != r.Spec.SnapshotRef ||
		oldR.Spec.TargetGuest != r.Spec.TargetGuest ||
		oldR.Spec.ResumeAfterRestore != r.Spec.ResumeAfterRestore {
		return nil, fmt.Errorf("SwiftRestore spec is immutable")
	}
	return nil, nil
}

func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwiftRestore(r *snapshotv1alpha1.SwiftRestore) error {
	if r.Spec.SnapshotRef.Name == "" {
		return fmt.Errorf("spec.snapshotRef.name is required")
	}
	if r.Spec.TargetGuest.Name == "" {
		return fmt.Errorf("spec.targetGuest.name is required")
	}
	if r.Spec.SnapshotRef.Name == r.Spec.TargetGuest.Name {
		// Naming the target identically to the snapshot is almost always
		// an operator typo (snapshots and guests live in different
		// namespaces of the same cluster API surface, but reusing the
		// same name across kinds invites confusion in `kubectl get all`).
		return fmt.Errorf("spec.targetGuest.name must differ from spec.snapshotRef.name")
	}
	// Identity is reserved for Phase 2; reject non-empty regenerate lists
	// so operators don't think the field works yet.
	if r.Spec.Identity != nil && len(r.Spec.Identity.Regenerate) > 0 {
		return fmt.Errorf("spec.identity.regenerate is reserved for Phase 2 and must be empty")
	}
	return nil
}
