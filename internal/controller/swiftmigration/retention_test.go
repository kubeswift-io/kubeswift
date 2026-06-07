package swiftmigration

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
)

func terminalMig(name string, ttl *metav1.Duration, terminalAgo *time.Duration) *migrationv1alpha1.SwiftMigration {
	m := newMigration(name, "default")
	m.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted
	if ttl != nil {
		m.Spec.TTL = ttl
	}
	if terminalAgo != nil {
		ta := metav1.NewTime(time.Now().Add(-*terminalAgo))
		m.Status.TerminalAt = &ta
	}
	return m
}

func retReconciler(t *testing.T, mig *migrationv1alpha1.SwiftMigration) (*SwiftMigrationReconciler, client.Client) {
	t.Helper()
	scheme := preparingScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig).WithStatusSubresource(mig).Build()
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}, c
}

func exists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var m migrationv1alpha1.SwiftMigration
	err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &m)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

func TestHandleTerminalRetention_NoTTL_NoOp(t *testing.T) {
	ago := 2 * time.Hour
	mig := terminalMig("m", nil, &ago) // TerminalAt set but no ttl
	r, c := retReconciler(t, mig)
	res, err := r.handleTerminalRetention(context.Background(), mig)
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("no ttl should be a no-op; res=%v err=%v", res, err)
	}
	if !exists(t, c, "m") {
		t.Error("no-ttl migration must not be deleted")
	}
}

func TestHandleTerminalRetention_NoAnchor_NoOp(t *testing.T) {
	mig := terminalMig("m", dur(time.Hour), nil) // ttl set but TerminalAt nil
	r, c := retReconciler(t, mig)
	res, err := r.handleTerminalRetention(context.Background(), mig)
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("missing TerminalAt anchor should be a no-op; res=%v err=%v", res, err)
	}
	if !exists(t, c, "m") {
		t.Error("migration without a terminal anchor must not be deleted")
	}
}

func TestHandleTerminalRetention_NotExpired_Requeues(t *testing.T) {
	ago := time.Minute
	mig := terminalMig("m", dur(time.Hour), &ago) // terminal 1m ago, ttl 1h -> ~59m left
	r, c := retReconciler(t, mig)
	res, err := r.handleTerminalRetention(context.Background(), mig)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > migrationRetentionMaxRequeue {
		t.Errorf("requeue = %v, want in (0, %v]", res.RequeueAfter, migrationRetentionMaxRequeue)
	}
	if !exists(t, c, "m") {
		t.Error("not-yet-expired migration must not be deleted")
	}
}

func TestHandleTerminalRetention_Expired_Deletes(t *testing.T) {
	ago := 2 * time.Hour
	mig := terminalMig("m", dur(time.Hour), &ago) // terminal 2h ago, ttl 1h -> expired
	r, c := retReconciler(t, mig)
	res, err := r.handleTerminalRetention(context.Background(), mig)
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("expired migration should delete (no requeue); res=%v err=%v", res, err)
	}
	if exists(t, c, "m") {
		t.Error("expired migration must be deleted")
	}
}

// TestPersist_StampsTerminalAt: the non-terminal -> terminal persist stamps the
// TTL anchor.
func TestPersist_StampsTerminalAt(t *testing.T) {
	scheme := preparingScheme(t)
	mig := newMigration("m", "default")
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseResuming
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mig).WithStatusSubresource(mig).Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	status.Phase = migrationv1alpha1.SwiftMigrationPhaseCompleted
	if err := r.persist(context.Background(), mig, status); err != nil {
		t.Fatal(err)
	}
	var got migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: "m", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.TerminalAt == nil {
		t.Error("terminal transition should stamp status.terminalAt")
	}
}
