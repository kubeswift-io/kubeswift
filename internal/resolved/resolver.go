package resolved

import (
	"context"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	seedv1alpha1 "github.com/projectbeskar/kubeswift/api/seed/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Resolver resolves SwiftGuest to ResolvedGuest by fetching and merging referenced resources.
type Resolver interface {
	Resolve(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*ResolvedGuest, error)
}

// NewResolver returns a Resolver that uses the given client.
func NewResolver(c client.Client) Resolver {
	return &resolver{client: c}
}

type resolver struct {
	client client.Client
}

// Resolve fetches referenced resources, validates, merges, and returns ResolvedGuest or ResolutionError.
func (r *resolver) Resolve(ctx context.Context, guest *swiftv1alpha1.SwiftGuest) (*ResolvedGuest, error) {
	// Fetch GuestClass (cluster-scoped)
	guestClass := &swiftv1alpha1.SwiftGuestClass{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: guest.Spec.GuestClassRef.Name}, guestClass); err != nil {
		return nil, &ResolutionError{Reason: "SwiftGuestClass not found: " + err.Error(), AffectedResource: guest.Spec.GuestClassRef.Name}
	}

	// Fetch Image (namespaced)
	image := &imagev1alpha1.SwiftImage{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: guest.Spec.ImageRef.Name}, image); err != nil {
		return nil, &ResolutionError{Reason: "SwiftImage not found: " + err.Error(), AffectedResource: guest.Spec.ImageRef.Name}
	}

	// Fetch SeedProfile if referenced
	var seedProfile *seedv1alpha1.SwiftSeedProfile
	if guest.Spec.SeedProfileRef != nil {
		sp := &seedv1alpha1.SwiftSeedProfile{}
		if err := r.client.Get(ctx, types.NamespacedName{Namespace: guest.Namespace, Name: guest.Spec.SeedProfileRef.Name}, sp); err != nil {
			return nil, &ResolutionError{Reason: "SwiftSeedProfile not found: " + err.Error(), AffectedResource: guest.Spec.SeedProfileRef.Name}
		}
		seedProfile = sp
	}

	// Validate existence
	if err := ValidateExistence(guest, guestClass, image, seedProfile); err != nil {
		return nil, err
	}

	// Merge
	rg := Merge(guest, guestClass, image, seedProfile)

	// Cross-object validation
	if err := ValidateCompatibility(rg); err != nil {
		return nil, err
	}

	return rg, nil
}
