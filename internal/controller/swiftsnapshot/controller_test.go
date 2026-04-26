package swiftsnapshot

import (
	"context"
	"testing"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme: %v", err)
	}
	if err := volumesnapshotv1.AddToScheme(s); err != nil {
		t.Fatalf("volumesnapshotv1: %v", err)
	}
	gvSnap := schema.GroupVersion{Group: "snapshot.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSnap,
		&snapshotv1alpha1.SwiftSnapshot{}, &snapshotv1alpha1.SwiftSnapshotList{},
		&snapshotv1alpha1.SwiftRestore{}, &snapshotv1alpha1.SwiftRestoreList{},
	)
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift,
		&swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{},
	)
	metav1.AddToGroupVersion(s, gvSnap)
	metav1.AddToGroupVersion(s, gvSwift)
	return s
}

func newReconciler(t *testing.T, objs ...client.Object) (*SwiftSnapshotReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.SwiftSnapshot{}).
		Build()
	return &SwiftSnapshotReconciler{Client: c, Scheme: scheme}, c
}

func makeSwiftSnapshot(name, ns, guestName, vsClass string) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: guestName},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type:              snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot,
				CSIVolumeSnapshot: &snapshotv1alpha1.CSIVolumeSnapshotBackend{VolumeSnapshotClassName: vsClass},
			},
		},
	}
}

func makeBoundClonePVC(ns, guestName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: rootPVCName(guestName), Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func makeGuest(ns, name string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef: &corev1.LocalObjectReference{Name: "ubuntu-noble"},
		},
	}
}

func reconcile(t *testing.T, r *SwiftSnapshotReconciler, name, ns string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name, Namespace: ns}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func get(t *testing.T, c client.Client, name, ns string) *snapshotv1alpha1.SwiftSnapshot {
	t.Helper()
	var s snapshotv1alpha1.SwiftSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &s); err != nil {
		t.Fatalf("get SwiftSnapshot: %v", err)
	}
	return &s
}

func TestPending_UnsupportedBackend_FlipsToFailed(t *testing.T) {
	// Phase 2: csi-volume-snapshot and local are wired up. S3 remains
	// reserved and is the only backend that should hit the controller's
	// UnsupportedBackend path. (The webhook rejects S3 too, but defense
	// in depth: existing pre-Phase-2 SwiftSnapshot resources can sneak
	// through if upgraded in place.)
	snap := makeSwiftSnapshot("snap1", "default", "g1", "")
	snap.Spec.Backend.Type = snapshotv1alpha1.SnapshotBackendS3

	r, c := newReconciler(t, snap)
	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Errorf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonUnsupportedBackend {
		t.Errorf("Ready condition reason = %q, want UnsupportedBackend", reasonOrEmpty(cond))
	}
}

