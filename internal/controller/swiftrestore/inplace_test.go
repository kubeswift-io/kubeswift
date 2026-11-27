package swiftrestore

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/rootdisk"
)

// in-place fixtures: guest g1 running from swiftguest-root-g1 (controlled by
// the guest), its launcher g1, a CSI snapshot snap1 of it, and restore r1 onto
// g1 with overwriteExisting.
func inPlaceFixture() (*snapshotv1alpha1.SwiftRestore, []client.Object) {
	restore := makeRestore("r1", "default", "snap1", "g1", true)
	restore.Spec.TargetGuest.OverwriteExisting = true
	prev := makeBoundSourcePVC("default", "g1", "sc")
	prev.OwnerReferences = guestControllerRef("g1")
	return restore, []client.Object{
		restore,
		makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30),
		runningGuest("g1"),
		prev,
		launcherPod("g1", "swiftguest-root-g1", time.Now().Add(-time.Hour)),
	}
}

func launcherPod(name, claim string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			CreationTimestamp: metav1.NewTime(created),
			OwnerReferences:   guestControllerRef("g1"),
		},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name:         launcherRootVolume,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func bindPVC(t *testing.T, c client.Client, name string) {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &pvc); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := c.Status().Update(context.Background(), &pvc); err != nil {
		t.Fatalf("bind %s: %v", name, err)
	}
}

func getGuest(t *testing.T, c client.Client) *swiftv1alpha1.SwiftGuest {
	t.Helper()
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &g); err != nil {
		t.Fatalf("get guest: %v", err)
	}
	return &g
}

func pvcExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &pvc)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// swapToResuming drives the fixture's restore through the swap.
func swapToResuming(t *testing.T, r *SwiftRestoreReconciler, c client.Client) {
	t.Helper()
	reconcile(t, r, "r1", "default") // Pending -> Restoring
	reconcile(t, r, "r1", "default") // record the swap
	rd := get(t, c, "r1", "default").Status.RootDisk
	if rd == nil || rd.ClaimName != "swiftguest-root-g1-g1" || rd.PreviousClaimName != "swiftguest-root-g1" {
		t.Fatalf("status.rootDisk = %+v", rd)
	}
	reconcile(t, r, "r1", "default") // seed the PVC
	if !pvcExists(t, c, "swiftguest-root-g1-g1") {
		t.Fatal("restored PVC not created")
	}
	// Not Bound: the guest keeps running on its disk.
	if getGuest(t, c).Annotations[snapshotv1alpha1.AnnotationDiskRestore] != "" {
		t.Fatal("guest held before the restored disk is Bound")
	}
	bindPVC(t, c, "swiftguest-root-g1-g1")
	reconcile(t, r, "r1", "default") // hold + delete launcher
	if getGuest(t, c).Annotations[snapshotv1alpha1.AnnotationDiskRestore] != "r1" {
		t.Fatal("guest not held")
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &pod); !apierrors.IsNotFound(err) {
		t.Fatalf("launcher not deleted: %v", err)
	}
	reconcile(t, r, "r1", "default") // swap
	if got := get(t, c, "r1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseResuming || !got.Status.RootDisk.Switched {
		t.Fatalf("after swap: phase %s, rootDisk %+v", got.Status.Phase, got.Status.RootDisk)
	}
}

