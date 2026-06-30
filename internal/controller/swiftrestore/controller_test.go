package swiftrestore

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	swiftguestctrl "github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
)

func testScheme(t *testing.T) *runtime.Scheme {
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
	gvSwift := schema.GroupVersion{Group: "swift.kubeswift.io", Version: "v1alpha1"}
	s.AddKnownTypes(gvSwift,
		&swiftv1alpha1.SwiftGuest{}, &swiftv1alpha1.SwiftGuestList{},
	)
	metav1.AddToGroupVersion(s, gvSnap)
	metav1.AddToGroupVersion(s, gvSwift)
	return s
}

func newReconciler(t *testing.T, objs ...client.Object) (*SwiftRestoreReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&snapshotv1alpha1.SwiftRestore{}).
		Build()
	return &SwiftRestoreReconciler{Client: c, Scheme: scheme}, c
}

func makeReadySnapshot(name, ns, sourceGuest, vsName string, sizeBytes int64) *snapshotv1alpha1.SwiftSnapshot {
	return &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftSnapshotSpec{
			GuestRef: snapshotv1alpha1.SwiftSnapshotGuestRef{Name: sourceGuest},
			Backend: snapshotv1alpha1.SwiftSnapshotBackend{
				Type: snapshotv1alpha1.SnapshotBackendCSIVolumeSnapshot,
			},
		},
		Status: snapshotv1alpha1.SwiftSnapshotStatus{
			Phase: snapshotv1alpha1.SwiftSnapshotPhaseReady,
			Disks: []snapshotv1alpha1.SnapshotDiskRef{{
				Role:      "root",
				SizeBytes: sizeBytes,
				Handle:    ns + "/" + vsName,
			}},
		},
	}
}

func makeRestore(name, ns, snapName, targetName string, resume bool) *snapshotv1alpha1.SwiftRestore {
	return &snapshotv1alpha1.SwiftRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: snapshotv1alpha1.SwiftRestoreSpec{
			SnapshotRef:        snapshotv1alpha1.SwiftRestoreSnapshotRef{Name: snapName},
			TargetGuest:        snapshotv1alpha1.SwiftRestoreTarget{Name: targetName},
			ResumeAfterRestore: resume,
		},
	}
}

func makeSourceGuest(ns, name, image string) *swiftv1alpha1.SwiftGuest {
	return &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: swiftv1alpha1.SwiftGuestSpec{
			ImageRef:      &corev1.LocalObjectReference{Name: image},
			GuestClassRef: corev1.LocalObjectReference{Name: "default-class"},
			RunPolicy:     swiftv1alpha1.RunPolicyRunning,
		},
	}
}

func makeBoundSourcePVC(ns, sourceGuest, storageClass string) *corev1.PersistentVolumeClaim {
	sc := storageClass
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: rootPVCName(sourceGuest), Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("40Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func reconcile(t *testing.T, r *SwiftRestoreReconciler, name, ns string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name, Namespace: ns}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func get(t *testing.T, c client.Client, name, ns string) *snapshotv1alpha1.SwiftRestore {
	t.Helper()
	var s snapshotv1alpha1.SwiftRestore
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: ns}, &s); err != nil {
		t.Fatalf("get SwiftRestore: %v", err)
	}
	return &s
}

