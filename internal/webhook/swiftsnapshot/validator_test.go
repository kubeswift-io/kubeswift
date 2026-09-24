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

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
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
			HostPath: "/var/lib/kubeswift/snapshots/default_snap1",
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

// TestValidate_DeletionPolicyWarning: Retain on a CSI snapshot warns (it's
// ignored there); Delete on CSI and Retain on local/s3 do not warn.
func TestValidate_DeletionPolicyWarning(t *testing.T) {
	v := &Validator{}
	csiRetain := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	csiRetain.Spec.DeletionPolicy = snapshotv1alpha1.SnapshotDeletionPolicyRetain
	w, err := v.ValidateCreate(context.Background(), csiRetain)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) == 0 || !strings.Contains(w[0], "ignored for csi-volume-snapshot") {
		t.Errorf("CSI + Retain should warn; got %v", w)
	}

	csiDelete := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	csiDelete.Spec.DeletionPolicy = snapshotv1alpha1.SnapshotDeletionPolicyDelete
	if w, _ := v.ValidateCreate(context.Background(), csiDelete); len(w) != 0 {
		t.Errorf("CSI + Delete (the no-op default) should NOT warn; got %v", w)
	}

	localRetain := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	localRetain.Spec.IncludeMemory = true // production default; avoid the includeMemory no-op warning
	localRetain.Spec.DeletionPolicy = snapshotv1alpha1.SnapshotDeletionPolicyRetain
	if w, _ := v.ValidateCreate(context.Background(), localRetain); len(w) != 0 {
		t.Errorf("local + Retain (honored) must NOT warn; got %v", w)
	}
}

func TestValidate_LocalBackend_OK(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), makeSnap(snapshotv1alpha1.SnapshotBackendLocal)); err != nil {
		t.Errorf("local backend with valid hostPath should be valid: %v", err)
	}
}

// The directory is derived, so neither backend.local nor its hostPath is
// needed.
func TestValidate_LocalBackend_LocalCarrierOptional(t *testing.T) {
	v := &Validator{}
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local = nil
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("backend.local omitted: %v", err)
	}
	snap = makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local.HostPath = ""
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("hostPath omitted: %v", err)
	}
}

func TestValidate_LocalBackend_HostPathInvalidPrefix(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local.HostPath = "/tmp/some-snapshot"
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil || !strings.Contains(err.Error(), "must be omitted or be /var/lib/kubeswift/snapshots/default_snap1") {
		t.Errorf("expected rejection naming the derived dir, got: %v", err)
	}
}

func TestValidate_LocalBackend_HostPathParentTraversal(t *testing.T) {
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.Backend.Local.HostPath = "/var/lib/kubeswift/snapshots/../etc"
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), snap)
	if err == nil {
		t.Errorf("expected parent-traversal rejection, got nil")
	}
}

// A hostPath is accepted only as the directory derived for the snapshot. Any
// other name -- another tenant's directory, the old "<ns>-<name>" form, a
// directory shared by several snapshots -- let a capture empty, or a delete
// remove, a directory the snapshot did not own.
func TestValidateLocalHostPath_OnlyTheDerivedDir(t *testing.T) {
	for _, hp := range []string{
		"/var/lib/kubeswift/snapshots/default_snap1",
		"/var/lib/kubeswift/snapshots/default_snap1/",
		"",
	} {
		if err := ValidateLocalHostPath("default", "snap1", hp); err != nil {
			t.Errorf("hostPath %q should be accepted, got: %v", hp, err)
		}
	}
	for _, hp := range []string{
		"/var/lib/kubeswift/snapshots/other_snap1",   // another namespace's directory
		"/var/lib/kubeswift/snapshots/default-snap1", // the old, ambiguous form
		"/var/lib/kubeswift/snapshots/default_snap2", // another snapshot's
		"/var/lib/kubeswift/snapshots/",
		"/var/lib/kubeswift/snapshots/default_snap1/x",
	} {
		if err := ValidateLocalHostPath("default", "snap1", hp); err == nil {
			t.Errorf("hostPath %q should be rejected", hp)
		}
	}
}

