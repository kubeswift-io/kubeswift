package swiftmigration

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// stuckOfflinePreparing is an offline migration that claimed the guest
// (marker + runPolicy=Stopped) an hour ago and never got further: the source
// pod is gone but its volume never detached.
func stuckOfflinePreparing(t *testing.T, strategy migrationv1alpha1.SwiftMigrationTimeoutStrategy) (*SwiftMigrationReconciler, client.Client) {
	t.Helper()
	scheme := preparingScheme(t)
	guest := newGuestForValidating("guest", "default", "class-default")
	guest.Status.NodeName = "worker-1"
	guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyStopped
	guest.Annotations = map[string]string{migrationv1alpha1.AnnotationMigrationInProgress: "m"}
	mig := newMigration("m", "default")
	mig.Finalizers = []string{FinalizerName}
	mig.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	mig.Spec.TimeoutStrategy = strategy
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	mig.Status.StartedAt = &started
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	mig.Status.Mode = migrationv1alpha1.SwiftMigrationModeOffline
	pvc := newPVCBoundTo(guest.Name+"-root", "default", "pv-1")
	va := newVolumeAttachment("va-1", "pv-1", "worker-1")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(mig, guest, pvc, va).
		WithStatusSubresource(mig).Build()
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}, c
}

// An offline migration had no timeout at all: one stuck in Preparing kept the
// guest's migration-in-progress marker forever, blocking every later
// migration and drain. It now fails at spec.timeout, restarting the guest
// where it was and releasing the marker.
func TestOffline_StuckMigrationTimesOutAndReleasesTheGuest(t *testing.T) {
	r, c := stuckOfflinePreparing(t, migrationv1alpha1.SwiftMigrationTimeoutStrategyCancel)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	var mig migrationv1alpha1.SwiftMigration
	if err := c.Get(ctx, client.ObjectKey{Name: "m", Namespace: "default"}, &mig); err != nil {
		t.Fatal(err)
	}
	if mig.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseFailed || mig.Status.FailureReason != migrationv1alpha1.FailureReasonTimeout {
		t.Fatalf("phase=%s reason=%s, want Failed/Timeout", mig.Status.Phase, mig.Status.FailureReason)
	}
	var g swiftv1alpha1.SwiftGuest
	if err := c.Get(ctx, client.ObjectKey{Name: "guest", Namespace: "default"}, &g); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Annotations[migrationv1alpha1.AnnotationMigrationInProgress]; ok {
		t.Error("the migration-in-progress marker must be released")
	}
	if g.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning {
		t.Errorf("runPolicy = %s; a pre-cutover failure restarts the guest where it was", g.Spec.RunPolicy)
	}
}

// timeoutStrategy: ignore turns the backstop off (it was accepted and never
// read).
func TestTimeoutStrategyIgnore_DisablesTheTimeout(t *testing.T) {
	r, c := stuckOfflinePreparing(t, migrationv1alpha1.SwiftMigrationTimeoutStrategyIgnore)
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"}}); err != nil {
		t.Fatal(err)
	}
	var mig migrationv1alpha1.SwiftMigration
	if err := c.Get(ctx, client.ObjectKey{Name: "m", Namespace: "default"}, &mig); err != nil {
		t.Fatal(err)
	}
	if mig.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Errorf("timeoutStrategy=ignore must not fail on timeout: %s", mig.Status.FailureMessage)
	}

	live := newMigration("l", "default")
	live.Spec.Timeout = &metav1.Duration{Duration: time.Minute}
	live.Spec.TimeoutStrategy = migrationv1alpha1.SwiftMigrationTimeoutStrategyIgnore
	started := metav1.NewTime(time.Now().Add(-time.Hour))
	if timeoutExceeded(live, &migrationv1alpha1.SwiftMigrationStatus{StartedAt: &started}) {
		t.Error("the live-mode check must honor ignore too")
	}
	live.Spec.TimeoutStrategy = ""
	if !timeoutExceeded(live, &migrationv1alpha1.SwiftMigrationStatus{StartedAt: &started}) {
		t.Error("the default strategy (cancel) must act on an expired timeout")
	}
}
