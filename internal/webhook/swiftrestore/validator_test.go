package swiftrestore

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

func makeRestore(snapName, target string) *snapshotv1alpha1.SwiftRestore {
	return &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default"},
		Spec: snapshotv1alpha1.SwiftRestoreSpec{
			SnapshotRef: snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: snapName},
			TargetGuest: snapshotv1alpha1.SwiftRestoreTarget{Name: target},
		},
	}
}

func TestValidate_OK(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), makeRestore("snap1", "target")); err != nil {
		t.Errorf("valid restore rejected: %v", err)
	}
}

func TestValidate_SnapshotRef_Required(t *testing.T) {
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), makeRestore("", "target"))
	if err == nil || !strings.Contains(err.Error(), "snapshotRef") {
		t.Errorf("expected snapshotRef rejection, got: %v", err)
	}
}

func TestValidate_TargetGuest_Required(t *testing.T) {
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), makeRestore("snap1", ""))
	if err == nil || !strings.Contains(err.Error(), "targetGuest") {
		t.Errorf("expected targetGuest rejection, got: %v", err)
	}
}

func TestValidate_SameNameAsSnapshot_Rejected(t *testing.T) {
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), makeRestore("same", "same"))
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Errorf("expected name collision rejection, got: %v", err)
	}
}

func TestValidate_IdentityRegenerate_Phase2Reserved(t *testing.T) {
	r := makeRestore("snap1", "target")
	r.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{snapshotv1alpha1.RegenHostname},
	}
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "Phase 2") {
		t.Errorf("expected Phase 2 rejection of identity.regenerate, got: %v", err)
	}
}

func TestValidate_SpecImmutable(t *testing.T) {
	old := makeRestore("snap1", "target")
	new := makeRestore("snap2", "target")
	v := &Validator{}
	_, err := v.ValidateUpdate(context.Background(), old, new)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("expected immutability rejection, got: %v", err)
	}
}

func TestValidate_SpecUnchangedUpdateOK(t *testing.T) {
	old := makeRestore("snap1", "target")
	new := makeRestore("snap1", "target")
	v := &Validator{}
	if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
		t.Errorf("identical-spec update should be allowed: %v", err)
	}
}