// The prefix itself, a glob, shell metacharacters and a nested path must all be
// refused: a snapshot directory is mounted into a privileged Job and handed to
// rm, so the shared root would wipe every namespace's snapshots and a
// metacharacter would reach a shell. A single safe segment is accepted.
func TestValidateSnapshotDir_SegmentRules(t *testing.T) {
	reject := []string{
		"/var/lib/kubeswift/snapshots/",         // the shared root itself
		"/var/lib/kubeswift/snapshots/*",        // glob
		"/var/lib/kubeswift/snapshots/a b",      // space
		"/var/lib/kubeswift/snapshots/a;rm -rf", // shell metacharacter
		"/var/lib/kubeswift/snapshots/a/b",      // nested
		"/var/lib/kubeswift/snapshots/-rf",      // leading dash (rm flag)
		"/var/lib/kubeswift/snapshots/..",       // dot-dot
		"/tmp/x",                                // outside the root
	}
	for _, hp := range reject {
		if err := ValidateSnapshotDir(hp); err == nil {
			t.Errorf("dir %q should be rejected", hp)
		}
	}
	for _, hp := range []string{
		"/var/lib/kubeswift/snapshots/default_snap1",
		"/var/lib/kubeswift/snapshots/default-snap1", // captured by an earlier version
		"/var/lib/kubeswift/snapshots/ns-name-1700000000",
		"/var/lib/kubeswift/snapshots/a.b_c-1/",
	} {
		if err := ValidateSnapshotDir(hp); err != nil {
			t.Errorf("dir %q should be accepted, got: %v", hp, err)
		}
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

func TestValidate_S3Backend(t *testing.T) {
	v := &Validator{}
	valid := func() *snapshotv1alpha1.SwiftSnapshot {
		s := makeSnap(snapshotv1alpha1.SnapshotBackendS3)
		s.Spec.Backend.S3 = &snapshotv1alpha1.S3Backend{
			Bucket:               "b",
			Region:               "us-east-1",
			CredentialsSecretRef: &snapshotv1alpha1.SecretObjectReference{Name: "creds"},
		}
		return s
	}
	if _, err := v.ValidateCreate(context.Background(), valid()); err != nil {
		t.Errorf("valid s3 backend should be accepted; got %v", err)
	}

	cases := []struct {
		name   string
		mut    func(*snapshotv1alpha1.S3Backend)
		reject string
	}{
		{"missing bucket", func(s *snapshotv1alpha1.S3Backend) { s.Bucket = "" }, "bucket"},
		{"missing creds", func(s *snapshotv1alpha1.S3Backend) { s.CredentialsSecretRef = nil }, "credentialsSecretRef"},
		{"missing region and endpoint", func(s *snapshotv1alpha1.S3Backend) { s.Region = "" }, "region"},
	}
	for _, tc := range cases {
		s := valid()
		tc.mut(s.Spec.Backend.S3)
		if _, err := v.ValidateCreate(context.Background(), s); err == nil || !strings.Contains(err.Error(), tc.reject) {
			t.Errorf("%s: want rejection mentioning %q; got %v", tc.name, tc.reject, err)
		}
	}

	// Endpoint set (S3-compatible) without region is accepted.
	s := valid()
	s.Spec.Backend.S3.Region = ""
	s.Spec.Backend.S3.Endpoint = "minio.svc:9000"
	if _, err := v.ValidateCreate(context.Background(), s); err != nil {
		t.Errorf("endpoint without region should be accepted; got %v", err)
	}
}

func TestValidate_OCIBackend(t *testing.T) {
	v := &Validator{}
	valid := func() *snapshotv1alpha1.SwiftSnapshot {
		s := makeSnap(snapshotv1alpha1.SnapshotBackendOCI)
		s.Spec.Backend.OCI = &snapshotv1alpha1.OCIBackend{
			Repository: "zot.registry.svc:5000/vm-snapshots",
		}
		return s
	}
	// Repository set, anonymous (no creds) is valid.
	if _, err := v.ValidateCreate(context.Background(), valid()); err != nil {
		t.Errorf("valid oci backend should be accepted; got %v", err)
	}
	// A bare tag is accepted.
	s := valid()
	s.Spec.Backend.OCI.Tag = "prod-2026"
	if _, err := v.ValidateCreate(context.Background(), s); err != nil {
		t.Errorf("bare tag should be accepted; got %v", err)
	}
	// A named signing key ref is accepted (opt-in provenance signing).
	sig := valid()
	sig.Spec.Backend.OCI.SigningKeySecretRef = &snapshotv1alpha1.SecretObjectReference{Name: "cosign-key"}
	if _, err := v.ValidateCreate(context.Background(), sig); err != nil {
		t.Errorf("named signingKeySecretRef should be accepted; got %v", err)
	}

	cases := []struct {
		name   string
		mut    func(*snapshotv1alpha1.OCIBackend)
		reject string
	}{
		{"missing repository", func(o *snapshotv1alpha1.OCIBackend) { o.Repository = "" }, "repository"},
		{"tag carries a ref", func(o *snapshotv1alpha1.OCIBackend) { o.Tag = "repo:tag" }, "bare tag"},
		{"tag carries a digest", func(o *snapshotv1alpha1.OCIBackend) { o.Tag = "x@sha256" }, "bare tag"},
		{"signing key ref without name", func(o *snapshotv1alpha1.OCIBackend) {
			o.SigningKeySecretRef = &snapshotv1alpha1.SecretObjectReference{}
		}, "signingKeySecretRef.name"},
	}
	for _, tc := range cases {
		s := valid()
		tc.mut(s.Spec.Backend.OCI)
		if _, err := v.ValidateCreate(context.Background(), s); err == nil || !strings.Contains(err.Error(), tc.reject) {
			t.Errorf("%s: want rejection mentioning %q; got %v", tc.name, tc.reject, err)
		}
	}

	// type=oci with a nil oci carrier is rejected.
	bare := makeSnap(snapshotv1alpha1.SnapshotBackendOCI)
	if _, err := v.ValidateCreate(context.Background(), bare); err == nil || !strings.Contains(err.Error(), "backend.oci is required") {
		t.Errorf("nil oci carrier: want 'backend.oci is required'; got %v", err)
	}
}

// TestValidate_OCICarrierForbidden: an oci carrier on a non-oci backend is rejected.
func TestValidate_OCICarrierForbidden(t *testing.T) {
	v := &Validator{}
	s := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	s.Spec.Backend.OCI = &snapshotv1alpha1.OCIBackend{Repository: "r"}
	if _, err := v.ValidateCreate(context.Background(), s); err == nil || !strings.Contains(err.Error(), "backend.oci") {
		t.Errorf("oci carrier on local backend should be rejected; got %v", err)
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
		// The shared-base guard resolves a guest's class; without the class
		// type registered the lookup panics instead of failing.
		&swiftv1alpha1.SwiftGuestClass{}, &swiftv1alpha1.SwiftGuestClassList{},
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
	// csi-volume-snapshot is disk-only — the VFIO/QEMU memory rules don't apply
	// even for a GPU guest. Gated on the BACKEND, not the includeMemory flag.
	guest := makeSourceGuest("g1", "default")
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "h200"}

	v := validatorWithGuest(t, guest)
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	snap.Spec.GuestRef.Name = "g1"
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("disk-only (csi) capture should bypass GPU rule: %v", err)
	}
}

// TestValidate_CSIWithDefaultIncludeMemory_GPUGuest_NotRejected reproduces the
// production false-positive: includeMemory defaults to true, so a csi disk-only
// snapshot of a GPU guest used to be wrongly rejected. Backend-gating fixes it.
func TestValidate_CSIWithDefaultIncludeMemory_GPUGuest_NotRejected(t *testing.T) {
	guest := makeSourceGuest("g1", "default")
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "h200"}
	v := validatorWithGuest(t, guest)
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	snap.Spec.GuestRef.Name = "g1"
	snap.Spec.IncludeMemory = true // the apiserver default
	if _, err := v.ValidateCreate(context.Background(), snap); err != nil {
		t.Errorf("csi (disk-only) snapshot of a GPU guest must NOT be rejected: %v", err)
	}
}