func TestPending_SnapshotMissing_StaysPending(t *testing.T) {
	restore := makeRestore("r1", "default", "missing", "target", true)
	r, c := newReconciler(t, restore)

	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhasePending {
		t.Errorf("phase = %s, want Pending (snapshot may appear later)", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonSnapshotNotFound {
		t.Errorf("Ready reason = %q, want SnapshotNotFound", reasonOrEmpty(cond))
	}
}

func TestPending_SnapshotNotReady_StaysPending(t *testing.T) {
	snap := makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30)
	snap.Status.Phase = snapshotv1alpha1.SwiftSnapshotPhaseCapturing
	restore := makeRestore("r1", "default", "snap1", "target", true)
	r, c := newReconciler(t, restore, snap)

	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhasePending {
		t.Errorf("phase = %s, want Pending (snapshot still Capturing)", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonSnapshotNotReady {
		t.Errorf("Ready reason = %q, want SnapshotNotReady", reasonOrEmpty(cond))
	}
}

func TestPending_TargetExists_NoOverwrite_FlipsToFailed(t *testing.T) {
	snap := makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30)
	source := makeSourceGuest("default", "g1", "ubuntu-noble")
	existingTarget := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "default"},
		Spec:       swiftv1alpha1.SwiftGuestSpec{GuestClassRef: corev1.LocalObjectReference{Name: "x"}},
	}
	restore := makeRestore("r1", "default", "snap1", "target", true)
	r, c := newReconciler(t, restore, snap, source, existingTarget)

	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseFailed {
		t.Errorf("phase = %s, want Failed", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Reason != ReasonTargetConflict {
		t.Errorf("Ready reason = %q, want TargetConflict", reasonOrEmpty(cond))
	}
}

// Drives Pending -> Restoring -> creates restore PVC + SwiftGuest -> Resuming.
func TestRestore_HappyPath_CreatesPVCAndGuest(t *testing.T) {
	snap := makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30)
	source := makeSourceGuest("default", "g1", "ubuntu-noble")
	sourcePVC := makeBoundSourcePVC("default", "g1", "longhorn")
	restore := makeRestore("r1", "default", "snap1", "target", true)
	r, c := newReconciler(t, restore, snap, source, sourcePVC)

	// 1: Pending -> Restoring
	reconcile(t, r, "r1", "default")
	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseRestoring {
		t.Fatalf("after first reconcile phase = %s, want Restoring", got.Status.Phase)
	}

	// 2: Restoring creates the PVC; not yet Bound -> stays Restoring.
	reconcile(t, r, "r1", "default")
	var pvc corev1.PersistentVolumeClaim
	pvcName := rootPVCName("target")
	if err := c.Get(context.Background(), client.ObjectKey{Name: pvcName, Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("restore PVC missing: %v", err)
	}
	if pvc.Labels[swiftguestctrl.RestoreSeededLabel] != "true" {
		t.Errorf("restore PVC label %s missing", swiftguestctrl.RestoreSeededLabel)
	}
	if pvc.Spec.DataSource == nil || pvc.Spec.DataSource.Name != "vs1" || pvc.Spec.DataSource.Kind != "VolumeSnapshot" {
		t.Errorf("restore PVC dataSource = %v, want VolumeSnapshot/vs1", pvc.Spec.DataSource)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "longhorn" {
		t.Errorf("restore PVC storage class = %v, want longhorn", pvc.Spec.StorageClassName)
	}

	// Mark Bound and reconcile again -> Resuming.
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(context.Background(), &pvc); err != nil {
		t.Fatalf("update PVC status: %v", err)
	}
	reconcile(t, r, "r1", "default")

	var target swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "target", Namespace: "default"}, &target); err != nil {
		t.Fatalf("target SwiftGuest missing: %v", err)
	}
	if target.Spec.ImageRef == nil || target.Spec.ImageRef.Name != "ubuntu-noble" {
		t.Errorf("target.imageRef = %v, want ubuntu-noble", target.Spec.ImageRef)
	}
	if target.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("target.runPolicy = %s, want Running (inherited from source)", target.Spec.RunPolicy)
	}

	got = get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseResuming {
		t.Fatalf("phase = %s, want Resuming", got.Status.Phase)
	}
	if got.Status.GuestRef == nil || got.Status.GuestRef.Name != "target" {
		t.Errorf("status.guestRef = %v, want {Name: target}", got.Status.GuestRef)
	}
}

