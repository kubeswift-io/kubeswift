package swiftrestore

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func TestValidate_IdentityRegenerate_KnownItemAccepted(t *testing.T) {
	// Phase 2 wires identity regeneration. A single known item with no
	// Client (so the macAddresses-on-clone rule is skipped) must pass.
	r := makeRestore("snap1", "target")
	r.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{snapshotv1alpha1.RegenHostname},
	}
	v := &Validator{}
	if _, err := v.ValidateCreate(context.Background(), r); err != nil {
		t.Errorf("known regenerate item should be accepted: %v", err)
	}
}

func TestValidate_IdentityRegenerate_DuplicateRejected(t *testing.T) {
	r := makeRestore("snap1", "target")
	r.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{
			snapshotv1alpha1.RegenHostname,
			snapshotv1alpha1.RegenHostname,
		},
	}
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate rejection, got: %v", err)
	}
}

func TestValidate_IdentityRegenerate_UnknownItemRejected(t *testing.T) {
	r := makeRestore("snap1", "target")
	r.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{"not-a-real-item"},
	}
	v := &Validator{}
	_, err := v.ValidateCreate(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "unknown value") {
		t.Errorf("expected unknown-item rejection, got: %v", err)
	}
}

// -------- macAddresses-on-clone tests (Client-backed Validator) --------

func newSchemeForCloneTests(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	gvSnap := schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSnap,
		&snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{},
		&snapshotv1alpha1.SwiftRestore{}, &snapshotv1alpha1.SwiftRestoreList{},
	)
	metav1.AddToGroupVersion(s, gvSnap)
	return s
}

// makeLocalMemorySnap fabricates a Tier B SwiftSnapshot whose source
// guest has the given name. Memory snapshots inherently have a memory
// component; the validator infers "memory" from backend.type=local.
func makeLocalMemorySnap(name, ns, sourceGuest string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: sourceGuest},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{
					HostPath: "/var/lib/kubeswift/snapshots/default-snap1",
				},
			},
			IncludeMemory: true,
		},
	}
}

func validatorWithSnap(t *testing.T, snap *snapshotv1alpha1.SwiftSnapshot) *Validator {
	t.Helper()
	scheme := newSchemeForCloneTests(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snap).Build()
	return &Validator{Client: c}
}

func TestValidate_CloneOfMemorySnap_RequiresMACRegen(t *testing.T) {
	snap := makeLocalMemorySnap("snap1", "default", "g1")
	v := validatorWithSnap(t, snap)
	r := makeRestore("snap1", "g1-cloned") // cloning to a different name
	// No identity.regenerate set → must reject.
	_, err := v.ValidateCreate(context.Background(), r)
	if err == nil {
		t.Fatal("expected rejection (cloning memory snap without macAddresses regen)")
	}
	if !strings.Contains(err.Error(), "macAddresses") {
		t.Errorf("error should reference macAddresses; got %v", err)
	}
}

func TestValidate_CloneOfMemorySnap_WithMACRegen_OK(t *testing.T) {
	snap := makeLocalMemorySnap("snap1", "default", "g1")
	v := validatorWithSnap(t, snap)
	r := makeRestore("snap1", "g1-cloned")
	r.Spec.Identity = &snapshotv1alpha1.IdentityRegeneration{
		Regenerate: []snapshotv1alpha1.IdentityRegenerationItem{
			snapshotv1alpha1.RegenMACAddresses,
		},
	}
	if _, err := v.ValidateCreate(context.Background(), r); err != nil {
		t.Errorf("clone with macAddresses regen should pass: %v", err)
	}
}

func TestValidate_InPlaceRestoreOfMemorySnap_DoesNotRequireMACRegen(t *testing.T) {
	// Restore back into the same SwiftGuest name (no clone): MAC stays
	// the same, no L2 conflict possible.
	snap := makeLocalMemorySnap("snap1", "default", "g1")
	v := validatorWithSnap(t, snap)
	r := makeRestore("snap1", "g1") // same as source
	if _, err := v.ValidateCreate(context.Background(), r); err != nil {
		t.Errorf("in-place restore should not require MAC regen: %v", err)
	}
}

func TestValidate_CloneOfCSISnap_DoesNotRequireMACRegen(t *testing.T) {
	// CSI (disk-only) snapshot has no memory state; cloning it produces
	// a fresh-boot VM that gets its own MAC anyway. No need to regen.
	csi := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap1", Namespace: "default"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "g1"},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:              snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot,
				CSIVolumeSnapshot: &snapshotv1alpha1.CSIVolumeSnapshotBackend{},
			},
		},
	}
	v := validatorWithSnap(t, csi)
	r := makeRestore("snap1", "g1-cloned")
	if _, err := v.ValidateCreate(context.Background(), r); err != nil {
		t.Errorf("cloning CSI snap should not require MAC regen: %v", err)
	}
}

func TestValidate_SnapshotMissing_DefersToController(t *testing.T) {
	// Operator can apply SwiftRestore alongside SwiftSnapshot in a
	// single pass. Webhook fails open when the snapshot doesn't exist
	// yet — controller re-checks at restore time.
	scheme := newSchemeForCloneTests(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &Validator{Client: c}
	r := makeRestore("not-yet-created", "g1-cloned")
	if _, err := v.ValidateCreate(context.Background(), r); err != nil {
		t.Errorf("missing snapshot should defer to controller: %v", err)
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
