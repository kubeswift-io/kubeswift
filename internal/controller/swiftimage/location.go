// Connection settings for a SwiftImage oci source (#703).
//
// An image names what to pull, so source.oci.repository stays required. What a
// storage location contributes is how to connect: the credentials Secret name,
// insecure, a CA bundle and a verify key. They are resolved once, before the
// import starts, and recorded in status.source; the import Job is built from
// that record.
package swiftimage

import (
	"context"
	"fmt"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// explicitConnection reports whether an oci source sets any connection field:
// then they are used as written, all or nothing.
func explicitConnection(o *imagev1alpha1.OCIImageSource) bool {
	return o.Insecure || o.CABundle != "" ||
		(o.CredentialsSecretRef != nil && o.CredentialsSecretRef.Name != "") ||
		(o.VerifyKeySecretRef != nil && o.VerifyKeySecretRef.Name != "")
}

// resolveImageSource works out what status.source records for an oci source:
//
//  1. Connection fields set on the source: used as written (Explicit).
//  2. Else a storage location: locationRef, else the namespace default, else
//     the cluster default. Its settings apply only if its registry host is
//     the repository's; a default for another registry applies nothing (None,
//     an anonymous pull as before), a locationRef for another registry waits.
//  3. No location: None.
//
// A location whose credentials Secret is missing in the image's namespace
// makes the image wait, naming it.
func (r *SwiftImageReconciler) resolveImageSource(ctx context.Context, img *imagev1alpha1.SwiftImage) (*imagev1alpha1.ImageSourceStatus, *storagelocation.Wait, error) {
	o := img.Spec.Source.OCI
	if explicitConnection(o) {
		src := &imagev1alpha1.ImageSourceStatus{
			Location: imagev1alpha1.ImageSourceLocationExplicit,
			Insecure: o.Insecure,
			CABundle: o.CABundle,
		}
		if o.CredentialsSecretRef != nil {
			src.CredentialsSecretName = o.CredentialsSecretRef.Name
		}
		if o.VerifyKeySecretRef != nil {
			src.VerifyKeySecretName = o.VerifyKeySecretRef.Name
		}
		return src, nil, nil
	}

	res, wait, err := storagelocation.Resolve(ctx, r.Client, img.Namespace, o.LocationRef, storagelocation.NeedOCI)
	if err != nil || wait != nil {
		return nil, wait, err
	}
	if res == nil {
		return &imagev1alpha1.ImageSourceStatus{Location: imagev1alpha1.ImageSourceLocationNone}, nil, nil
	}
	imageHost, err := storagelocation.Registry(o.Repository)
	if err != nil {
		return nil, &storagelocation.Wait{Reason: storagelocation.ReasonInvalid, Message: err.Error()}, nil
	}
	loc := res.Spec.OCI
	locHost, err := storagelocation.Registry(loc.Repository)
	if err != nil {
		return nil, &storagelocation.Wait{Reason: storagelocation.ReasonInvalid, Message: fmt.Sprintf("%s: %v", res.Source(), err)}, nil
	}
	if locHost != imageHost {
		if o.LocationRef != nil {
			return nil, &storagelocation.Wait{Reason: storagelocation.ReasonInvalid, Message: fmt.Sprintf(
				"source.oci.locationRef names %s, which is for registry %s; the image is on %s", res.Source(), locHost, imageHost)}, nil
		}
		return &imagev1alpha1.ImageSourceStatus{Location: imagev1alpha1.ImageSourceLocationNone}, nil, nil
	}
	creds := storagelocation.CredentialsSecret(loc)
	if w, err := storagelocation.CheckSecrets(ctx, r.Client, img.Namespace, creds, ""); err != nil || w != nil {
		return nil, w, err
	}
	return &imagev1alpha1.ImageSourceStatus{
		Location:              res.Source(),
		Insecure:              loc.Insecure,
		CABundle:              loc.CABundle,
		CredentialsSecretName: creds,
		VerifyKey:             loc.VerifyKey,
	}, nil, nil
}
