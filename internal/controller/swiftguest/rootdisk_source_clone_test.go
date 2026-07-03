package swiftguest

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/resolved"
)

// localCloneSnap is a Tier-B (local) memory snapshot — no oci disk, so a clone's
// root disk must come from the SOURCE guest's disk, not the pristine image.
func localCloneSnap() *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap", Namespace: "ns"},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: "src"},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:  snapshotv1alpha1.SnapshotBackendLocal,
				Local: &snapshotv1alpha1.LocalBackend{HostPath: "/var/lib/kubeswift/snapshots/"},
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			Phase:    snapshotv1alpha1.SwiftSnapshotPhaseReady,
			NodeName: "boba",
		},
	}
}

func sourceRootPVCObj() *corev1.PersistentVolumeClaim {
	fs := corev1.PersistentVolumeFilesystem
	sc := "longhorn"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: RootDiskCloneName("src"), Namespace: "ns"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:       &fs,
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

// A memory-only cloneFromSnapshot clone must CSI-clone the SOURCE's root PVC
// (grown geometry + real data), NOT copy the pristine SwiftImage — the fix for
// the clone-reboot "bad geometry" initramfs drop.
func TestMaybeRootDiskFromSourceClone_CSIClonesSourcePVC(t *testing.T) {
	ctx := context.Background()
	g := cloneGuest()
	r, c := newCloneReconciler(t, g, localCloneSnap(), sourceRootPVCObj())

	// Pass 1: creates the clone PVC as a CSI clone of the source root PVC, requeues.
	handled, res, err := r.maybeRootDiskFromSourceClone(ctx, g, &resolved.ResolvedGuest{})
	if !handled || res != nil || err == nil {
		t.Fatalf("memory-only clone must be handled + requeue on PVC create; handled=%v res=%v err=%v", handled, res, err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Name: RootDiskCloneName("clone-a"), Namespace: "ns"}, &pvc); err != nil {
		t.Fatalf("clone PVC not created: %v", err)
	}
	// dataSource = the SOURCE's root PVC (NOT a copy Job from the image).
	if pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Kind != "PersistentVolumeClaim" ||
		pvc.Spec.DataSource.Name != RootDiskCloneName("src") {
		t.Fatalf("clone PVC must dataSource-clone the source root PVC; got %+v", pvc.Spec.DataSource)
	}
	// RestoreSeeded so the image copy path skips it; owned by the clone guest.
	if pvc.Labels[RestoreSeededLabel] != "true" {
		t.Errorf("clone PVC must be RestoreSeeded; labels=%+v", pvc.Labels)
	}
	if len(pvc.OwnerReferences) == 0 || pvc.OwnerReferences[0].Name != "clone-a" {
		t.Errorf("clone PVC must be owned by the clone guest; ownerRefs=%+v", pvc.OwnerReferences)
	}
	// Same size/class as the source (CSI clone requires an exact match).
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "10Gi" {
		t.Errorf("clone PVC size = %q, want 10Gi (match the source)", got.String())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "longhorn" {
		t.Errorf("clone PVC class must match the source; got %v", pvc.Spec.StorageClassName)
	}

	// Pass 2: PVC Bound → returns the materialized root disk, never grow-init.
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(ctx, &pvc); err != nil {
		t.Fatal(err)
	}
	handled, res, err = r.maybeRootDiskFromSourceClone(ctx, g, &resolved.ResolvedGuest{})
	if !handled || err != nil || res == nil {
		t.Fatalf("Bound clone PVC should return the root disk; handled=%v res=%v err=%v", handled, res, err)
	}
	if res.PVCName != RootDiskCloneName("clone-a") {
		t.Errorf("result PVC = %q, want %q", res.PVCName, RootDiskCloneName("clone-a"))
	}
	if res.NeedsGrowInit {
		t.Errorf("a byte-clone of the source disk must NOT be grow-init'd (would desync partition vs the resumed fs)")
	}
}

// Source root PVC gone → a memory-only clone cannot build a consistent disk;
// fail loudly (no silent fall-through to a pristine-image copy).
func TestMaybeRootDiskFromSourceClone_SourceGone_FailsLoud(t *testing.T) {
	g := cloneGuest()
	r, _ := newCloneReconciler(t, g, localCloneSnap()) // no source root PVC
	handled, res, err := r.maybeRootDiskFromSourceClone(context.Background(), g, &resolved.ResolvedGuest{})
	if !handled || res != nil || err == nil {
		t.Fatalf("missing source disk must be handled + error, not a silent fall-through; handled=%v res=%v err=%v", handled, res, err)
	}
}

// A full-state (oci disk) snapshot is handled by maybeRootDiskFromOCI — the
// source-clone path must decline it (handled=false).
func TestMaybeRootDiskFromSourceClone_DeclinesOCIDiskSnapshot(t *testing.T) {
	g := cloneGuest()
	snap := ociCloneSnap()
	snap.Status.OCI.Disk = &snapshotv1alpha1.OCIDiskArtifact{Reference: "r:t-disk", ManifestDigest: "sha256:disk"}
	r, _ := newCloneReconciler(t, g, snap)
	handled, _, _ := r.maybeRootDiskFromSourceClone(context.Background(), g, &resolved.ResolvedGuest{})
	if handled {
		t.Fatal("a full-state oci-disk clone must be left to maybeRootDiskFromOCI (handled=false)")
	}
}
