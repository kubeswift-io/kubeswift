package swiftguest

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/projectbeskar/kubeswift/internal/resolved"
)

// resolvedAccessModes / resolvedVolumeMode / resolvedStorageClassName
// translate the resolved storage spec into PVC fields. Empty/legacy
// resolution must produce the pre-PR-32 PVC shape (RWO, Filesystem,
// inherit storage class from source SwiftImage's PVC).

func TestResolvedAccessModes_DefaultsToRWO(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{}}
	got := resolvedAccessModes(rg)
	if len(got) != 1 || got[0] != corev1.ReadWriteOnce {
		t.Errorf("resolvedAccessModes = %v, want [ReadWriteOnce]", got)
	}
}

func TestResolvedAccessModes_PassesThroughRWX(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{AccessMode: "ReadWriteMany"}}
	got := resolvedAccessModes(rg)
	if len(got) != 1 || got[0] != corev1.ReadWriteMany {
		t.Errorf("resolvedAccessModes = %v, want [ReadWriteMany]", got)
	}
}

func TestResolvedVolumeMode_DefaultsToFilesystem(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{}}
	got := resolvedVolumeMode(rg)
	if got == nil {
		t.Fatal("resolvedVolumeMode = nil, want explicit Filesystem")
	}
	if *got != corev1.PersistentVolumeFilesystem {
		t.Errorf("resolvedVolumeMode = %v, want Filesystem", *got)
	}
}

func TestResolvedVolumeMode_PassesThroughBlock(t *testing.T) {
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{VolumeMode: "Block"}}
	got := resolvedVolumeMode(rg)
	if got == nil || *got != corev1.PersistentVolumeBlock {
		t.Errorf("resolvedVolumeMode = %v, want Block", got)
	}
}

func TestResolvedStorageClassName_FallsThroughToSourcePVC(t *testing.T) {
	// Pre-PR-32 behaviour: when resolved.Storage.StorageClassName is
	// empty, the per-guest clone PVC inherits the source SwiftImage's
	// PVC's storage class. Required for Longhorn (refuses cross-class
	// clones) and matches the historical default.
	srcClass := "longhorn"
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{StorageClassName: ""}}
	src := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &srcClass}}
	got := resolvedStorageClassName(rg, src)
	if got == nil || *got != "longhorn" {
		t.Errorf("resolvedStorageClassName = %v, want longhorn (inherited)", got)
	}
}

func TestResolvedStorageClassName_ResolvedSpecOverridesSource(t *testing.T) {
	// Operator override: resolved.Storage.StorageClassName non-empty
	// wins over the source PVC's class. This is how operators target a
	// migratable Longhorn class without changing the SwiftImage.
	srcClass := "longhorn"
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{StorageClassName: "longhorn-migratable"}}
	src := &corev1.PersistentVolumeClaim{Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &srcClass}}
	got := resolvedStorageClassName(rg, src)
	if got == nil || *got != "longhorn-migratable" {
		t.Errorf("resolvedStorageClassName = %v, want longhorn-migratable", got)
	}
}

// checkStorageReady runs per-driver pre-flight checks. The Longhorn
// migratable-parameter check is the only one today; non-Longhorn drivers
// pass through, and non-RWX guests don't trigger the check at all.

func storageScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	return s
}

func TestCheckStorageReady_NonLiveMigrationCapablePassesThrough(t *testing.T) {
	// RWO+Filesystem (the default) doesn't need the migratable check —
	// it isn't claiming live-migration capability.
	r := &SwiftGuestReconciler{Client: fake.NewClientBuilder().WithScheme(storageScheme(t)).Build()}
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{
		AccessMode:       "ReadWriteOnce",
		VolumeMode:       "Filesystem",
		StorageClassName: "any",
	}}
	_, _, ok := r.checkStorageReady(context.Background(), rg)
	if !ok {
		t.Errorf("checkStorageReady = !ok for non-live-migration-capable spec; should pass through")
	}
}

