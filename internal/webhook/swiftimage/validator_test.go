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
