package swiftimagepublish

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

// AnnotationPublishedBy is on a SwiftImage createImage made: the UID of the
// SwiftImagePublish. It is how a re-run recognises its own image; a
// SwiftImage of that name without it is somebody else's.
const AnnotationPublishedBy = "image.kubeswift.io/published-by"

// ReasonImageExists: createImage names a SwiftImage this request did not make.
const ReasonImageExists = "ImageExists"

const gib = int64(1) << 30

func mustQuantity(s string) resource.Quantity { return resource.MustParse(s) }

// imageFor is the SwiftImage createImage makes: it pulls the pushed digest,
// never the tag, with the destination's connection settings (the location
// itself, or the explicit fields), and its root disk is at least the image's
// size. It has no owner: deleting the request leaves it.
func imageFor(pub *imagev1alpha1.SwiftImagePublish) *imagev1alpha1.SwiftImage {
	dst := pub.Status.Destination
	src := &imagev1alpha1.OCIImageSource{
		Repository: dst.Repository,
		Digest:     pub.Status.Digest,
	}
	if dst.Location == LocationExplicit {
		src.Insecure = dst.Insecure
		src.CABundle = dst.CABundle
		if dst.CredentialsSecretName != "" {
			src.CredentialsSecretRef = &imagev1alpha1.SecretObjectReference{Name: dst.CredentialsSecretName}
		}
		if dst.VerifyKeySecretName != "" {
			src.VerifyKeySecretRef = &imagev1alpha1.SecretObjectReference{Name: dst.VerifyKeySecretName}
		}
	} else if kind, name, ok := cutLocation(dst.Location); ok {
		// The location applies to the image as it did to the push (same
		// registry), including its verify key.
		src.LocationRef = &storagev1alpha1.StorageLocationRef{Kind: kind, Name: name}
	}
	size := ((pub.Status.SizeBytes + gib - 1) / gib) * gib
	return &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pub.Spec.CreateImage.Name,
			Namespace:   pub.Namespace,
			Labels:      map[string]string{LabelPublish: pub.Name},
			Annotations: map[string]string{AnnotationPublishedBy: string(pub.UID)},
		},
		Spec: imagev1alpha1.SwiftImageSpec{
			Source:   imagev1alpha1.ImageSource{OCI: src},
			Format:   imagev1alpha1.DiskFormatRaw,
			OSType:   pub.Status.Source.OSType,
			RootDisk: &imagev1alpha1.SwiftImageRootDiskSpec{Size: resource.NewQuantity(size, resource.BinarySI)},
		},
	}
}

func cutLocation(s string) (kind, name string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], s[:i] != "" && s[i+1:] != ""
		}
	}
	return "", "", false
}

// ensureImage creates the SwiftImage createImage asks for. It returns a
// failure condition when the name is taken by an image this request did not
// make.
func ensureImage(ctx context.Context, c client.Client, pub *imagev1alpha1.SwiftImagePublish) (*condition, error) {
	want := imageFor(pub)
	var got imagev1alpha1.SwiftImage
	err := c.Get(ctx, client.ObjectKeyFromObject(want), &got)
	switch {
	case err == nil:
		if got.Annotations[AnnotationPublishedBy] == string(pub.UID) {
			return nil, nil
		}
		return &condition{ReasonImageExists, "SwiftImage " + want.Name + " already exists and was not created by this request; the image was published as " +
			pub.Status.Reference + "@" + pub.Status.Digest}, nil
	case apierrors.IsNotFound(err):
		// AlreadyExists (a race) is returned: the next pass reads whose it is.
		return nil, c.Create(ctx, want)
	default:
		return nil, err
	}
}
