package swiftimage

import (
	"context"
	"testing"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// W9.x — issue #37. The CSI external-snapshotter requires the
// `snapshot.storage.kubernetes.io/allow-volume-mode-change: "true"`
// annotation on the VolumeSnapshotContent (not the VolumeSnapshot)
// when a clone PVC's volumeMode differs from the source's. Without
// this, RWX+Block SwiftGuests cloning from a Filesystem-mode SwiftImage
// snapshot (the default — the SwiftImage import PVC is RWO+Filesystem)
// fail at PVC provisioning.
//
// The SwiftImage controller's EnsureCloneSeed patches the bound VSC
// with the annotation after the snapshotter creates it. These tests
// pin three contracts:
//
//  1. Pre-bind no-op: when status.boundVolumeSnapshotContentName is
//     not yet populated, ensureAllowVolumeModeChange returns nil
//     without error and without touching state. The next reconcile
//     loop retries.
//  2. Post-bind annotation set: when the VSC exists, the annotation
//     is patched to "true". Idempotent — re-running on an already-
//     annotated VSC is a no-op.
//  3. VSC missing: when status names a VSC that does not exist (CSI
//     race between status update and VSC visibility in the cache),
//     ensureAllowVolumeModeChange returns nil for retry rather than
//     surfacing a hard error.

func newReconciler(t *testing.T, objs ...client.Object) *SwiftImageReconciler {
	t.Helper()
	scheme := finalizerScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &SwiftImageReconciler{Client: c, Scheme: scheme}
}

// TestEnsureAllowVolumeModeChange_NotYetBound is the pre-bind contract.
// The snapshotter has not yet created the VSC; the controller must not
// error out — the next reconcile loop will pick up the binding.
func TestEnsureAllowVolumeModeChange_NotYetBound(t *testing.T) {
	r := newReconciler(t)
	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "img-clone-seed", Namespace: "default"},
		// Status either nil or with BoundVolumeSnapshotContentName=nil:
		// the snapshotter has not bound a VSC yet.
		Status: nil,
	}
	if err := r.ensureAllowVolumeModeChange(context.Background(), snap); err != nil {
		t.Fatalf("nil status should be a no-op; got err=%v", err)
	}

	snap.Status = &volumesnapshotv1.VolumeSnapshotStatus{}
	if err := r.ensureAllowVolumeModeChange(context.Background(), snap); err != nil {
		t.Fatalf("empty status should be a no-op; got err=%v", err)
	}

	emptyName := ""
	snap.Status.BoundVolumeSnapshotContentName = &emptyName
	if err := r.ensureAllowVolumeModeChange(context.Background(), snap); err != nil {
		t.Fatalf("empty BoundVolumeSnapshotContentName should be a no-op; got err=%v", err)
	}
}

// TestEnsureAllowVolumeModeChange_PatchesUnannotatedVSC is the headline
// contract. After the snapshotter binds a VSC, the controller patches
// it with the annotation. This is the fix for W9.x / issue #37.
func TestEnsureAllowVolumeModeChange_PatchesUnannotatedVSC(t *testing.T) {
	vscName := "snapcontent-9290f326"
	vsc := &volumesnapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: vscName},
	}
	r := newReconciler(t, vsc)

	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "img-clone-seed", Namespace: "default"},
		Status: &volumesnapshotv1.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: ptr.To(vscName),
		},
	}
	if err := r.ensureAllowVolumeModeChange(context.Background(), snap); err != nil {
		t.Fatalf("ensureAllowVolumeModeChange: %v", err)
	}

	var got volumesnapshotv1.VolumeSnapshotContent
	if err := r.Get(context.Background(), client.ObjectKey{Name: vscName}, &got); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	if got.Annotations[AllowVolumeModeChangeAnnotation] != "true" {
		t.Errorf("annotation %s = %q, want true; full annotations=%v",
			AllowVolumeModeChangeAnnotation, got.Annotations[AllowVolumeModeChangeAnnotation], got.Annotations)
	}
}