// ResumeAfterRestore=false skips Resuming and forces target.runPolicy=Stopped.
func TestRestore_NoResume_GoesStraightToReady_AndStopsTarget(t *testing.T) {
	snap := makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30)
	source := makeSourceGuest("default", "g1", "ubuntu-noble")
	sourcePVC := makeBoundSourcePVC("default", "g1", "longhorn")
	restore := makeRestore("r1", "default", "snap1", "target", false)
	r, c := newReconciler(t, restore, snap, source, sourcePVC)

	reconcile(t, r, "r1", "default") // Pending -> Restoring

	reconcile(t, r, "r1", "default") // Restoring creates PVC (still pending bind)

	var pvc corev1.PersistentVolumeClaim
	pvcName := rootPVCName("target")
	if err := c.Get(context.Background(), client.ObjectKey{Name: pvcName, Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("restore PVC missing: %v", err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(context.Background(), &pvc); err != nil {
		t.Fatalf("update PVC status: %v", err)
	}
	reconcile(t, r, "r1", "default") // PVC bound, target created, jump to Ready

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseReady {
		t.Fatalf("phase = %s, want Ready (resume=false skips Resuming)", got.Status.Phase)
	}
	if got.Status.CompletedAt == nil {
		t.Errorf("CompletedAt not set on Ready")
	}

	var target swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "target", Namespace: "default"}, &target); err != nil {
		t.Fatalf("target missing: %v", err)
	}
	if target.Spec.RunPolicy != swiftv1alpha1.RunPolicyStopped {
		t.Errorf("target.runPolicy = %s, want Stopped", target.Spec.RunPolicy)
	}
}

func TestResuming_GuestRunning_FlipsToReady(t *testing.T) {
	target := makeSourceGuest("default", "target", "ubuntu-noble")
	target.Status.Conditions = []metav1.Condition{{
		Type:               "GuestRunning",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "Running",
	}}
	restore := makeRestore("r1", "default", "snap1", "target", true)
	restore.Status.Phase = snapshotv1alpha1.SwiftRestorePhaseResuming
	r, c := newReconciler(t, restore, target)

	reconcile(t, r, "r1", "default")

	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseReady {
		t.Errorf("phase = %s, want Ready (target Running)", got.Status.Phase)
	}
	if cond := findReady(got); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready cond = %v, want True", cond)
	}
}

func TestTerminalPhases_NoOp(t *testing.T) {
	for _, phase := range []snapshotv1alpha1.SwiftRestorePhase{
		snapshotv1alpha1.SwiftRestorePhaseReady,
		snapshotv1alpha1.SwiftRestorePhaseFailed,
	} {
		restore := makeRestore("r1", "default", "snap1", "target", true)
		restore.Status.Phase = phase
		r, c := newReconciler(t, restore)

		reconcile(t, r, "r1", "default")

		got := get(t, c, "r1", "default")
		if got.Status.Phase != phase {
			t.Errorf("terminal %s mutated to %s", phase, got.Status.Phase)
		}
	}
}

func TestSnapshotHandle_Parse(t *testing.T) {
	cases := []struct {
		in       string
		ns, name string
		ok       bool
	}{
		{"default/vs1", "default", "vs1", true},
		{"prod/swift-snap-foo", "prod", "swift-snap-foo", true},
		{"", "", "", false},
		{"justname", "", "", false},
		{"/nons", "", "", false},
		{"noname/", "", "", false},
	}
	for _, tc := range cases {
		ns, name, ok := SnapshotHandle(tc.in)
		if ok != tc.ok || ns != tc.ns || name != tc.name {
			t.Errorf("SnapshotHandle(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.in, ns, name, ok, tc.ns, tc.name, tc.ok)
		}
	}
}

func findReady(restore *snapshotv1alpha1.SwiftRestore) *metav1.Condition {
	for i := range restore.Status.Conditions {
		if restore.Status.Conditions[i].Type == snapshotv1alpha1.SwiftRestoreConditionReady {
			return &restore.Status.Conditions[i]
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

func msgOrEmpty(c *metav1.Condition) string {
	if c == nil {
		return ""
	}
	return c.Message
}
