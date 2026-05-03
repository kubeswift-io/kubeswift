package swiftmigration

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Live-mode dispatch tests. B2 replaced the Validating-live stub with
// a real body; the test now verifies dispatch routes to the live body
// (which fails fast on a missing source pod, since live-migrating a
// non-running guest is invalid).

func TestDispatch_Validating_LiveMode_RoutesToLiveBody(t *testing.T) {
	scheme := testScheme(t)
	mig := newMigration("m", "default")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default"},
		Status:     swiftv1alpha1.SwiftGuestStatus{NodeName: "miles"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()

	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}
	// First reconcile: Pending → Validating (no handler dispatch).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"}}); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	// Second reconcile: Validating-live runs → fails on missing src pod.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "m", Namespace: "default"}}); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &got); err != nil {
		t.Fatalf("re-get: %v", err)
	}

	if got.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Errorf("phase: want Failed (live body, no src pod), got %q", got.Status.Phase)
	}
	if !strings.Contains(got.Status.FailureMessage, "has no pod") {
		t.Errorf("FailureMessage: want missing-pod message, got %q", got.Status.FailureMessage)
	}
	if got.Status.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", got.Status.FailureReason)
	}
	if got.Status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		t.Errorf("status.Mode: want live (set before failure), got %q", got.Status.Mode)
	}
}

func TestDispatch_Validating_OfflineMode_DoesNotRouteToLiveStub(t *testing.T) {
	// Offline-mode SwiftMigration must NOT enter the live stub. The
	// existing Phase 1 offline tests cover the full offline flow; this
	// test only verifies the dispatch gate behaves correctly. Direct
	// isLiveMode check avoids needing to set up the full
	// SwiftGuestClass scheme + capacity check fixtures.
	mig := &migrationv1alpha1.SwiftMigration{
		Spec: migrationv1alpha1.SwiftMigrationSpec{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	// Direct call to handleValidatingLive must NOT happen here; this
	// test asserts that isLiveMode returns false for offline, which
	// means the dispatch gate in handleValidating won't take the
	// live branch.
	if isLiveMode(mig) {
		t.Errorf("offline-mode mig must not dispatch to live; isLiveMode=true")
	}
}

// Per-handler defensive-guard tests. Each *_live handler asserts
// isLiveMode at entry and returns FailureReasonOther if invoked
// without live mode. These guards are belt-and-suspenders against
// future code changes that bypass the dispatch wiring.

func TestHandleValidatingLive_GuardFires_WhenNotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Spec: migrationv1alpha1.SwiftMigrationSpec{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	res := r.handleValidatingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(res.FailureMsg, "internal: handleValidatingLive invoked without live mode") {
		t.Errorf("guard message: got %q", res.FailureMsg)
	}
	if res.FailureReason != migrationv1alpha1.FailureReasonOther {
		t.Errorf("FailureReason: want Other, got %q", res.FailureReason)
	}
}

func TestHandlePreparingLive_GuardFires_WhenNotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	res := r.handlePreparingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(res.FailureMsg, "internal: handlePreparingLive invoked without live mode") {
		t.Errorf("guard message: got %q", res.FailureMsg)
	}
}

func TestHandleStopAndCopyLive_GuardFires_WhenNotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	res := r.handleStopAndCopyLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(res.FailureMsg, "internal: handleStopAndCopyLive invoked without live mode") {
		t.Errorf("guard message: got %q", res.FailureMsg)
	}
}

func TestHandleResumingLive_GuardFires_WhenNotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	res := r.handleResumingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(res.FailureMsg, "internal: handleResumingLive invoked without live mode") {
		t.Errorf("guard message: got %q", res.FailureMsg)
	}
}

// (Validating-live stub-not-implemented test removed in B2; body is
// real now. See TestDispatch_Validating_LiveMode_RoutesToLiveBody and
// the validating_live_test.go suite for the behavior tests.)
