package swiftimage

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	imagev1alpha1 "github.com/projectbeskar/kubeswift/api/image/v1alpha1"
)

func makeImage(name string, strategy imagev1alpha1.CloneStrategy, vsClass string) *imagev1alpha1.SwiftImage {
	return &imagev1alpha1.SwiftImage{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: imagev1alpha1.SwiftImageSpec{
			Source: imagev1alpha1.ImageSource{
				HTTP: &imagev1alpha1.HTTPSource{URL: "https://example.com/img.qcow2"},
			},
			Format:                  imagev1alpha1.DiskFormatQcow2,
			CloneStrategy:           strategy,
			VolumeSnapshotClassName: vsClass,
		},
	}
}

func TestValidateCloneStrategy_Copy_OK(t *testing.T) {
	img := makeImage("ok", imagev1alpha1.CloneStrategyCopy, "")
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), img); err != nil {
		t.Errorf("copy strategy should be valid: %v", err)
	}
}

func TestValidateCloneStrategy_Default_TreatedAsCopy(t *testing.T) {
	img := makeImage("ok", "", "")
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), img); err != nil {
		t.Errorf("empty cloneStrategy (default copy) should be valid: %v", err)
	}
}

func TestValidateCloneStrategy_CopyWithSnapshotClass_Rejected(t *testing.T) {
	img := makeImage("bad", imagev1alpha1.CloneStrategyCopy, "csi-class")
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), img)
	if err == nil || !strings.Contains(err.Error(), "volumeSnapshotClassName") {
		t.Errorf("expected volumeSnapshotClassName rejection, got: %v", err)
	}
}

func TestValidateCloneStrategy_SnapshotWithoutClass_Rejected(t *testing.T) {
	img := makeImage("bad", imagev1alpha1.CloneStrategySnapshot, "")
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), img)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Errorf("expected snapshot-without-class rejection, got: %v", err)
	}
}

func TestValidateCloneStrategy_Snapshot_OK(t *testing.T) {
	img := makeImage("ok", imagev1alpha1.CloneStrategySnapshot, "csi-class")
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), img); err != nil {
		t.Errorf("snapshot strategy with class should be valid: %v", err)
	}
}

func TestValidateUpdate_CloneStrategyImmutableAfterImporting(t *testing.T) {
	old := makeImage("img", imagev1alpha1.CloneStrategyCopy, "")
	old.Status.Phase = imagev1alpha1.SwiftImagePhaseImporting

	new := makeImage("img", imagev1alpha1.CloneStrategySnapshot, "csi-class")
	new.Status.Phase = imagev1alpha1.SwiftImagePhaseImporting

	v := &Validator{}
	_, err := v.ValidateUpdate(context.Background(), old, new)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("expected immutability rejection, got: %v", err)
	}
}

func TestValidateUpdate_CloneStrategyMutableInPending(t *testing.T) {
	old := makeImage("img", imagev1alpha1.CloneStrategyCopy, "")
	old.Status.Phase = imagev1alpha1.SwiftImagePhasePending

	new := makeImage("img", imagev1alpha1.CloneStrategySnapshot, "csi-class")
	new.Status.Phase = imagev1alpha1.SwiftImagePhasePending

	v := &Validator{}
	if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
		t.Errorf("changing strategy in Pending should be allowed: %v", err)
	}
}

// TestValidateUpdate_FinalizerRemovalOnDeletingReadyImage_Allowed is the
// TFU-17 contract test. A Ready, snapshot-strategy SwiftImage that carries
// CloneSeedFinalizer and is being deleted (deletionTimestamp set) must be
// allowed to shed its finalizer via UPDATE — otherwise GC can never reap it
// and the namespace stays Terminating forever.
//
// old and new are built via separate makeImage calls, so their Spec.Source
// pointers differ exactly as in a real admission request (etcd decode vs.
// request decode). Before the deletionTimestamp carve-out this UPDATE was
// rejected by the spec-immutability rule's pointer-identity comparison.
func TestValidateUpdate_FinalizerRemovalOnDeletingReadyImage_Allowed(t *testing.T) {
	now := metav1.Now()

	old := makeImage("img", imagev1alpha1.CloneStrategySnapshot, "csi-class")
	old.Status.Phase = imagev1alpha1.SwiftImagePhaseReady
	old.DeletionTimestamp = &now
	old.Finalizers = []string{"kubeswift.io/clone-seed-protected"}

	// new is the post-removal object: same Ready spec, still being deleted,
	// finalizer dropped.
	new := makeImage("img", imagev1alpha1.CloneStrategySnapshot, "csi-class")
	new.Status.Phase = imagev1alpha1.SwiftImagePhaseReady
	new.DeletionTimestamp = &now
	new.Finalizers = nil

	v := &Validator{}
	if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
		t.Errorf("finalizer removal on a being-deleted Ready image must be allowed: %v", err)
	}
}

// TestValidateUpdate_SpecMutationOnReadyImage_StillRejected guards against
// over-allowing: the deletionTimestamp carve-out must NOT weaken the
// immutability rule for objects that are NOT being deleted. A genuine spec
// change on a Ready image (not being deleted) must still be rejected.
func TestValidateUpdate_SpecMutationOnReadyImage_StillRejected(t *testing.T) {
	old := makeImage("img", imagev1alpha1.CloneStrategyCopy, "")
	old.Status.Phase = imagev1alpha1.SwiftImagePhaseReady

	new := makeImage("img", imagev1alpha1.CloneStrategyCopy, "")
	new.Status.Phase = imagev1alpha1.SwiftImagePhaseReady
	new.Spec.Source.HTTP.URL = "https://example.com/different.qcow2"

	v := &Validator{}
	_, err := v.ValidateUpdate(context.Background(), old, new)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("spec mutation on a Ready image (not being deleted) must still be rejected, got: %v", err)
	}
}

// TestValidateUpdate_MetadataEditOnReadyImage_Allowed is the TFU #23 contract:
// a metadata-only edit (e.g. adding a label) on a Ready image that is NOT being
// deleted must be ALLOWED. old and new come from separate makeImage calls, so
// their Spec.Source pointers differ while the content is identical — the old
// `!=` pointer compare falsely rejected this; the DeepEqual fix allows it.
func TestValidateUpdate_MetadataEditOnReadyImage_Allowed(t *testing.T) {
	old := makeImage("img", imagev1alpha1.CloneStrategyCopy, "")
	old.Status.Phase = imagev1alpha1.SwiftImagePhaseReady

	new := makeImage("img", imagev1alpha1.CloneStrategyCopy, "")
	new.Status.Phase = imagev1alpha1.SwiftImagePhaseReady
	new.Labels = map[string]string{"team": "platform"} // metadata-only change

	v := &Validator{}
	if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
		t.Errorf("metadata-only edit on a Ready image must be allowed (TFU #23); got: %v", err)
	}
}