func TestInPlaceCSI_SwapsTheRootDiskAndKeepsTheOldOneUntilReady(t *testing.T) {
	_, objs := inPlaceFixture()
	r, c := newReconciler(t, objs...)
	swapToResuming(t, r, c)

	g := getGuest(t, c)
	if g.Annotations[snapshotv1alpha1.AnnotationDiskRestore] != "" {
		t.Error("guest still held after the swap")
	}
	name, gen, err := rootdisk.Claim(context.Background(), c, g)
	if err != nil || name != "swiftguest-root-g1-g1" || gen != 1 {
		t.Fatalf("guest's root disk = %q gen %d (%v), want swiftguest-root-g1-g1 gen 1", name, gen, err)
	}
	var pvc corev1.PersistentVolumeClaim
	_ = c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &pvc)
	if !metav1.IsControlledBy(&pvc, g) {
		t.Errorf("restored PVC owners = %+v, want the guest as controller", pvc.OwnerReferences)
	}
	if !pvcExists(t, c, "swiftguest-root-g1") {
		t.Fatal("previous root disk deleted before the restore is Ready")
	}

	// The guest controller starts a launcher on the new disk, and it runs.
	started := time.Now()
	if err := c.Create(context.Background(), launcherPod("g1", "swiftguest-root-g1-g1", started)); err != nil {
		t.Fatal(err)
	}
	g.Status.Conditions = []metav1.Condition{{Type: conditionGuestRunning, Status: metav1.ConditionTrue, Reason: "VmRunning",
		LastTransitionTime: metav1.NewTime(started.Add(time.Second))}}
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "r1", "default")
	if got := get(t, c, "r1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseReady {
		t.Fatalf("phase = %s (%s), want Ready", got.Status.Phase, msgOrEmpty(findReady(got)))
	}
	if pvcExists(t, c, "swiftguest-root-g1") {
		t.Error("previous root disk kept after Ready")
	}
}

// GuestRunning left from the launcher the restore deleted must not complete
// the restore.
func TestInPlaceCSI_StaleGuestRunningDoesNotComplete(t *testing.T) {
	_, objs := inPlaceFixture()
	r, c := newReconciler(t, objs...)
	swapToResuming(t, r, c)

	started := time.Now()
	if err := c.Create(context.Background(), launcherPod("g1", "swiftguest-root-g1-g1", started)); err != nil {
		t.Fatal(err)
	}
	g := getGuest(t, c)
	g.Status.Conditions = []metav1.Condition{{Type: conditionGuestRunning, Status: metav1.ConditionTrue, Reason: "VmRunning",
		LastTransitionTime: metav1.NewTime(started.Add(-time.Minute))}}
	if err := c.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "r1", "default")
	if got := get(t, c, "r1", "default"); got.Status.Phase != snapshotv1alpha1.SwiftRestorePhaseResuming {
		t.Fatalf("phase = %s, want Resuming", got.Status.Phase)
	}
	if !pvcExists(t, c, "swiftguest-root-g1") {
		t.Error("previous root disk deleted on a stale GuestRunning")
	}
}

// A guest that fails to start from the restored disk goes back to the previous
// one: the restored PVC and its launcher are deleted.
func TestInPlaceCSI_FailureAfterTheSwapRollsBack(t *testing.T) {
	_, objs := inPlaceFixture()
	r, c := newReconciler(t, objs...)
	swapToResuming(t, r, c)

	pod := launcherPod("g1", "swiftguest-root-g1-g1", time.Now())
	pod.Status.Phase = corev1.PodFailed
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "r1", "default")
	wantFailedWith(t, c, ReasonRestoreFailed)
	if pvcExists(t, c, "swiftguest-root-g1-g1") {
		t.Error("restored PVC kept after the rollback")
	}
	if !pvcExists(t, c, "swiftguest-root-g1") {
		t.Fatal("previous root disk lost")
	}
	var got corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "g1", Namespace: "default"}, &got); !apierrors.IsNotFound(err) {
		t.Errorf("launcher on the restored disk not deleted: %v", err)
	}
	name, _, _ := rootdisk.Claim(context.Background(), c, getGuest(t, c))
	if name != "swiftguest-root-g1" {
		t.Errorf("guest's root disk after rollback = %s", name)
	}
	if getGuest(t, c).Annotations[snapshotv1alpha1.AnnotationDiskRestore] != "" {
		t.Error("guest still held after the rollback")
	}
}