// TestValidate_LocalIncludeMemoryFalse_GPUGuest_StillRejected proves the bypass
// is closed: a local snapshot captures memory regardless of the flag, so the
// VFIO compat check must fire even when includeMemory:false.
func TestValidate_LocalIncludeMemoryFalse_GPUGuest_StillRejected(t *testing.T) {
	guest := makeSourceGuest("g1", "default")
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "h200"}
	v := validatorWithGuest(t, guest)
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.GuestRef.Name = "g1"
	snap.Spec.IncludeMemory = false // must NOT bypass — local always captures memory
	if _, err := v.ValidateCreate(context.Background(), snap); err == nil {
		t.Error("local + includeMemory:false on a GPU guest must still be rejected (memory is captured anyway)")
	}
}

// TestValidate_IncludeMemoryFalse_LocalWarns: a no-op includeMemory:false on a
// memory backend surfaces a warning (not a rejection).
func TestValidate_IncludeMemoryFalse_LocalWarns(t *testing.T) {
	v := &Validator{} // no client — shape + warning only
	snap := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	snap.Spec.IncludeMemory = false
	w, err := v.ValidateCreate(context.Background(), snap)
	if err != nil {
		t.Fatalf("includeMemory:false must warn, not reject: %v", err)
	}
	found := false
	for _, s := range w {
		if strings.Contains(s, "includeMemory:false is ignored") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an includeMemory no-op warning; got %v", w)
	}
	// csi + includeMemory:true (the default) does NOT warn.
	csi := makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot)
	csi.Spec.IncludeMemory = true
	if w, _ := v.ValidateCreate(context.Background(), csi); len(w) != 0 {
		t.Errorf("csi + includeMemory:true must not warn; got %v", w)
	}
}

