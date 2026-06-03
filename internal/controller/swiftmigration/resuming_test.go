package swiftmigration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// readyDstPod builds a destination launcher pod whose init containers have
// completed and whose launcher container is Ready (the W-GPU-3 gate's
// happy-path precondition).
func readyDstPod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "launcher"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "launcher", Ready: true}},
		},
	}
}

// guestRunning constructs a SwiftGuest with the GuestRunning=True
// condition set and an IP populated. Helper for the happy-path
// Resuming test.
func guestRunning(name, ns string, ip string) *swiftv1alpha1.SwiftGuest {
	g := newGuestForValidating(name, ns, "class-default")
	g.Status.NodeName = "miles"
	g.Status.Conditions = []metav1.Condition{
		{Type: guestRunningConditionType, Status: metav1.ConditionTrue},
	}
	g.Status.Network = &swiftv1alpha1.GuestNetworkStatus{PrimaryIP: ip}
	g.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	return g
}

// TestResuming_GuestRunningPlusIP_TransitionsToCompleted is the happy
// path: GuestRunning=True AND primaryIP populated → advance to
// Completed, clear annotation, set Ready=True, compute downtime.
func TestResuming_GuestRunningPlusIP_TransitionsToCompleted(t *testing.T) {
	scheme := preparingScheme(t)
	guest := guestRunning("guest", "default", "10.244.125.17")
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming
	startedAt := metav1.NewTime(time.Now().Add(-90 * time.Second))
	mig.Status.StartedAt = &startedAt
	mig.Status.SourceNode = "boba"
	mig.Status.DestinationNode = "miles"

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, readyDstPod("guest", "default", "miles")).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleResuming(context.Background(), mig, status)
	advanced, errMsg, err := result.Advanced, result.FailureMsg, result.Err
	if err != nil || errMsg != "" {
		t.Fatalf("Resuming happy path should not error; err=%v errMsg=%q", err, errMsg)
	}
	if !advanced {
		t.Fatal("must advance to Completed when GuestRunning=True + IP populated")
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Errorf("phase = %q, want Completed", status.Phase)
	}
	if status.CompletedAt == nil {
		t.Error("CompletedAt should be stamped")
	}
	if status.ObservedDowntime == nil {
		t.Error("ObservedDowntime should be computed")
	} else if status.ObservedDowntime.Duration < 60*time.Second {
		t.Errorf("ObservedDowntime = %s, expected ~90s; computation looks wrong", status.ObservedDowntime.Duration)
	}
	// Ready=True
	readyTrue := false
	for _, c := range status.Conditions {
		if c.Type == migrationv1alpha1.SwiftMigrationConditionReady && c.Status == metav1.ConditionTrue {
			readyTrue = true
		}
	}
	if !readyTrue {
		t.Error("Ready=True condition should be set on Completed")
	}
	// Annotation cleared.
	var got swiftv1alpha1.SwiftGuest
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got); err != nil {
		t.Fatalf("Get guest: %v", err)
	}
	if _, present := got.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]; present {
		t.Error("migration-in-progress annotation should be cleared on Completed")
	}
}

// TestResuming_GuestRunningButNoIP_StaysInResuming verifies the
// intermediate state where GuestRunning condition has flipped True
// but swiftletd's lease poller hasn't reported the IP yet.
func TestResuming_GuestRunningButNoIP_StaysInResuming(t *testing.T) {
	scheme := preparingScheme(t)
	guest := guestRunning("guest", "default", "")
	guest.Status.Network.PrimaryIP = "" // reset
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, readyDstPod("guest", "default", "miles")).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleResuming(context.Background(), mig, status)
	advanced, requeue := result.Advanced, result.Requeue
	if advanced {
		t.Error("must NOT advance when primaryIP is empty")
	}
	if requeue == 0 {
		t.Error("should requeue while waiting for IP")
	}
	if !strings.Contains(status.PhaseDetail, "primaryIP") {
		t.Errorf("phaseDetail should mention primaryIP wait; got %q", status.PhaseDetail)
	}
}

// TestResuming_GuestNotRunning_StaysInResuming verifies the typical
// boot-bound wait: the launcher pod is on the destination but
// swiftletd hasn't reported GuestRunning=True yet (CH still booting,
// cloud-init resuming).
func TestResuming_GuestNotRunning_StaysInResuming(t *testing.T) {
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "miles"
	// No GuestRunning condition; or False.
	guest.Status.Conditions = []metav1.Condition{
		{Type: guestRunningConditionType, Status: metav1.ConditionFalse},
	}
	guest.Annotations = map[string]string{
		migrationv1alpha1.AnnotationMigrationInProgress: "m",
	}
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, readyDstPod("guest", "default", "miles")).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleResuming(context.Background(), mig, status)
	advanced, requeue := result.Advanced, result.Requeue
	if advanced {
		t.Error("must NOT advance while GuestRunning=False")
	}
	if requeue == 0 {
		t.Error("should requeue while polling")
	}
	if !strings.Contains(status.PhaseDetail, "GuestRunning") {
		t.Errorf("phaseDetail should mention GuestRunning wait; got %q", status.PhaseDetail)
	}
}

// TestResuming_GuestDeletedMidFlight verifies graceful handling.
func TestResuming_GuestDeletedMidFlight(t *testing.T) {
	scheme := preparingScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleResuming(context.Background(), mig, status)
	errMsg := result.FailureMsg
	if !strings.Contains(errMsg, "deleted during Resuming") {
		t.Errorf("guest deleted should fail clearly; got %q", errMsg)
	}
}

// TestResuming_DstInitFailed_Fails is the W-GPU-3 regression: the destination
// pod's init container terminated with an error (e.g., gpu-init could not bind
// the GPU on the target), so the guest cannot boot there. Even with a STALE
// GuestRunning=True + IP carried over from the source pod, the migration must
// FAIL — not falsely report Completed.
func TestResuming_DstInitFailed_Fails(t *testing.T) {
	scheme := preparingScheme(t)
	guest := guestRunning("guest", "default", "10.244.125.17") // stale True + IP from source
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming
	mig.Status.DestinationNode = "miles"

	dstPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "miles"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "gpu-init", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, dstPod).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleResuming(context.Background(), mig, status)
	if !strings.Contains(result.FailureMsg, "failed to boot") {
		t.Errorf("dst init failure must fail the migration (no false Completed); got advanced=%v msg=%q",
			result.Advanced, result.FailureMsg)
	}
	if result.Advanced {
		t.Error("must NOT advance to Completed when the destination guest failed to boot")
	}
}

// TestResuming_DstLauncherNotReady_StaysInResuming: the destination launcher
// container is not yet Ready (still booting), so even a stale GuestRunning=True
// must not complete — requeue and wait for the real destination boot.
func TestResuming_DstLauncherNotReady_StaysInResuming(t *testing.T) {
	scheme := preparingScheme(t)
	guest := guestRunning("guest", "default", "10.244.125.17") // stale True + IP
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming
	mig.Status.DestinationNode = "miles"

	dstPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "miles", Containers: []corev1.Container{{Name: "launcher"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "launcher", Ready: false}}, // not ready yet
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig, guest, dstPod).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	result := r.handleResuming(context.Background(), mig, status)
	if result.Advanced || result.FailureMsg != "" {
		t.Errorf("launcher-not-ready must requeue, not complete/fail; got advanced=%v msg=%q", result.Advanced, result.FailureMsg)
	}
	if result.Requeue == 0 {
		t.Error("must requeue while the destination launcher is not ready")
	}
	if status.Phase == migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Error("must not be Completed on a stale GuestRunning when the dst launcher isn't ready")
	}
}