// A failure before the swap leaves the guest on its disk and releases it.
func TestInPlaceCSI_FailureBeforeTheSwapReleasesTheGuest(t *testing.T) {
	_, objs := inPlaceFixture()
	r, c := newReconciler(t, objs...)
	reconcile(t, r, "r1", "default")
	reconcile(t, r, "r1", "default")
	reconcile(t, r, "r1", "default")
	bindPVC(t, c, "swiftguest-root-g1-g1")
	reconcile(t, r, "r1", "default") // held, launcher deleted
	// The snapshot goes away mid-restore.
	var snap snapshotv1alpha1.SwiftSnapshot
	_ = c.Get(context.Background(), client.ObjectKey{Name: "snap1", Namespace: "default"}, &snap)
	if err := c.Delete(context.Background(), &snap); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r, "r1", "default")
	wantFailedWith(t, c, ReasonRestoreFailed)
	if pvcExists(t, c, "swiftguest-root-g1-g1") {
		t.Error("unswapped restored PVC kept")
	}
	if getGuest(t, c).Annotations[snapshotv1alpha1.AnnotationDiskRestore] != "" {
		t.Error("guest still held")
	}
	if name, _, _ := rootdisk.Claim(context.Background(), c, getGuest(t, c)); name != "swiftguest-root-g1" {
		t.Errorf("root disk = %s", name)
	}
}

func TestInPlaceCSI_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*snapshotv1alpha1.SwiftRestore, *swiftv1alpha1.SwiftGuest)
		reason string
	}{
		{"no overwriteExisting", func(r *snapshotv1alpha1.SwiftRestore, _ *swiftv1alpha1.SwiftGuest) {
			r.Spec.TargetGuest.OverwriteExisting = false
		}, ReasonTargetConflict},
		{"resumeAfterRestore false", func(r *snapshotv1alpha1.SwiftRestore, _ *swiftv1alpha1.SwiftGuest) {
			r.Spec.ResumeAfterRestore = false
		}, ReasonInPlaceUnsupported},
		{"kernel boot", func(_ *snapshotv1alpha1.SwiftRestore, g *swiftv1alpha1.SwiftGuest) {
			g.Spec.KernelRef = &corev1.LocalObjectReference{Name: "k"}
		}, ReasonInPlaceUnsupported},
		{"shared-base disk", func(_ *snapshotv1alpha1.SwiftRestore, g *swiftv1alpha1.SwiftGuest) {
			g.Status.SharedBaseDisk = &swiftv1alpha1.SharedBaseDiskStatus{}
		}, ReasonInPlaceUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := makeRestore("r1", "default", "snap1", "g1", true)
			restore.Spec.TargetGuest.OverwriteExisting = true
			g := runningGuest("g1")
			tc.mutate(restore, g)
			r, c := newReconciler(t, restore, makeReadySnapshot("snap1", "default", "g1", "vs1", 40<<30), g)
			reconcile(t, r, "r1", "default")
			wantFailedWith(t, c, tc.reason)
			if getGuest(t, c).Annotations[snapshotv1alpha1.AnnotationDiskRestore] != "" {
				t.Error("refused restore held the guest")
			}
		})
	}
}

func TestInPlaceCSI_WaitsForAMigration(t *testing.T) {
	_, objs := inPlaceFixture()
	g := objs[2].(*swiftv1alpha1.SwiftGuest)
	g.Annotations = map[string]string{migrationv1alpha1.AnnotationMigrationInProgress: "m1"}
	mig := &migrationv1alpha1.SwiftMigration{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Status: migrationv1alpha1.SwiftMigrationStatus{Phase: migrationv1alpha1.SwiftMigrationPhasePreparing}}
	r, c := newReconciler(t, append(objs, mig)...)
	reconcile(t, r, "r1", "default")
	got := get(t, c, "r1", "default")
	if got.Status.Phase != snapshotv1alpha1.SwiftRestorePhasePending || reasonOrEmpty(findReady(got)) != ReasonGuestBusy {
		t.Fatalf("phase %s reason %s, want Pending GuestBusy", got.Status.Phase, reasonOrEmpty(findReady(got)))
	}
}
