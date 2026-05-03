package swiftmigration

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// TestStopAndCopy_FirstEntry_PatchesAtomically verifies the architect's
// Q1 atomicity rule: spec.runPolicy=Running AND spec.nodeName=target
// land in a single MergeFrom patch. The test reads the SwiftGuest after
// the call and asserts both fields match.
func TestStopAndCopy_FirstEntry_PatchesAtomically(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped // set by Preparing
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleStopAndCopy(context.Background(), mig, status)
	advanced, requeue, errMsg, err := result.Advanced, result.Requeue, result.FailureMsg, result.Err
	if err != nil || errMsg != "" {
		t.Fatalf("StopAndCopy first-entry should not error; err=%v errMsg=%q", err, errMsg)
	}
	if advanced {
		t.Error("StopAndCopy should NOT advance until destination pod exists; should poll")
	}
	if requeue == 0 {
		t.Error("StopAndCopy should requeue while polling for pod")
	}

	// Read the SwiftGuest. Both fields must be set atomically.
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest after StopAndCopy: %v", err)
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("runPolicy = %q, want Running (atomicity)", got.Spec.RunPolicy)
	}
	if got.Spec.NodeName != "miles" {
		t.Errorf("nodeName = %q, want miles (atomicity)", got.Spec.NodeName)
	}
}

// TestStopAndCopy_MissingAnnotation_Fails verifies the defensive
// invariant check: if the in-progress annotation is missing on entry
// to StopAndCopy, the phase ordering was violated and we fail rather
// than continue (would leave the guest in an inconsistent state).
func TestStopAndCopy_MissingAnnotation_Fails(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	// No annotation — Preparing should have set it but somehow didn't.
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleStopAndCopy(context.Background(), mig, status)
	errMsg := result.FailureMsg
	if !strings.Contains(errMsg, "missing the in-progress annotation") {
		t.Errorf("missing annotation should fail with phase-ordering error; got %q", errMsg)
	}
}

// TestStopAndCopy_PodOnDestination_Advances verifies the success path:
// once the SwiftGuest controller has recreated the launcher pod with
// pod.Spec.NodeName=target, advance to Resuming and stamp the
// destination pod ref.
func TestStopAndCopy_PodOnDestination_Advances(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning // already patched
	guest.Spec.NodeName = "miles"                         // already patched
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "miles",
			Containers: []corev1.Container{
				{Name: "launcher", Image: "ghcr.io/projectbeskar/kubeswift/swiftletd:test"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, pod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleStopAndCopy(context.Background(), mig, status)
	advanced, errMsg, err := result.Advanced, result.FailureMsg, result.Err
	if err != nil || errMsg != "" {
		t.Fatalf("StopAndCopy with destination pod should advance; err=%v errMsg=%q", err, errMsg)
	}
	if !advanced {
		t.Fatal("must advance to Resuming when destination pod exists on target")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseResuming {
		t.Errorf("phase = %q, want Resuming", status.Phase)
	}
	if status.DestinationPodRef == nil || status.DestinationPodRef.Name != "guest" {
		t.Errorf("DestinationPodRef = %+v, want {Name: guest}", status.DestinationPodRef)
	}
}

// TestStopAndCopy_PodOnWrongNode_Fails verifies the atomicity-violation
// detection: if the destination pod somehow landed on a node other
// than the target, fail with a clear error rather than advance.
// In practice this should be impossible because of the combined patch
// + commit-3 pod builder pinning, but defense in depth.
func TestStopAndCopy_PodOnWrongNode_Fails(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	guest.Spec.NodeName = "miles"
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	wrongPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName: "boba", // landed on the wrong node!
			Containers: []corev1.Container{
				{Name: "launcher", Image: "ghcr.io/projectbeskar/kubeswift/swiftletd:test"},
			},
		},
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, wrongPod).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleStopAndCopy(context.Background(), mig, status)
	errMsg := result.FailureMsg
	if !strings.Contains(errMsg, "atomicity invariant violated") {
		t.Errorf("wrong-node pod should fail with atomicity error; got %q", errMsg)
	}
}

// TestStopAndCopy_GuestDeleted_Fails verifies graceful handling when
// the source guest is deleted mid-flight.
func TestStopAndCopy_GuestDeleted_Fails(t *testing.T) {
	scheme := preparingScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleStopAndCopy(context.Background(), mig, status)
	errMsg := result.FailureMsg
	if !strings.Contains(errMsg, "deleted during StopAndCopy") {
		t.Errorf("guest deleted mid-flight should fail clearly; got %q", errMsg)
	}
}

// TestStopAndCopy_Idempotent_PatchAlreadyApplied verifies re-entry
// when the spec is already patched (re-reconcile after the patch
// landed but before the pod was observed). The MergeFrom against
// current state is a no-op and the controller proceeds to the pod
// poll. Critical for architect Risk 2 (drive-forward post-cutover):
// crash-and-resume must not corrupt the patched spec.
func TestStopAndCopy_Idempotent_PatchAlreadyApplied(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "boba"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning // already patched
	guest.Spec.NodeName = "miles"                         // already patched
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleStopAndCopy(context.Background(), mig, status)
	requeue, errMsg, err := result.Requeue, result.FailureMsg, result.Err
	if err != nil || errMsg != "" {
		t.Fatalf("idempotent re-entry should not error; err=%v errMsg=%q", err, errMsg)
	}
	if requeue == 0 {
		t.Error("re-entry without destination pod should requeue")
	}

	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	if got.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning || got.Spec.NodeName != "miles" {
		t.Errorf("idempotent re-entry corrupted spec: runPolicy=%q nodeName=%q", got.Spec.RunPolicy, got.Spec.NodeName)
	}
}
