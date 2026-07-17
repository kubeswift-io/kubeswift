// Package swiftsandbox holds the SwiftSandbox admission validator.
package swiftsandbox

import (
	"context"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	sandboxv1alpha1 "github.com/kubeswift-io/kubeswift/api/sandbox/v1alpha1"
)

// Validator validates SwiftSandbox admission with per-operation discipline
// (PR #26 / Design Principle #10): ValidateCreate does full validation,
// ValidateUpdate is shape + immutability only, ValidateDelete is pass-through.
type Validator struct{}

var _ admission.CustomValidator = &Validator{}

func (v *Validator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	sb, ok := obj.(*sandboxv1alpha1.SwiftSandbox)
	if !ok {
		return nil, fmt.Errorf("expected a SwiftSandbox, got %T", obj)
	}
	return nil, validateSpec(&sb.Spec)
}

func (v *Validator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	newSb, ok := newObj.(*sandboxv1alpha1.SwiftSandbox)
	if !ok {
		return nil, fmt.Errorf("expected a SwiftSandbox, got %T", newObj)
	}
	// Deletion carve-out: a finalizer-removal / terminal update must never be
	// blocked by spec validation (the vswiftimage-finalizer-trap lesson).
	if newSb.GetDeletionTimestamp() != nil {
		return nil, nil
	}
	oldSb, ok := oldObj.(*sandboxv1alpha1.SwiftSandbox)
	if !ok {
		return nil, fmt.Errorf("expected a SwiftSandbox, got %T", oldObj)
	}
	// A sandbox is ephemeral: its launch-affecting spec is immutable once created
	// (recreate to change). spec.ttl is the exception — retention is adjustable.
	if launchSpecChanged(&oldSb.Spec, &newSb.Spec) {
		return nil, fmt.Errorf("SwiftSandbox spec is immutable except spec.ttl; recreate the sandbox to change image/resources/command/network")
	}
	return nil, validateSpec(&newSb.Spec)
}

func (v *Validator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSpec(s *sandboxv1alpha1.SwiftSandboxSpec) error {
	if s.Image == "" {
		return fmt.Errorf("spec.image is required")
	}
	if s.Timeout != nil && s.Timeout.Duration <= 0 {
		return fmt.Errorf("spec.timeout must be > 0 when set")
	}
	if s.TTL != nil && s.TTL.Duration <= 0 {
		return fmt.Errorf("spec.ttl must be > 0 when set")
	}
	if s.VerifyKeySecretRef != nil && s.VerifyKeySecretRef.Name == "" {
		return fmt.Errorf("spec.verifyKeySecretRef.name is required when verifyKeySecretRef is set")
	}
	// Exactly one GPU backend: gpuProfileRef (native) XOR gpuResourceClaim (DRA).
	if s.GPUProfileRef != nil && s.GPUResourceClaim != nil {
		return fmt.Errorf("spec.gpuProfileRef and spec.gpuResourceClaim are mutually exclusive: choose the native (gpuProfileRef) or DRA (gpuResourceClaim) GPU backend")
	}
	if rc := s.GPUResourceClaim; rc != nil {
		// Exactly one claim source (mirror the SwiftGuest DRA rule).
		hasName := rc.ResourceClaimName != ""
		hasTemplate := rc.ResourceClaimTemplateName != ""
		if hasName == hasTemplate {
			return fmt.Errorf("spec.gpuResourceClaim requires exactly one of resourceClaimName or resourceClaimTemplateName")
		}
		// A GPU sandbox boots cold — a warm pool cannot hold a scarce GPU idle.
		if s.PoolRef != nil {
			return fmt.Errorf("spec.gpuResourceClaim and spec.poolRef are mutually exclusive: a GPU sandbox boots cold (a warm pool cannot hold a GPU reservation)")
		}
	}
	if pr := s.GPUProfileRef; pr != nil {
		if pr.Name == "" {
			return fmt.Errorf("spec.gpuProfileRef.name is required when gpuProfileRef is set")
		}
		// Same cold-boot rule as the DRA backend: a warm pool cannot hold a GPU.
		if s.PoolRef != nil {
			return fmt.Errorf("spec.gpuProfileRef and spec.poolRef are mutually exclusive: a GPU sandbox boots cold (a warm pool cannot hold a GPU reservation)")
		}
	}
	return nil
}

// launchSpecChanged reports whether any launch-affecting field changed. It
// compares the full spec minus ttl via deep semantic equality (so a
// pointer-identity diff on Timeout/KernelProfileRef never false-fires — the
// vswiftimage pointer-comparison lesson).
func launchSpecChanged(oldS, newS *sandboxv1alpha1.SwiftSandboxSpec) bool {
	o := oldS.DeepCopy()
	n := newS.DeepCopy()
	o.TTL, n.TTL = nil, nil
	return !apiequality.Semantic.DeepEqual(o, n)
}
