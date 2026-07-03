package swiftimage

import (
	"context"
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	imagev1alpha1 "github.com/kubeswift-io/kubeswift/api/image/v1alpha1"
)

// Validator validates SwiftImage resources.
type Validator struct{}

func (v *Validator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	img, ok := obj.(*imagev1alpha1.SwiftImage)
	if !ok {
		return nil, fmt.Errorf("expected SwiftImage, got %T", obj)
	}
	return nil, validateSwiftImage(img)
}

func (v *Validator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	img, ok := newObj.(*imagev1alpha1.SwiftImage)
	if !ok {
		return nil, fmt.Errorf("expected SwiftImage, got %T", newObj)
	}
	oldImg, ok := oldObj.(*imagev1alpha1.SwiftImage)
	if !ok {
		return nil, fmt.Errorf("expected SwiftImage, got %T", oldObj)
	}
	// Per-operation discipline (Design Principle #10; same lesson as PR #26).
	// A being-deleted object is shedding finalizers so GC can proceed;
	// finalizer removal is a metadata-only UPDATE that is never a spec
	// mutation. The spec-immutability rule must not block it, or the
	// CloneSeedFinalizer can never be removed and the namespace stays
	// Terminating forever (TFU-17).
	//
	// Note on why the rule fired here at all: validateSwiftImageUpdate
	// compares Spec.Source with `!=`, which is a pointer-identity
	// comparison over ImageSource's pointer fields (HTTP/Upload/PVCClone).
	// The old (etcd) and new (admission request) objects are independent
	// decodes, so their Source pointers always differ — the immutability
	// rule fires on EVERY update of a Ready image, including finalizer
	// removal. This guard is the carve-out; it does not depend on the
	// comparison being fixed.
	if img.GetDeletionTimestamp() != nil {
		return nil, nil
	}
	return nil, validateSwiftImageUpdate(oldImg, img)
}

func (v *Validator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateSwiftImage(img *imagev1alpha1.SwiftImage) error {
	src := &img.Spec.Source
	n := 0
	if src.HTTP != nil {
		n++
		if src.HTTP.URL == "" {
			return fmt.Errorf("spec.source.http.url is required when http source is specified")
		}
	}
	if src.PVCClone != nil {
		n++
		if src.PVCClone.Name == "" {
			return fmt.Errorf("spec.source.pvcClone.name is required when pvcClone source is specified")
		}
	}
	if src.Upload != nil {
		n++
	}
	if src.OCI != nil {
		n++
		if src.OCI.Repository == "" {
			return fmt.Errorf("spec.source.oci.repository is required when oci source is specified")
		}
		if src.OCI.Tag != "" && src.OCI.Digest != "" {
			return fmt.Errorf("spec.source.oci: only one of tag or digest may be specified")
		}
		if src.OCI.CredentialsSecretRef != nil && src.OCI.CredentialsSecretRef.Name == "" {
			return fmt.Errorf("spec.source.oci.credentialsSecretRef.name is required when credentialsSecretRef is set")
		}
		if src.OCI.VerifyKeySecretRef != nil {
			if src.OCI.VerifyKeySecretRef.Name == "" {
				return fmt.Errorf("spec.source.oci.verifyKeySecretRef.name is required when verifyKeySecretRef is set")
			}
			// cosign verify speaks HTTPS only and does not honor --allow-http-registry
			// on the registry ping, so signature verification over a plaintext
			// registry can never succeed — reject the combination at admission
			// rather than let the import fail opaquely.
			if src.OCI.Insecure {
				return fmt.Errorf("spec.source.oci: verifyKeySecretRef requires a TLS registry; cosign verify does not support insecure (plaintext http)")
			}
		}
	}
	if n == 0 {
		return fmt.Errorf("spec.source: exactly one of http, pvcClone, upload, or oci must be specified")
	}
	if n > 1 {
		return fmt.Errorf("spec.source: only one of http, pvcClone, upload, or oci may be specified")
	}
	if img.Spec.Format == "" {
		return fmt.Errorf("spec.format is required (raw or qcow2)")
	}
	// osType enum (defense-in-depth; the CRD schema also enforces it).
	switch img.Spec.OSType {
	case "", imagev1alpha1.OSTypeLinux, imagev1alpha1.OSTypeWindows:
	default:
		return fmt.Errorf("spec.osType must be linux or windows, got %q", img.Spec.OSType)
	}
	if err := validateCloneStrategy(img); err != nil {
		return err
	}
	return nil
}

// validateCloneStrategy enforces the rules around cloneStrategy /
// volumeSnapshotClassName / cloneStorageClassName described in
// docs/images/clone-strategies.md.
//
//   - cloneStrategy "" (default) is treated as "copy" and accepts no
//     snapshot fields.
//   - cloneStrategy "snapshot" requires volumeSnapshotClassName.
//   - volumeSnapshotClassName is meaningful only with cloneStrategy=snapshot.
func validateCloneStrategy(img *imagev1alpha1.SwiftImage) error {
	strategy := img.Spec.CloneStrategy
	switch strategy {
	case "", imagev1alpha1.CloneStrategyCopy:
		if img.Spec.VolumeSnapshotClassName != "" {
			return fmt.Errorf("spec.volumeSnapshotClassName is only valid when spec.cloneStrategy is 'snapshot'")
		}
	case imagev1alpha1.CloneStrategySnapshot:
		if img.Spec.VolumeSnapshotClassName == "" {
			return fmt.Errorf("spec.volumeSnapshotClassName is required when spec.cloneStrategy is 'snapshot'")
		}
	default:
		return fmt.Errorf("spec.cloneStrategy: unsupported value %q (allowed: copy, snapshot)", strategy)
	}
	return nil
}

func validateSwiftImageUpdate(oldImg, img *imagev1alpha1.SwiftImage) error {
	if err := validateSwiftImage(img); err != nil {
		return err
	}
	if oldImg.Status.Phase == imagev1alpha1.SwiftImagePhaseReady {
		// Source is compared by CONTENT, not pointer identity (TFU #23). The old
		// (etcd) and new (admission request) objects are independent decodes, so
		// their Source pointers always differ; a `!=` pointer compare fired on
		// EVERY update of a Ready image — including innocuous metadata edits
		// (label/annotation) where the spec content is unchanged. DeepEqual
		// compares the dereferenced HTTP/Upload/PVCClone content. Format/OSType
		// are strings (value compare, no foot-gun).
		if !apiequality.Semantic.DeepEqual(oldImg.Spec.Source, img.Spec.Source) ||
			oldImg.Spec.Format != img.Spec.Format ||
			oldImg.Spec.OSType != img.Spec.OSType {
			return fmt.Errorf("SwiftImage spec is immutable when status.phase is Ready")
		}
	}
	// CloneStrategy is immutable after import has progressed past Pending.
	// Switching strategies on a partially imported image leaves the prepared
	// PVC in an ambiguous state (no clone seed for snapshot path, no
	// guarantee of size match for copy path).
	if oldImg.Spec.CloneStrategy != img.Spec.CloneStrategy &&
		oldImg.Status.Phase != "" &&
		oldImg.Status.Phase != imagev1alpha1.SwiftImagePhasePending {
		return fmt.Errorf("spec.cloneStrategy is immutable once import has started (current phase=%s)", oldImg.Status.Phase)
	}
	return nil
}
