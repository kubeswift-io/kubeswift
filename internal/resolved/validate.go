package resolved

import (
	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// ValidateExistence checks that all required resources exist and SwiftImage is Ready.
// Called before merge. Returns ResolutionError on failure.
func ValidateExistence(
	guest *swiftv1alpha1.SwiftGuest,
	guestClass *swiftv1alpha1.SwiftGuestClass,
	image *imagev1alpha1.SwiftImage,
	seedProfile *seedv1alpha1.SwiftSeedProfile,
) *ResolutionError {
	if guestClass == nil {
		return &ResolutionError{Reason: "SwiftGuestClass not found", AffectedResource: guest.Spec.GuestClassRef.Name}
	}
	if image == nil {
		return &ResolutionError{Reason: "SwiftImage not found", AffectedResource: guest.Spec.ImageRef.Name}
	}
	if image.Status.Phase != imagev1alpha1.SwiftImagePhaseReady {
		return &ResolutionError{Reason: "SwiftImage not Ready", AffectedResource: guest.Spec.ImageRef.Name}
	}
	if guest.Spec.SeedProfileRef != nil {
		if seedProfile == nil {
			return &ResolutionError{Reason: "SwiftSeedProfile not found", AffectedResource: guest.Spec.SeedProfileRef.Name}
		}
	}
	return nil
}

// ValidateCompatibility checks cross-object compatibility after merge.
// MVP: root disk format compatible with image format.
func ValidateCompatibility(rg *ResolvedGuest) *ResolutionError {
	// Format compatibility: root disk format must match or be compatible with image format
	imgFormat := rg.PreparedImage.Format
	diskFormat := rg.RootDisk.Format
	if imgFormat != "" && diskFormat != "" && imgFormat != diskFormat {
		// For MVP: require exact match. Conversion could be added later.
		return &ResolutionError{Reason: "root disk format incompatible with image format"}
	}
	return nil
}