func TestPending_GuestNotFound_StaysPending(t *testing.T) {
	snap := makeSwiftSnapshot("snap1", "default", "missing", "")
	r, c := newReconciler(t, snap)

	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending {
		t.Errorf("phase = %s, want Pending (guest may appear later)", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonGuestNotFound {
		t.Errorf("Ready condition reason = %q, want GuestNotFound", reasonOrEmpty(cond))
	}
}

func TestPending_RootPVCMissing_StaysPending(t *testing.T) {
	snap := makeSwiftSnapshot("snap1", "default", "g1", "")
	guest := makeGuest("default", "g1")
	r, c := newReconciler(t, snap, guest)

	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhasePending {
		t.Errorf("phase = %s, want Pending (no root-disk PVC yet)", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonRootPVCNotFound {
		t.Errorf("Ready condition reason = %q, want RootPVCNotFound", reasonOrEmpty(cond))
	}
}

func TestPending_AdvancesToCapturing_AndCreatesVolumeSnapshot(t *testing.T) {
	snap := makeSwiftSnapshot("snap1", "default", "g1", "csi-hostpath-snapclass")
	guest := makeGuest("default", "g1")
	pvc := makeBoundClonePVC("default", "g1")
	r, c := newReconciler(t, snap, guest, pvc)

	// First reconcile: Pending -> Capturing (status persisted).
	reconcile(t, r, "snap1", "default")
	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Errorf("phase = %s, want Capturing after first reconcile", got.Status.Phase)
	}
	if got.Status.GuestSpec == nil || got.Status.GuestSpec.ImageName != "ubuntu-noble" {
		t.Errorf("captured guest spec ImageName = %v, want ubuntu-noble", got.Status.GuestSpec)
	}

	// Second reconcile: handleCapturing creates the VolumeSnapshot.
	reconcile(t, r, "snap1", "default")

	var vs volumesnapshotv1.VolumeSnapshot
	if err := c.Get(context.Background(), client.ObjectKey{Name: VolumeSnapshotName("snap1"), Namespace: "default"}, &vs); err != nil {
		t.Fatalf("VolumeSnapshot not created on second reconcile: %v", err)
	}
	if vs.Spec.VolumeSnapshotClassName == nil || *vs.Spec.VolumeSnapshotClassName != "csi-hostpath-snapclass" {
		t.Errorf("VolumeSnapshotClassName = %v, want csi-hostpath-snapclass", vs.Spec.VolumeSnapshotClassName)
	}
	if vs.Spec.Source.PersistentVolumeClaimName == nil || *vs.Spec.Source.PersistentVolumeClaimName != rootPVCName("g1") {
		t.Errorf("source PVC = %v, want %s", vs.Spec.Source.PersistentVolumeClaimName, rootPVCName("g1"))
	}
}

func TestCapturing_SnapshotNotReady_StaysCapturing(t *testing.T) {
	snap := makeSwiftSnapshot("snap1", "default", "g1", "csi-class")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	guest := makeGuest("default", "g1")
	pvc := makeBoundClonePVC("default", "g1")
	vs := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: VolumeSnapshotName("snap1"), Namespace: "default"},
		Status: &volumesnapshotv1.VolumeSnapshotStatus{
			ReadyToUse: ptr.To(false),
		},
	}
	r, c := newReconciler(t, snap, guest, pvc, vs)

	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseCapturing {
		t.Errorf("phase = %s, want Capturing (snapshot not yet ready)", got.Status.Phase)
	}
}

func TestCapturing_SnapshotReady_FlipsToReady(t *testing.T) {
	snap := makeSwiftSnapshot("snap1", "default", "g1", "csi-class")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	guest := makeGuest("default", "g1")
	pvc := makeBoundClonePVC("default", "g1")
	restoreSize := resource.MustParse("40Gi")
	vs := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: VolumeSnapshotName("snap1"), Namespace: "default"},
		Status: &volumesnapshotv1.VolumeSnapshotStatus{
			ReadyToUse:  ptr.To(true),
			RestoreSize: &restoreSize,
		},
	}
	r, c := newReconciler(t, snap, guest, pvc, vs)

	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Status.Phase)
	}
	if got.Status.CapturedAt == nil {
		t.Errorf("CapturedAt not set on Ready transition")
	}
	if len(got.Status.Disks) != 1 || got.Status.Disks[0].Role != "root" {
		t.Errorf("disks = %+v, want one root disk", got.Status.Disks)
	}
	wantHandle := "default/" + VolumeSnapshotName("snap1")
	if got.Status.Disks[0].Handle != wantHandle {
		t.Errorf("disk handle = %q, want %q", got.Status.Disks[0].Handle, wantHandle)
	}
	if got.Status.Disks[0].SizeBytes != restoreSize.Value() {
		t.Errorf("disk size = %d, want %d", got.Status.Disks[0].SizeBytes, restoreSize.Value())
	}
	if got.Status.TotalSizeBytes != restoreSize.Value() {
		t.Errorf("total size = %d, want %d", got.Status.TotalSizeBytes, restoreSize.Value())
	}
	if cond := findReady(got); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition = %v, want True", cond)
	}
}

func TestCapturing_SnapshotErrored_FlipsToFailed(t *testing.T) {
	snap := makeSwiftSnapshot("snap1", "default", "g1", "csi-class")
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	guest := makeGuest("default", "g1")
	pvc := makeBoundClonePVC("default", "g1")
	errMsg := "disk too small for snapshot"
	vs := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: VolumeSnapshotName("snap1"), Namespace: "default"},
		Status: &volumesnapshotv1.VolumeSnapshotStatus{
			Error:      &volumesnapshotv1.VolumeSnapshotError{Message: &errMsg},
			ReadyToUse: ptr.To(false),
		},
	}
	r, c := newReconciler(t, snap, guest, pvc, vs)

	reconcile(t, r, "snap1", "default")

	got := get(t, c, "snap1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftSnapshotPhaseFailed {
		t.Errorf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonSnapshotFailed {
		t.Errorf("Ready condition reason = %q, want SnapshotFailed", reasonOrEmpty(cond))
	}
}

func TestTerminalPhases_NoOp(t *testing.T) {
	for _, phase := range []snapshotv1alpha1.SwiftSnapshotPhase{
		snapshotv1alpha1.SwiftSnapshotPhaseReady,
		snapshotv1alpha1.SwiftSnapshotPhaseFailed,
	} {
		snap := makeSwiftSnapshot("snap1", "default", "g1", "csi-class")
		snap.Status.Phase = phase
		r, c := newReconciler(t, snap)

		reconcile(t, r, "snap1", "default")

		got := get(t, c, "snap1", "default")
		if got.Status.Phase != phase {
			t.Errorf("terminal phase %s mutated to %s", phase, got.Status.Phase)
		}
	}
}

func findReady(snap *snapshotv1alpha1.SwiftSnapshot) *metav1.Condition {
	for i := range snap.Status.Conditions {
		if snap.Status.Conditions[i].Type == snapshotv1alpha1.SwiftSnapshotConditionReady {
			return &snap.Status.Conditions[i]
		}
	}
	return nil
}

func reasonOrEmpty(c *metav1.Condition) string {
	if c == nil {
		return ""
	}
	return c.Reason
}
