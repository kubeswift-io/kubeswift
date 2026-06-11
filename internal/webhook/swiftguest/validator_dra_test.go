package swiftguest

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// TestValidate_GPUResourceClaim covers the DRA backend admission rules: a
// well-formed gpuResourceClaim is accepted, and the mutual-exclusivity /
// one-of-claim-ref rules are enforced.
func TestValidate_GPUResourceClaim(t *testing.T) {
	// Valid: a single template reference.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimTemplateName: "gpu-tmpl", Tier: "pcie"}
	})); err != nil {
		t.Errorf("a single-template gpuResourceClaim should be valid: %v", err)
	}
	// Valid: a single shared-claim reference.
	if err := validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimName: "gpu-claim"}
	})); err != nil {
		t.Errorf("a single shared-claim gpuResourceClaim should be valid: %v", err)
	}

	// gpuProfileRef + gpuResourceClaim: rejected (two backends).
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "prof"}
		g.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimName: "c"}
	})), "mutually exclusive")

	// Neither claimName nor templateName: rejected.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{Tier: "pcie"}
	})), "exactly one of resourceClaimName or resourceClaimTemplateName")

	// Both claimName and templateName: rejected.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{
			ResourceClaimName: "c", ResourceClaimTemplateName: "t",
		}
	})), "exactly one of resourceClaimName or resourceClaimTemplateName")
}

// TestValidate_GPUResourceClaim_SharesGPUConstraints proves the DRA backend is
// guarded by the same v1 constraints as the native backend (usesGPU covers both).
func TestValidate_GPUResourceClaim_SharesGPUConstraints(t *testing.T) {
	withClaim := func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.GPUResourceClaim = &swiftv1alpha1.GPUResourceClaimSpec{ResourceClaimName: "c"}
	}

	// cloneFromSnapshot + DRA GPU: rejected.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.ImageRef = nil
		g.Spec.CloneFromSnapshot = &swiftv1alpha1.CloneFromSnapshotSource{SnapshotRef: corev1.LocalObjectReference{Name: "snap"}}
		withClaim(g)
	})), "mutually exclusive with GPU passthrough")

	// osType: windows + DRA GPU: rejected.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		g.Spec.OSType = swiftv1alpha1.OSTypeWindows
		withClaim(g)
	})), "GPU passthrough to Windows is out of scope")

	// virtiofs + DRA GPU: rejected.
	errContains(t, validateSwiftGuest(guest(func(g *swiftv1alpha1.SwiftGuest) {
		hp := "/srv"
		g.Spec.Filesystems = []swiftv1alpha1.Filesystem{
			{Name: "share", Source: swiftv1alpha1.FilesystemSource{HostPath: &hp}},
		}
		withClaim(g)
	})), "not supported with GPU passthrough")
}
