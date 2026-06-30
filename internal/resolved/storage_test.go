package resolved

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// Storage tests cover the per-field merge between SwiftGuest.spec.storage,
// SwiftGuestClass.spec.storage, and the system defaults
// (RWO + Filesystem). The architect (Q2) confirmed per-field merge is the
// resolution rule: each non-empty field on SwiftGuest wins, then the
// same field on SwiftGuestClass, then defaults.

func TestMergeStorage_DefaultsWhenBothUnset(t *testing.T) {
	guest := &swiftv1alpha1.SwiftGuest{ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns"}}
	class := &swiftv1alpha1.SwiftGuestClass{}
	got := MergeStorage(guest, class)
	if got.AccessMode != "ReadWriteOnce" {
		t.Errorf("AccessMode = %q, want ReadWriteOnce (system default)", got.AccessMode)
	}
	if got.VolumeMode != "Filesystem" {
		t.Errorf("VolumeMode = %q, want Filesystem (system default)", got.VolumeMode)
	}
	if got.StorageClassName != "" {
		t.Errorf("StorageClassName = %q, want empty (legacy fall-through)", got.StorageClassName)
	}
}

func TestMergeStorage_ClassValuesUsedWhenGuestUnset(t *testing.T) {
	class := &swiftv1alpha1.SwiftGuestClass{
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				AccessMode:       corev1.ReadWriteMany,
				VolumeMode:       corev1.PersistentVolumeBlock,
				StorageClassName: ptr.To("longhorn-migratable"),
			},
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{}
	got := MergeStorage(guest, class)
	if got.AccessMode != "ReadWriteMany" {
		t.Errorf("AccessMode = %q, want ReadWriteMany (from class)", got.AccessMode)
	}
	if got.VolumeMode != "Block" {
		t.Errorf("VolumeMode = %q, want Block (from class)", got.VolumeMode)
	}
	if got.StorageClassName != "longhorn-migratable" {
		t.Errorf("StorageClassName = %q, want longhorn-migratable", got.StorageClassName)
	}
}

func TestMergeStorage_GuestOverridesClassPerField(t *testing.T) {
	// Class sets RWO+FS+default; guest overrides only AccessMode and
	// StorageClassName. VolumeMode falls through to the class value.
	class := &swiftv1alpha1.SwiftGuestClass{
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				AccessMode:       corev1.ReadWriteOnce,
				VolumeMode:       corev1.PersistentVolumeFilesystem,
				StorageClassName: ptr.To("default-class"),
			},
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				AccessMode:       corev1.ReadWriteMany,
				StorageClassName: ptr.To("guest-override-class"),
			},
		},
	}
	got := MergeStorage(guest, class)
	if got.AccessMode != "ReadWriteMany" {
		t.Errorf("AccessMode = %q, want ReadWriteMany (guest override)", got.AccessMode)
	}
	if got.VolumeMode != "Filesystem" {
		t.Errorf("VolumeMode = %q, want Filesystem (class fall-through)", got.VolumeMode)
	}
	if got.StorageClassName != "guest-override-class" {
		t.Errorf("StorageClassName = %q, want guest-override-class", got.StorageClassName)
	}
}

func TestMergeStorage_GuestEmptyStringStorageClassExplicitlyClears(t *testing.T) {
	// *string distinguishes nil ("fall through") from "" ("explicit
	// cluster default"). Both currently resolve to empty string, but
	// the test locks in that the explicit-empty case is at least
	// representable through the spec — operators who want to override a
	// class's storageClassName back to the cluster default can do so.
	class := &swiftv1alpha1.SwiftGuestClass{
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				StorageClassName: ptr.To("class-class"),
			},
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				StorageClassName: ptr.To(""),
			},
		},
	}
	got := MergeStorage(guest, class)
	if got.StorageClassName != "" {
		t.Errorf("StorageClassName = %q, want empty (guest explicitly cleared)", got.StorageClassName)
	}
}

func TestMergeStorage_GuestNilFieldFallsThroughToClass(t *testing.T) {
	// Guest's storage block is non-nil but every field is zero/nil.
	// Each field falls through to the class value, then to defaults.
	class := &swiftv1alpha1.SwiftGuestClass{
		Spec: swiftv1alpha1.SwiftGuestClassSpec{
			Storage: &swiftv1alpha1.StorageSpec{
				AccessMode: corev1.ReadWriteMany,
				VolumeMode: corev1.PersistentVolumeBlock,
			},
		},
	}
	guest := &swiftv1alpha1.SwiftGuest{
		Spec: swiftv1alpha1.SwiftGuestSpec{
			Storage: &swiftv1alpha1.StorageSpec{},
		},
	}
	got := MergeStorage(guest, class)
	if got.AccessMode != "ReadWriteMany" {
		t.Errorf("AccessMode = %q, want ReadWriteMany (class)", got.AccessMode)
	}
	if got.VolumeMode != "Block" {
		t.Errorf("VolumeMode = %q, want Block (class)", got.VolumeMode)
	}
}

func TestStorage_IsLiveMigrationCapable(t *testing.T) {
	// IsLiveMigrationCapable is the canonical KubeVirt-style rule:
	// RWX+Block, nothing else. Any other combination is non-live-
	// migratable (including RWX+Filesystem, which the CRD admission
	// rejects but which the function still classifies correctly as a
	// defense-in-depth signal).
	cases := []struct {
		name string
		s    Storage
		want bool
	}{
		{"RWO+Filesystem", Storage{AccessMode: "ReadWriteOnce", VolumeMode: "Filesystem"}, false},
		{"RWO+Block", Storage{AccessMode: "ReadWriteOnce", VolumeMode: "Block"}, false},
		{"RWX+Filesystem", Storage{AccessMode: "ReadWriteMany", VolumeMode: "Filesystem"}, false},
		{"RWX+Block", Storage{AccessMode: "ReadWriteMany", VolumeMode: "Block"}, true},
		{"empty/empty", Storage{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.IsLiveMigrationCapable(); got != tc.want {
				t.Errorf("IsLiveMigrationCapable = %v, want %v", got, tc.want)
			}
		})
	}
}

// Cross-package agreement test: api/swift/v1alpha1.IsLiveMigrationCapable
// (operates on ResolvedStorageStatus) and resolved.Storage.IsLiveMigrationCapable
// MUST agree on the rule. Both are independently called by the
// SwiftMigration webhook (recompute path) and swiftctl describe; a
// drift between them would produce inconsistent capability signals.
func TestIsLiveMigrationCapable_AgreesAcrossPackages(t *testing.T) {
	cases := []struct {
		access, volume string
	}{
		{"ReadWriteOnce", "Filesystem"},
		{"ReadWriteOnce", "Block"},
		{"ReadWriteMany", "Filesystem"},
		{"ReadWriteMany", "Block"},
		{"", ""},
	}
	for _, tc := range cases {
		resolvedStorage := Storage{AccessMode: tc.access, VolumeMode: tc.volume}
		apiStatus := &swiftv1alpha1.ResolvedStorageStatus{
			AccessMode: corev1.PersistentVolumeAccessMode(tc.access),
			VolumeMode: corev1.PersistentVolumeMode(tc.volume),
		}
		if resolvedStorage.IsLiveMigrationCapable() != swiftv1alpha1.IsLiveMigrationCapable(apiStatus) {
			t.Errorf("disagreement on access=%q volume=%q: resolved=%v api=%v",
				tc.access, tc.volume,
				resolvedStorage.IsLiveMigrationCapable(),
				swiftv1alpha1.IsLiveMigrationCapable(apiStatus))
		}
	}
}
