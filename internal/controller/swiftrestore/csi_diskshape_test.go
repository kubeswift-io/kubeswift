package swiftrestore

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

func srcRootPVC(name string, modes []corev1.PersistentVolumeAccessMode, vm *corev1.PersistentVolumeMode, sc string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: rootPVCName(name), Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      modes,
			VolumeMode:       vm,
			StorageClassName: &sc,
		},
	}
}

// A restore must reproduce the shape of the disk the snapshot was taken from.
// Provisioning RWO+Filesystem against a Block source asks the CSI driver to
// reinterpret the bytes: enforcing drivers (Ceph RBD) refuse the mode change so
// the PVC never binds, and where it does bind the launcher pod builder derives
// Block from the guest's resolved storage and the mount fails permanently.
func TestSourceDiskShapePreservesBlockAndRWX(t *testing.T) {
	blk := corev1.PersistentVolumeBlock
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(srcRootPVC("src", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, &blk, "ceph-block")).
		Build()
	r := &SwiftRestoreReconciler{Client: cl, Scheme: scheme.Scheme}

	shape, err := r.sourceDiskShapeFor(context.Background(), "default", "src")
	if err != nil {
		t.Fatalf("sourceDiskShapeFor: %v", err)
	}
	if shape.StorageClassName != "ceph-block" {
		t.Errorf("storage class = %q, want ceph-block", shape.StorageClassName)
	}
	if shape.VolumeMode == nil || *shape.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("volumeMode = %v, want Block — a Filesystem restore of a Block disk cannot bind on an enforcing driver", shape.VolumeMode)
	}
	if len(shape.AccessModes) != 1 || shape.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("accessModes = %v, want [ReadWriteMany] — RWO would silently drop live-migration eligibility", shape.AccessModes)
	}
}

// A Filesystem source must stay Filesystem — the fix must not flip everything
// to Block.
func TestSourceDiskShapePreservesFilesystemAndRWO(t *testing.T) {
	fs := corev1.PersistentVolumeFilesystem
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(srcRootPVC("src", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, &fs, "longhorn")).
		Build()
	r := &SwiftRestoreReconciler{Client: cl, Scheme: scheme.Scheme}

	shape, err := r.sourceDiskShapeFor(context.Background(), "default", "src")
	if err != nil {
		t.Fatalf("sourceDiskShapeFor: %v", err)
	}
	if shape.VolumeMode == nil || *shape.VolumeMode != corev1.PersistentVolumeFilesystem {
		t.Errorf("volumeMode = %v, want Filesystem", shape.VolumeMode)
	}
	if len(shape.AccessModes) != 1 || shape.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", shape.AccessModes)
	}
}

// The shape must land on the PVC the restore actually creates, not just be
// computed and dropped.
func TestEnsureRestorePVCAppliesTheSourceShape(t *testing.T) {
	blk := corev1.PersistentVolumeBlock
	cl := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	r := &SwiftRestoreReconciler{Client: cl, Scheme: scheme.Scheme}

	restore := &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "default"},
	}
	shape := sourceDiskShape{
		StorageClassName: "ceph-block",
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		VolumeMode:       &blk,
	}
	if err := r.ensureRestorePVC(context.Background(), restore, "swiftguest-root-tgt", "vs1", shape, 6<<30); err != nil {
		t.Fatalf("ensureRestorePVC: %v", err)
	}

	var got corev1.PersistentVolumeClaim
	if err := cl.Get(context.Background(), client.ObjectKey{Name: "swiftguest-root-tgt", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get created PVC: %v", err)
	}
	if got.Spec.VolumeMode == nil || *got.Spec.VolumeMode != corev1.PersistentVolumeBlock {
		t.Errorf("created PVC volumeMode = %v, want Block", got.Spec.VolumeMode)
	}
	if len(got.Spec.AccessModes) != 1 || got.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("created PVC accessModes = %v, want [ReadWriteMany]", got.Spec.AccessModes)
	}
	if got.Spec.StorageClassName == nil || *got.Spec.StorageClassName != "ceph-block" {
		t.Errorf("created PVC storageClass = %v, want ceph-block", got.Spec.StorageClassName)
	}
}
