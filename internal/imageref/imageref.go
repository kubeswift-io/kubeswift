// Package imageref centralises the lookup that both the SwiftImage and
// SwiftGuest controllers need: "give me the SwiftGuests in this namespace
// that reference this SwiftImage". The SwiftImage controller uses it for
// finalizer removal (only delete the clone seed once no SwiftGuests
// reference the image); the SwiftGuest controller uses it to enqueue
// reconciles when a SwiftImage changes.
//
// This is namespace-scoped — same-namespace constraint per Phase 0 §6a.
package imageref

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// ListGuestsReferencingImage returns the SwiftGuests in the SwiftImage's
// namespace whose spec.imageRef.name matches the given image's name.
func ListGuestsReferencingImage(ctx context.Context, c client.Client, img *imagev1alpha1.SwiftImage) ([]swiftv1alpha1.SwiftGuest, error) {
	var list swiftv1alpha1.SwiftGuestList
	if err := c.List(ctx, &list, client.InNamespace(img.Namespace)); err != nil {
		return nil, err
	}
	var out []swiftv1alpha1.SwiftGuest
	for i := range list.Items {
		g := &list.Items[i]
		if g.Spec.ImageRef != nil && g.Spec.ImageRef.Name == img.Name {
			out = append(out, *g)
		}
	}
	return out, nil
}
