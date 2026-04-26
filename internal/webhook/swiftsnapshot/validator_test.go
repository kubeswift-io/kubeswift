package swiftsnapshot

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
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

// -------- Memory-compat rules (require Client-backed Validator) --------

func newSchemeForMemoryTests(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift,
		&swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{},
	)
	metav1.AddToGroupVersion(s, gvSwift)
	return s
}

func validatorWithGuest(t *testing.T, guest *swiftv1alpha1.SwiftGuest) *Validator {
	t.Helper()
	scheme := newSchemeForMemoryTests(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest).Build()
	return &Validator{Client: c}
}

func makeSourceGuest(name, ns string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
	}
}

func makeMemoryCaptureSnap(guestName string) *snapshotv1alpha1.SwiftSnapshot {
	s := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	s.Spec.GuestRef.Name = guestName
	s.Spec.IncludeMemory = true
	return s
}

func TestValidate_MemoryCapture_GuestWithGPU_Rejected(t *testing.T) {
	guest := makeSourceGuest("g1", "default")
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "h200-shared"}

	v := validatorWithGuest(t, guest)
	snap := makeMemoryCaptureSnap("g1")
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil {
		t.Fatal("expected rejection for GPU + memory snapshot")
	}
	if !strings.Contains(err.Error(), "gpuProfileRef") {
		t.Errorf("error should reference gpuProfileRef; got %v", err)
	}
}

func TestValidate_MemoryCapture_GuestWithSRIOV_Rejected(t *testing.T) {
	guest := makeSourceGuest("g1", "default")
	guest.Spec.Interfaces = []swiftv1alpha1.GuestInterface{
		{Name: "rdma", Type: swiftv1alpha1.InterfaceTypeSRIOV},
	}

	v := validatorWithGuest(t, guest)
	snap := makeMemoryCaptureSnap("g1")
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil {
		t.Fatal("expected rejection for SR-IOV + memory snapshot")
	}
	if !strings.Contains(err.Error(), "SR-IOV") {
		t.Errorf("error should reference SR-IOV; got %v", err)
	}
}

func TestValidate_MemoryCapture_GuestWithQEMUOverride_Rejected(t *testing.T) {
	guest := makeSourceGuest("g1", "default")
	guest.Annotations = map[string]string{HypervisorOverrideAnnotation: "qemu"}

	v := validatorWithGuest(t, guest)
	snap := makeMemoryCaptureSnap("g1")
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil {
		t.Fatal("expected rejection for QEMU + memory snapshot")
	}
	if !strings.Contains(err.Error(), "QEMU") {
		t.Errorf("error should reference QEMU; got %v", err)
	}
}

func TestValidate_MemoryCapture_PlainGuest_OK(t *testing.T) {
	guest := makeSourceGuest("g1", "default")
	v := validatorWithGuest(t, guest)
	snap := makeMemoryCaptureSnap("g1")
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("plain guest should pass memory-snapshot rules: %v", err)
	}
}

func TestValidate_MemoryCapture_GuestNotYetExisting_DefersToController(t *testing.T) {
	// Webhook fail-open when source guest doesn't exist yet — operator
	// can apply SwiftSnapshot alongside SwiftGuest in the same kubectl
	// pass. The controller's pre-flight does the same check at
	// dispatch time with the guest fully resolved.
	v := &Validator{Client: fake.NewClientBuilder().WithScheme(newSchemeForMemoryTests(t)).Build()}
	snap := makeMemoryCaptureSnap("not-yet-created")
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("missing source guest should defer to controller, not reject: %v", err)
	}
}

func TestValidate_MemoryCapture_NoClient_SkipsLookup(t *testing.T) {
	// Validator without Client (defense-in-depth model: controller
	// re-checks). Memory-compat rules are skipped silently.
	v := &Validator{}
	snap := makeMemoryCaptureSnap("g1")
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("Validator without Client should skip memory-compat checks: %v", err)
	}
}

func TestValidate_DiskOnlyCapture_BypassesMemoryRules(t *testing.T) {
	// includeMemory=false (disk-only / CSI path) doesn't trigger the
	// VFIO/QEMU rules even when the guest has them.
	guest := makeSourceGuest("g1", "default")
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "h200"}

	v := validatorWithGuest(t, guest)
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	snap.Spec.GuestRef.Name = "g1"
	// Note: IncludeMemory left as default (false) for CSI snaps.
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("disk-only capture should bypass GPU rule: %v", err)
	}
}
