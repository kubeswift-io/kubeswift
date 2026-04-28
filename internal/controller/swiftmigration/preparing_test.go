package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// preparingScheme adds storage.k8s.io/v1 (VolumeAttachment) on top of
// the validating-test scheme. The fake client needs the type
// registered to list VolumeAttachments.
func preparingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := validatingScheme(t)
	gvStorage := schema.GroupVersion{Group: "storage.k8s.io", Version: "v1"}
	s.AddKnownTypes(gvStorage, &storagev1.VolumeAttachment{}, &storagev1.VolumeAttachmentList{})
	metav1.AddToGroupVersion(s, gvStorage)
	return s
}

func newPVCBoundTo(name, namespace, pvName string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
}

func newVolumeAttachment(name, pvName, nodeName string) *storagev1.VolumeAttachment {
	pv := pvName
	return &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: "driver.longhorn.io",
			NodeName: nodeName,
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pv},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
}

func newLauncherPod(guestName, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: guestName, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName: "boba",
			Containers: []corev1.Container{
				{Name: "launcher", Image: "ghcr.io/projectbeskar/kubeswift/swiftletd:test"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestPreparing_FirstEntry_ClaimsGuestAndDeletesPod verifies the first
// reconcile in Preparing patches the SwiftGuest with the migration-
// in-progress annotation AND runPolicy=Stopped (single combined patch
// per architect Q3 Option A), then issues Delete(pod) with grace.
// Returns advanced=false because the pod is still terminating.
func TestPreparing_FirstEntry_ClaimsGuestAndDeletesPod(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	pod := newLauncherPod("guest", "default")
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, requeue, errMsg, err := r.handlePreparing(context.Background(), mig, status)
	if err != nil {
		t.Fatalf("handlePreparing returned err = %v", err)
	}
	if errMsg != "" {
		t.Fatalf("handlePreparing returned errMsg = %q", errMsg)
	}
	if advanced {
		t.Error("handlePreparing should not advance on first entry (pod still terminating)")
	}
	if requeue == 0 {
		t.Error("handlePreparing should request a requeue while polling")
	}

	// SwiftGuest should be patched: annotation set, runPolicy=Stopped.
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest after Preparing: %v", err)
	}
	if got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] != "m" {
		t.Errorf("annotation = %q, want %q", got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress], "m")
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyStopped {
		t.Errorf("runPolicy = %q, want Stopped", got.Spec.RunPolicy)
	}

	// Pod should be deleted (gone from the fake client, since fake
	// client deletes immediately without grace simulation).
	var podGot corev1.Pod
	err = c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &podGot)
	if err == nil && podGot.DeletionTimestamp == nil {
		t.Error("pod should be deleted (or have DeletionTimestamp set)")
	}
}

// TestPreparing_AnnotationConflict_RejectedClearly verifies that when
// the SwiftGuest already carries an in-progress annotation for a
// different SwiftMigration, this migration fails with a clear error
// (architect Q3 mitigation: two-operator-concurrent-migration race)
// AND the source guest's runPolicy is NOT touched. The conflict
// check must short-circuit before the combined patch lands —
// otherwise we'd corrupt the other migration in-flight.
func TestPreparing_AnnotationConflict_RejectedClearly(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = "other-migration"
	// Pre-set runPolicy=Running. After the conflict rejection,
	// runPolicy must remain Running (we did NOT touch the guest).
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handlePreparing(context.Background(), mig, status)
	if !strings.Contains(errMsg, "another SwiftMigration") || !strings.Contains(errMsg, "other-migration") {
		t.Errorf("conflict error should name the other migration; got %q", errMsg)
	}
	// Critical: the conflict check fires BEFORE the patch. Verify
	// the source guest's runPolicy is unchanged.
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest after rejection: %v", err)
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("conflict path corrupted runPolicy: got %q, want Running (the conflict check must short-circuit before the patch)", got.Spec.RunPolicy)
	}
	if got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] != "other-migration" {
		t.Errorf("conflict path overwrote other migration's annotation: got %q, want %q",
			got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress], "other-migration")
	}
}

// TestPreparing_NilAnnotationsMap is a defensive test for the case
// where SwiftGuest.Annotations is nil (rather than an empty map).
// The first-entry path must initialise the map before writing to it.
func TestPreparing_NilAnnotationsMap(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Annotations = nil // nil map, not empty map
	pod := newLauncherPod("guest", "default")
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, err := r.handlePreparing(context.Background(), mig, status)
	if err != nil || errMsg != "" {
		t.Fatalf("nil annotations map should be handled gracefully; err=%v errMsg=%q", err, errMsg)
	}
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	if got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] != "m" {
		t.Errorf("annotation = %q, want %q", got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress], "m")
	}
}

// TestPreparing_PodHasDeletionTimestamp_DoesNotReDelete verifies the
// DeletionTimestamp guard: re-entering Preparing while the pod is
// still in a Terminating state must NOT re-issue Delete (would emit
// a duplicate PodTerminating event and is otherwise wasteful).
func TestPreparing_PodHasDeletionTimestamp_DoesNotReDelete(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = "m"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	now := metav1.Now()
	pod := newLauncherPod("guest", "default")
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{"kubernetes.io/test-finalizer"} // keeps fake client from immediate-deleting
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pod).
		WithStatusSubresource(mig).
		Build()
	recorder := record.NewFakeRecorder(10)
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: recorder}

	status := mig.Status.DeepCopy()
	advanced, requeue, errMsg, err := r.handlePreparing(context.Background(), mig, status)
	if err != nil || errMsg != "" {
		t.Fatalf("Terminating pod should not error; err=%v errMsg=%q", err, errMsg)
	}
	if advanced {
		t.Error("must NOT advance while pod is Terminating with finalizer")
	}
	if requeue == 0 {
		t.Error("should requeue while waiting for pod termination")
	}
	// Drain the recorder; assert no PodTerminating event was emitted
	// (the DeletionTimestamp guard skipped Delete and its event).
	close(recorder.Events)
	for ev := range recorder.Events {
		if strings.Contains(ev, "PodTerminating") {
			t.Errorf("must not re-emit PodTerminating event when DeletionTimestamp is set; got %q", ev)
		}
	}
}