// TestEnsureAllowVolumeModeChange_IdempotentOnAnnotatedVSC verifies that
// re-running on an already-annotated VSC is a no-op: no error, no state
// change. EnsureCloneSeed runs on every reconcile, so this path will be
// hit many times for a single SwiftImage; it must not churn.
func TestEnsureAllowVolumeModeChange_IdempotentOnAnnotatedVSC(t *testing.T) {
	vscName := "snapcontent-already-set"
	vsc := &volumesnapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: vscName,
			Annotations: map[string]string{
				AllowVolumeModeChangeAnnotation:  "true",
				"unrelated.example.com/leave-me": "alone",
			},
		},
	}
	r := newReconciler(t, vsc)

	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "img-clone-seed", Namespace: "default"},
		Status: &volumesnapshotv1.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: ptr.To(vscName),
		},
	}
	if err := r.ensureAllowVolumeModeChange(context.Background(), snap); err != nil {
		t.Fatalf("ensureAllowVolumeModeChange (idempotent run): %v", err)
	}

	var got volumesnapshotv1.VolumeSnapshotContent
	if err := r.Get(context.Background(), client.ObjectKey{Name: vscName}, &got); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	if got.Annotations[AllowVolumeModeChangeAnnotation] != "true" {
		t.Errorf("annotation lost: %q", got.Annotations[AllowVolumeModeChangeAnnotation])
	}
	if got.Annotations["unrelated.example.com/leave-me"] != "alone" {
		t.Errorf("unrelated annotation altered: %q", got.Annotations["unrelated.example.com/leave-me"])
	}
}

// TestEnsureAllowVolumeModeChange_VSCNotFoundIsNoOp covers the CSI race
// where status.boundVolumeSnapshotContentName has been set but the VSC
// is not yet visible to the controller's cached client. The controller
// must not surface this as a hard error — the next reconcile will see
// the VSC and patch it.
func TestEnsureAllowVolumeModeChange_VSCNotFoundIsNoOp(t *testing.T) {
	r := newReconciler(t)
	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "img-clone-seed", Namespace: "default"},
		Status: &volumesnapshotv1.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: ptr.To("snapcontent-not-yet-cached"),
		},
	}
	if err := r.ensureAllowVolumeModeChange(context.Background(), snap); err != nil {
		t.Fatalf("VSC NotFound should be a no-op (retry on next reconcile); got err=%v", err)
	}
}

// TestEnsureAllowVolumeModeChange_PreservesExistingAnnotations confirms
// the patch only adds the W9.x annotation; any other annotations on the
// VSC (e.g. snapshotter-internal annotations) are preserved.
func TestEnsureAllowVolumeModeChange_PreservesExistingAnnotations(t *testing.T) {
	vscName := "snapcontent-with-other-annotations"
	vsc := &volumesnapshotv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name: vscName,
			Annotations: map[string]string{
				"snapshot.storage.kubernetes.io/deletion-secret-name":      "secret",
				"snapshot.storage.kubernetes.io/deletion-secret-namespace": "kube-system",
			},
		},
	}
	r := newReconciler(t, vsc)

	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "img-clone-seed", Namespace: "default"},
		Status: &volumesnapshotv1.VolumeSnapshotStatus{
			BoundVolumeSnapshotContentName: ptr.To(vscName),
		},
	}
	if err := r.ensureAllowVolumeModeChange(context.Background(), snap); err != nil {
		t.Fatalf("ensureAllowVolumeModeChange: %v", err)
	}

	var got volumesnapshotv1.VolumeSnapshotContent
	if err := r.Get(context.Background(), client.ObjectKey{Name: vscName}, &got); err != nil {
		t.Fatalf("get VSC: %v", err)
	}
	if got.Annotations[AllowVolumeModeChangeAnnotation] != "true" {
		t.Errorf("W9.x annotation missing post-patch")
	}
	if got.Annotations["snapshot.storage.kubernetes.io/deletion-secret-name"] != "secret" {
		t.Errorf("deletion-secret-name annotation lost")
	}
	if got.Annotations["snapshot.storage.kubernetes.io/deletion-secret-namespace"] != "kube-system" {
		t.Errorf("deletion-secret-namespace annotation lost")
	}
}