func ociFullState() *snapshotv1alpha1.SwiftSnapshot {
	s := makeSnap(snapshotv1alpha1.SnapshotBackendOCI)
	s.Spec.Backend.OCI = &snapshotv1alpha1.OCIBackend{Repository: "zot.svc:5000/vm-snapshots"}
	s.Spec.IncludeMemory = true
	s.Spec.IncludeDisk = true
	return s
}

func TestValidate_IncludeDisk_OK(t *testing.T) {
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), ociFullState()); err != nil {
		t.Errorf("includeDisk + oci + includeMemory should be valid: %v", err)
	}
}

func TestValidate_IncludeDisk_RequiresOCI(t *testing.T) {
	v := &Validator{}
	s := makeSnap(snapshotv1alpha1.SnapshotBackendLocal)
	s.Spec.IncludeMemory = true
	s.Spec.IncludeDisk = true
	_, err := v.ValidateCreate(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "requires spec.backend.type=oci") {
		t.Errorf("includeDisk on a non-oci backend must be rejected; got %v", err)
	}
}

func TestValidate_IncludeDisk_RequiresMemory(t *testing.T) {
	v := &Validator{}
	s := ociFullState()
	s.Spec.IncludeMemory = false
	_, err := v.ValidateCreate(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "requires spec.includeMemory") {
		t.Errorf("includeDisk without includeMemory must be rejected; got %v", err)
	}
}

// Tier A captures a CSI VolumeSnapshot of the guest's root PVC, and a
// shared-base guest has no root PVC — its disk is a thin snapshot in a
// node-local pool. Refusing at admission gives the operator the answer when
// they apply, instead of a snapshot that sits Pending and then fails claiming a
// PVC "disappeared" that never existed.
func TestValidate_CSIBackend_SharedBaseGuestIsRefused(t *testing.T) {
	scheme := newSchemeForMemoryTests(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "shared"}},
	}
	// Cluster-scoped: no namespace.
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{SharedBaseDisk: true},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	v := &Validator{Client: c}

	err := v.validateSwiftSnapshot(context.Background(), makeSnap(snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot))
	if err == nil {
		t.Fatal("a csi snapshot of a sharedBaseDisk guest was accepted; it can never succeed")
	}
	for _, want := range []string{"sharedBaseDisk", "shared", "local"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
}

