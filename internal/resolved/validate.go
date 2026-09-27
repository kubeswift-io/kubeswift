package resolved

import (
	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/kubeswift-io/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// ValidateExistence checks that all required resources exist and SwiftImage is Ready.
// Called before merge for disk boot path. Returns ResolutionError on failure.
// A missing object or an image still importing is a wait (Waiting); only a
// Failed image fails the guest.
func ValidateExistence(
	guest *swiftv1alpha1.SwiftGuest,
	guestClass *swiftv1alpha1.SwiftGuestClass,
	image *imagev1alpha1.SwiftImage,
	seedProfile *seedv1alpha1.SwiftSeedProfile,
) *ResolutionError {
	if guestClass == nil {
		return &ResolutionError{Reason: "SwiftGuestClass not found", AffectedResource: guest.Spec.GuestClassRef.Name, Waiting: true}
	}
	if image == nil {
		imgName := ""
		if guest.Spec.ImageRef != nil {
			imgName = guest.Spec.ImageRef.Name
		}
		return &ResolutionError{Reason: "SwiftImage not found", AffectedResource: imgName, Waiting: true}
	}
	if image.Status.Phase == imagev1alpha1.SwiftImagePhaseFailed {
		return &ResolutionError{
			Reason:           withFailure("SwiftImage failed", image.Status.Conditions),
			AffectedResource: image.Name,
		}
	}
	if image.Status.Phase != imagev1alpha1.SwiftImagePhaseReady {
		// Only Failed is final. An image still importing becomes Ready by
		// itself, and a guest created alongside it waits for it.
		return &ResolutionError{
			Reason:           "SwiftImage not Ready",
			AffectedResource: image.Name,
			Waiting:          true,
		}
	}
	if guest.Spec.SeedProfileRef != nil {
		if seedProfile == nil {
			return &ResolutionError{Reason: "SwiftSeedProfile not found", AffectedResource: guest.Spec.SeedProfileRef.Name, Waiting: true}
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
