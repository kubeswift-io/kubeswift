package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
)

func makeSnap(backend snapshotv1alpha1.SnapshotBackendType) *snapshotv1alpha1.SwiftSnapshot {
	s := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "default"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"},
			Backend:  snapshotv1alpha1.SwiftSnapshotBackend{Type: backend},
		},
	}
	if backend == snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot {
		s.Spec.Backend.CSIVolumeSnapshot = &snapshotv1alpha1.CSIVolumeSnapshotBackend{}
	}
	if backend == snapshotv1alpha1.SnapshotBackendLocal {
		s.Spec.Backend.Local = &snapshotv1alpha1.LocalBackend{
			HostPath: "/var/lib/kubeswift/snapshots/default-snap1",
		}
	}
	return s
}

func TestValidate_CSIBackend_OK(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)); err != nil {
		t.Errorf("csi-volume-snapshot should be valid: %v", err)
	}
}

func TestValidate_LocalBackend_OK(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), makeSnap(snapshotv1alpha1.SnapshotBackendLocal)); err != nil {
		t.Errorf("local backend with valid hostPath should be valid: %v", err)
	}
}

func TestValidate_LocalBackend_RequiresLocalCarrier(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local = nil
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "spec.backend.local is required") {
		t.Errorf("expected backend.local required, got: %v", err)
	}
}

func TestValidate_LocalBackend_HostPathRequired(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local.HostPath = ""
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "hostPath is required") {
		t.Errorf("expected hostPath required, got: %v", err)
	}
}

func TestValidate_LocalBackend_HostPathInvalidPrefix(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local.HostPath = "/tmp/some-snapshot"
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "must be under /var/lib/kubeswift/snapshots/") {
		t.Errorf("expected prefix rejection, got: %v", err)
	}
}

func TestValidate_LocalBackend_HostPathParentTraversal(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local.HostPath = "/var/lib/kubeswift/snapshots/../etc"
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "must not contain '..'") {
		t.Errorf("expected parent-traversal rejection, got: %v", err)
	}
}

func TestValidate_LocalCarrier_OnNonLocalBackend_Rejected(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	snap.Spec.Backend.Local = &snapshotv1alpha1.LocalBackend{HostPath: "/var/lib/kubeswift/snapshots/default-snap1"}
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "spec.backend.local is only valid when") {
		t.Errorf("expected reserved-field rejection, got: %v", err)
	}
}

func TestValidate_S3Backend_Rejected(t *testing.T) {
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), makeSnap(snapshotv1alpha1.SnapshotBackendS3))
	if err == nil || !strings.Contains(err.Error(), "Phase 2") {
		t.Errorf("expected Phase 2 rejection of s3 backend, got: %v", err)
	}
}

func TestValidate_BackendTypeMissing_Rejected(t *testing.T) {
	snap := makeSnap("")
	snap.Spec.Backend.CSIVolumeSnapshot = nil
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "spec.backend.type is required") {
		t.Errorf("expected backend.type required, got: %v", err)
	}
}

func TestValidate_GuestRefRequired(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	snap.Spec.GuestRef.Name = ""
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "guestRef") {
		t.Errorf("expected guestRef rejection, got: %v", err)
	}
}

func TestValidate_S3CarrierForbidden(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	snap.Spec.Backend.S3 = &snapshotv1alpha1.S3Backend{Bucket: "x"}
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "backend.s3") {
		t.Errorf("expected reserved-field rejection, got: %v", err)
	}
}

func TestValidate_SpecImmutable(t *testing.T) {
	old := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	new := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	new.Spec.GuestRef.Name = "g2"
	v := &Validator{}
	_, err := v.ValidateUpdate(context.Background(), old, new)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("expected immutability rejection, got: %v", err)
	}
}

func TestValidate_LocalBackendSpecImmutable(t *testing.T) {
	old := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	new := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	new.Spec.Backend.Local.HostPath = "/var/lib/kubeswift/snapshots/different"
	v := &Validator{}
	_, err := v.ValidateUpdate(context.Background(), old, new)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("expected immutability rejection on local hostPath change, got: %v", err)
	}
}

func TestValidate_SpecUnchangedUpdateOK(t *testing.T) {
	old := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	new := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	v := &Validator{}
	if _, err := v.ValidateUpdate(context.Background(), old, new); err != nil {
		t.Errorf("identical-spec update should be allowed: %v", err)
	}
}