// Tier B and Tier C capture through the running guest, not the PVC, so they are
// unaffected. Refusing them too would remove the only snapshot paths a
// shared-base guest has.
func TestValidate_LocalBackend_SharedBaseGuestIsAllowed(t *testing.T) {
	scheme := newSchemeForMemoryTests(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "shared"}},
	}
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{SharedBaseDisk: true},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	v := &Validator{Client: c}

	if err := v.validateSwiftSnapshot(context.Background(), makeSnap(snapshotv1alpha1.SnapshotBackendLocal)); err != nil {
		t.Errorf("a local (Tier B) snapshot of a sharedBaseDisk guest must be allowed: %v", err)
	}
}

// A full-state (includeDisk) snapshot exports the guest's root PVC, and a
// shared-base guest has none. This must be refused at admission.
//
// It is also a regression guard for an ordering bug: an oci snapshot captures
// memory, and the memory-compatibility check RETURNED early, so a shared-base
// check placed after it was unreachable for exactly the oci + includeDisk case
// it existed for. Caught by this test, not by reading the code.
func TestValidate_IncludeDisk_SharedBaseGuestIsRefused(t *testing.T) {
	scheme := newSchemeForMemoryTests(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "shared"}},
	}
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{SharedBaseDisk: true},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	v := &Validator{Client: c}

	snap := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "default"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef:      snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"},
			IncludeMemory: true,
			IncludeDisk:   true,
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendOCI,
				OCI:  &snapshotv1alpha1.OCIBackend{Repository: "registry.example.com/snaps"},
			},
		},
	}
	err := v.validateSwiftSnapshot(context.Background(), snap)
	if err == nil {
		t.Fatal("an includeDisk snapshot of a sharedBaseDisk guest was admitted; the disk export needs a root PVC it does not have")
	}
	if !strings.Contains(err.Error(), "includeDisk") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// A memory-only oci snapshot of the same guest is fine: it captures through the
// running guest and never reads a PVC.
func TestValidate_OCIMemoryOnly_SharedBaseGuestIsAllowed(t *testing.T) {
	scheme := newSchemeForMemoryTests(t)
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "default"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "shared"}},
	}
	class := &swiftv1alpha1.SwiftGuestClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       swiftv1alpha1.SwiftGuestClassSpec{SharedBaseDisk: true},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(guest, class).Build()
	v := &Validator{Client: c}

	snap := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "default"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef:      snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"},
			IncludeMemory: true,
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendOCI,
				OCI:  &snapshotv1alpha1.OCIBackend{Repository: "registry.example.com/snaps"},
			},
		},
	}
	if err := v.validateSwiftSnapshot(context.Background(), snap); err != nil {
		t.Errorf("a memory-only oci snapshot of a sharedBaseDisk guest must be allowed: %v", err)
	}
}

// A snapshot was validated against its source guest when it was created. If
// the guest changes afterwards (here: a GPU is added), updates that do not
// touch the snapshot's spec -- above all the controller removing its cleanup
// finalizer -- must still be admitted, or the snapshot and its namespace stay
// Terminating.
func TestValidateUpdate_SourceGuestChangedSinceCreation(t *testing.T) {
	guest := makeSourceGuest("g1", "default")
	guest.Spec.GPUProfileRef = &corev1.LocalObjectReference{Name: "h200-shared"} // added after the capture
	v := validatorWithGuest(t, guest)
	ctx := context.Background()

	old := makeMemoryCaptureSnap("g1")
	old.Finalizers = []string{"kubeswift.io/snapshot-hostpath-cleanup"}

	labeled := old.DeepCopy()
	labeled.Labels = map[string]string{"team": "a"}
	if _, err := v.ValidateUpdate(ctx, old, labeled); err != nil {
		t.Errorf("metadata-only update rejected because the source guest changed: %v", err)
	}

	now := metav1.Now()
	deleting := old.DeepCopy()
	deleting.DeletionTimestamp = &now
	released := deleting.DeepCopy()
	released.Finalizers = nil
	if _, err := v.ValidateUpdate(ctx, deleting, released); err != nil {
		t.Errorf("finalizer removal rejected: %v", err)
	}

	// The spec is still immutable.
	changed := old.DeepCopy()
	changed.Spec.GuestRef.Name = "other"
	if _, err := v.ValidateUpdate(ctx, old, changed); err == nil {
		t.Error("a spec change must still be rejected")
	}
}