// TestPreparing_ReentryWithMatchingAnnotation_NoOpClaim verifies
// idempotency: a re-reconcile in Preparing with our annotation
// already present must NOT re-issue the runPolicy patch (no-op
// re-claim). It does proceed to the pod-deletion / wait phases.
func TestPreparing_ReentryWithMatchingAnnotation_NoOpClaim(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = "m"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, _, errMsg, err := r.handlePreparing(context.Background(), mig, status)
	if err != nil || errMsg != "" {
		t.Fatalf("re-entry should succeed; err=%v errMsg=%q", err, errMsg)
	}
	// Pod is gone, no PVC/VA either — should advance.
	if !advanced {
		t.Error("re-entry with no pod and no VA should advance to StopAndCopy")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy {
		t.Errorf("phase = %q, want StopAndCopy", status.Phase)
	}
}

// TestPreparing_PodGone_VAStillPresent_DoesNotAdvance verifies the
// dual-poll: Pod NotFound is necessary but not sufficient — the
// VolumeAttachment for the per-guest root PVC must also be GC'd
// before advancing. Without this gate, the destination pod would
// hit a Multi-Attach error on RWO storage.
func TestPreparing_PodGone_VAStillPresent_DoesNotAdvance(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = "m"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	pvc := newPVCBoundTo("swiftguest-root-guest", "default", "pv-1")
	va := newVolumeAttachment("va-stale", "pv-1", "boba") // still attached on source node
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pvc, va).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, requeue, errMsg, err := r.handlePreparing(context.Background(), mig, status)
	if err != nil || errMsg != "" {
		t.Fatalf("pod-gone+VA-present should not error; err=%v errMsg=%q", err, errMsg)
	}
	if advanced {
		t.Error("must NOT advance when VolumeAttachment for the PV is still present (Multi-Attach risk)")
	}
	if requeue == 0 {
		t.Error("should requeue while waiting for VA GC")
	}
	if !strings.Contains(status.PhaseDetail, "volume detach") {
		t.Errorf("phaseDetail should report volume-detach wait; got %q", status.PhaseDetail)
	}
}

// TestPreparing_PodGoneAndVAGone_Advances verifies the success path:
// once the source pod is gone AND no VolumeAttachment exists for the
// per-guest root PV, advance to StopAndCopy. Mirrors the spike's
// observed timing on Longhorn (~45s after Delete(pod)).
func TestPreparing_PodGoneAndVAGone_Advances(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = "m"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	pvc := newPVCBoundTo("swiftguest-root-guest", "default", "pv-1")
	// No VolumeAttachment objects; no Pod objects.
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pvc).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, _, errMsg, err := r.handlePreparing(context.Background(), mig, status)
	if err != nil || errMsg != "" {
		t.Fatalf("pod-gone+VA-gone should advance; err=%v errMsg=%q", err, errMsg)
	}
	if !advanced {
		t.Fatal("must advance to StopAndCopy when pod gone and no VA")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy {
		t.Errorf("phase = %q, want StopAndCopy", status.Phase)
	}
}

// TestPreparing_GuestDeletedMidFlight verifies that if the source
// SwiftGuest is deleted while Preparing is in progress, the migration
// fails with a clear "deleted during Preparing" error. Belt-and-
// suspenders against operator error.
func TestPreparing_GuestDeletedMidFlight(t *testing.T) {
	scheme := preparingScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	_, _, errMsg, _ := r.handlePreparing(context.Background(), mig, status)
	if !strings.Contains(errMsg, "deleted during Preparing") {
		t.Errorf("errMsg = %q, want mention of deleted during Preparing", errMsg)
	}
}

// TestPreparing_PVCNotYetBound_AdvancesGracefully covers the corner
// case where the per-guest root PVC isn't bound to a PV yet (e.g.,
// SwiftGuest was created moments before the SwiftMigration). With no
// VolumeName, isPVCStillAttached returns false and the controller
// advances. This is correct: there's nothing to detach if the PVC
// never had a backing PV.
func TestPreparing_PVCNotYetBound_AdvancesGracefully(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = "m"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	// PVC exists but Spec.VolumeName == "" (not yet bound).
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "swiftguest-root-guest", Namespace: "default"},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pvc).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, _, errMsg, err := r.handlePreparing(context.Background(), mig, status)
	if err != nil || errMsg != "" {
		t.Fatalf("unbound PVC should not error; err=%v errMsg=%q", err, errMsg)
	}
	if !advanced {
		t.Error("unbound PVC has nothing to detach; must advance")
	}
}

// TestPreparing_VAForOtherPVNotMatched verifies the VA matching is
// PV-specific. A VolumeAttachment referencing a different PV must NOT
// block this migration — otherwise we'd serialize migrations across
// the cluster.
func TestPreparing_VAForOtherPVNotMatched(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	if guest.Annotations == nil {
		guest.Annotations = map[string]string{}
	}
	guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] = "m"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	pvc := newPVCBoundTo("swiftguest-root-guest", "default", "pv-1")
	// VA for a totally unrelated PV.
	otherVA := newVolumeAttachment("va-other", "pv-other", "miles")
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pvc, otherVA).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	advanced, _, _, _ := r.handlePreparing(context.Background(), mig, status)
	if !advanced {
		t.Error("unrelated VolumeAttachment must not block this migration")
	}
}
