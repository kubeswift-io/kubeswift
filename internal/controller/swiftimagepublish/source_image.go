package swiftimagepublish

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/storagelocation"
)

// resolveImageSource looks at a SwiftImage source. One pulled by digest is
// copied from that digest with the connection it was pulled with
// (status.source); any other is read from its prepared PVC.
func resolveImageSource(ctx context.Context, c client.Reader, pub *imagev1alpha1.SwiftImagePublish) (sourceResult, error) {
	name := pub.Spec.Source.ImageRef.Name
	var img imagev1alpha1.SwiftImage
	if err := c.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: name}, &img); err != nil {
		if apierrors.IsNotFound(err) {
			return sourceResult{wait: &condition{ReasonSourceNotFound, "SwiftImage " + name + " does not exist"}}, nil
		}
		return sourceResult{}, err
	}
	switch img.Status.Phase {
	case imagev1alpha1.SwiftImagePhaseReady:
	case imagev1alpha1.SwiftImagePhaseFailed:
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftImage " + name + " failed"}}, nil
	default:
		return sourceResult{wait: &condition{ReasonSourceNotReady, "SwiftImage " + name + " is not Ready yet"}}, nil
	}
	osType := pub.Spec.OSType
	if osType == "" {
		osType = img.Spec.OSType
	}
	if osType == "" {
		osType = imagev1alpha1.OSTypeLinux
	}
	st := &imagev1alpha1.ImagePublishSourceStatus{
		Kind:   "SwiftImage",
		Name:   img.Name,
		UID:    string(img.UID),
		OSType: osType,
	}

	if o := img.Spec.Source.OCI; o != nil && o.Digest != "" && img.Status.Source != nil {
		cp := &imagev1alpha1.ImagePublishCopySource{
			Repository:            o.Repository,
			Digest:                o.Digest,
			Insecure:              img.Status.Source.Insecure,
			CABundle:              img.Status.Source.CABundle,
			CredentialsSecretName: img.Status.Source.CredentialsSecretName,
			VerifyKey:             img.Status.Source.VerifyKey,
			VerifyKeySecretName:   img.Status.Source.VerifyKeySecretName,
		}
		if cp.CredentialsSecretName != "" {
			w, err := storagelocation.CheckSecrets(ctx, c, pub.Namespace, cp.CredentialsSecretName, "")
			if err != nil {
				return sourceResult{}, err
			}
			if w != nil {
				return sourceResult{wait: &condition{w.Reason, "source " + w.Message}}, nil
			}
		}
		if img.Status.PreparedArtifact != nil && img.Status.PreparedArtifact.Size != nil {
			st.SizeBytes = img.Status.PreparedArtifact.Size.Value()
		}
		st.CopyFrom = cp
		return sourceResult{status: st}, nil
	}

	pa := img.Status.PreparedArtifact
	if pa == nil || pa.PVCRef == nil || pa.PVCRef.Name == "" {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftImage " + name + " records no prepared PVC"}}, nil
	}
	if pa.PVCRef.Namespace != "" && pa.PVCRef.Namespace != pub.Namespace {
		return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftImage " + name + "'s PVC is in another namespace"}}, nil
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: pub.Namespace, Name: pa.PVCRef.Name}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return sourceResult{fail: &condition{ReasonSourceUnsupported, "SwiftImage " + name + "'s PVC " + pa.PVCRef.Name + " does not exist"}}, nil
		}
		return sourceResult{}, err
	}
	st.PVCName = pvc.Name
	st.Block = pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock
	if pa.Size != nil {
		st.SizeBytes = pa.Size.Value()
	} else {
		st.SizeBytes = pvc.Status.Capacity.Storage().Value()
	}
	return sourceResult{status: st}, nil
}