func TestCheckStorageReady_LonghornMissingMigratableParameterFails(t *testing.T) {
	// RWX+Block on a Longhorn class without parameters.migratable=true:
	// surfaces a clear status condition with the cluster-admin remedy.
	scheme := storageScheme(t)
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "longhorn"},
		Provisioner: LonghornCSIProvisioner,
		Parameters:  map[string]string{}, // missing migratable
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sc).Build()
	r := &SwiftGuestReconciler{Client: c}
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{
		AccessMode:       "ReadWriteMany",
		VolumeMode:       "Block",
		StorageClassName: "longhorn",
	}}
	reason, msg, ok := r.checkStorageReady(context.Background(), rg)
	if ok {
		t.Errorf("checkStorageReady = ok for Longhorn without migratable; want false")
	}
	if reason != "LonghornNotMigratable" {
		t.Errorf("reason = %q, want LonghornNotMigratable", reason)
	}
	if !strings.Contains(msg, "migratable") {
		t.Errorf("message %q should mention 'migratable' so cluster admins know what to fix", msg)
	}
}

func TestCheckStorageReady_LonghornWithMigratablePasses(t *testing.T) {
	scheme := storageScheme(t)
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "longhorn-migratable"},
		Provisioner: LonghornCSIProvisioner,
		Parameters:  map[string]string{LonghornMigratableParameter: "true"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sc).Build()
	r := &SwiftGuestReconciler{Client: c}
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{
		AccessMode:       "ReadWriteMany",
		VolumeMode:       "Block",
		StorageClassName: "longhorn-migratable",
	}}
	_, _, ok := r.checkStorageReady(context.Background(), rg)
	if !ok {
		t.Errorf("checkStorageReady = !ok for properly configured Longhorn class; want ok")
	}
}

func TestCheckStorageReady_NonLonghornPassesThrough(t *testing.T) {
	// Non-Longhorn provisioner: no check, no false alarm. Future
	// per-driver checks (Ceph RBD imageFeatures, EBS volume type) land
	// alongside the Longhorn one.
	scheme := storageScheme(t)
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: "rook-ceph"},
		Provisioner: "rook-ceph.rbd.csi.ceph.com",
		Parameters:  map[string]string{},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sc).Build()
	r := &SwiftGuestReconciler{Client: c}
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{
		AccessMode:       "ReadWriteMany",
		VolumeMode:       "Block",
		StorageClassName: "rook-ceph",
	}}
	_, _, ok := r.checkStorageReady(context.Background(), rg)
	if !ok {
		t.Errorf("checkStorageReady = !ok for non-Longhorn driver; should pass through")
	}
}

func TestCheckStorageReady_StorageClassNotFoundFails(t *testing.T) {
	// RWX+Block referencing a class that doesn't exist: status
	// condition surfaces the gap. The PVC will fail to bind anyway;
	// surfacing the cause earlier helps operators find it.
	scheme := storageScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SwiftGuestReconciler{Client: c}
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{
		AccessMode:       "ReadWriteMany",
		VolumeMode:       "Block",
		StorageClassName: "nonexistent",
	}}
	reason, _, ok := r.checkStorageReady(context.Background(), rg)
	if ok {
		t.Errorf("checkStorageReady = ok for missing StorageClass; want false")
	}
	if reason != "StorageClassNotFound" {
		t.Errorf("reason = %q, want StorageClassNotFound", reason)
	}
}

func TestCheckStorageReady_EmptyStorageClassNameDefersWithoutFalseAlarm(t *testing.T) {
	// RWX+Block but storageClassName falls through to source-image
	// class (empty in resolved spec). The check can't see the inherited
	// class from this layer, so it deliberately doesn't false-alarm —
	// the migratable check will be re-evaluated when the operator
	// names the class explicitly.
	r := &SwiftGuestReconciler{Client: fake.NewClientBuilder().WithScheme(storageScheme(t)).Build()}
	rg := &resolved.ResolvedGuest{Storage: resolved.Storage{
		AccessMode:       "ReadWriteMany",
		VolumeMode:       "Block",
		StorageClassName: "",
	}}
	_, _, ok := r.checkStorageReady(context.Background(), rg)
	if !ok {
		t.Errorf("checkStorageReady = !ok for empty storageClassName; should defer (passes through)")
	}
}
