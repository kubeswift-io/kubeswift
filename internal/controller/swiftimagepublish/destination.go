package swiftimagepublish

import (
	"context"
	"strings"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LocationExplicit marks a destination given as an explicit repository.
const LocationExplicit = "Explicit"

// imageRepository is where a location's images go: a cluster location's
// repository plus <namespace>/images/<image>, so each namespace has its own
// path, or a namespace location's plus images/<image>.
func imageRepository(res *storagelocation.Resolved, namespace, image string) string {
	repo := strings.TrimSuffix(res.Spec.OCI.Repository, "/")
	if res.Kind == storagev1alpha1.KindSwiftClusterStorageLocation {
		return repo + "/" + namespace + "/images/" + image
	}
	return repo + "/images/" + image
}

// resolveDestination resolves where pub pushes, once. An explicit repository
// is used as written. Otherwise the storage location (locationRef, else the
// namespace's default, else the cluster's) supplies the repository and every
// connection setting. Secret names are only ever read in pub's namespace.
func resolveDestination(ctx context.Context, c client.Reader, pub *imagev1alpha1.SwiftImagePublish) (*imagev1alpha1.ImagePublishDestinationStatus, *storagelocation.Wait, error) {
	d := &pub.Spec.Destination
	if d.Repository != "" {
		out := &imagev1alpha1.ImagePublishDestinationStatus{
			Location:   LocationExplicit,
			Repository: d.Repository,
			Insecure:   d.Insecure,
			CABundle:   d.CABundle,
		}
		if d.CredentialsSecretRef != nil {
			out.CredentialsSecretName = d.CredentialsSecretRef.Name
		}
		if d.SigningKeySecretRef != nil {
			out.SigningKeySecretName = d.SigningKeySecretRef.Name
		}
		if d.VerifyKeySecretRef != nil {
			out.VerifyKeySecretName = d.VerifyKeySecretRef.Name
		}
		return out, nil, nil
	}
	res, wait, err := storagelocation.Resolve(ctx, c, pub.Namespace, d.LocationRef, storagelocation.NeedOCI)
	if err != nil || wait != nil {
		return nil, wait, err
	}
	if res == nil {
		return nil, &storagelocation.Wait{
			Reason:  storagelocation.ReasonNone,
			Message: "no storage location with an oci registry applies: set destination.locationRef or destination.repository, or create a default SwiftStorageLocation or SwiftClusterStorageLocation",
		}, nil
	}
	o := res.Spec.OCI
	return &imagev1alpha1.ImagePublishDestinationStatus{
		Location:              res.Source(),
		Repository:            imageRepository(res, pub.Namespace, d.Image),
		Insecure:              o.Insecure,
		CABundle:              o.CABundle,
		CredentialsSecretName: storagelocation.CredentialsSecret(o),
		SigningKeySecretName:  o.SigningKeySecretName,
		VerifyKey:             o.VerifyKey,
	}, nil, nil
}

// checkSecrets makes the request wait while a Secret the destination names
// is missing from pub's namespace, before any Job exists.
func checkSecrets(ctx context.Context, c client.Reader, namespace string, d *imagev1alpha1.ImagePublishDestinationStatus) (*storagelocation.Wait, error) {
	return storagelocation.CheckSecrets(ctx, c, namespace, d.CredentialsSecretName, d.SigningKeySecretName)
}
