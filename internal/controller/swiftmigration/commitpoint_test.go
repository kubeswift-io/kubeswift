package swiftmigration

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
)

// Once the source has reported migration-status=complete, its Cloud Hypervisor
// has exited and the destination holds the only running copy. A spec.timeout
// expiry at that instant must NOT fail the migration and delete the
// destination pod — the guest would be lost. The migration drives forward to
// cutover instead.
func TestCommitPoint_TimeoutAtSrcCompleteDoesNotDeleteDestination(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	started := metav1.NewTime(time.Now().Add(-31 * time.Minute))
	mig.Status.StartedAt = &started
	mig.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "sent")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got migrationv1alpha1.SwiftMigration
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(mig), &got)
	var p corev1.Pod
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(dst), &p); apierrors.IsNotFound(err) {
		t.Fatal("destination pod (the only running copy after src=complete) was deleted by timeout")
	}
	if got.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Errorf("migration failed on timeout after the commit point; want forward progress, got reason %q", got.Status.FailureReason)
	}
}

// A timeout that expires between cutover step 1 (PodRefSwapped) and step 2
// (source-pod delete) must not fail the migration — the guest is already on the
// destination; the remaining cutover step runs.
func TestCommitPoint_TimeoutAfterStep1CompletesCutover(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	started := metav1.NewTime(time.Now().Add(-31 * time.Minute))
	mig.Status.StartedAt = &started
	mig.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveCutoverDeleteSrc
	mig.Status.Conditions = []metav1.Condition{{
		Type: migrationv1alpha1.SwiftMigrationConditionPodRefSwapped, Status: metav1.ConditionTrue,
		Reason: "x", LastTransitionTime: now,
	}}
	guest.Status.PodRef = &corev1.ObjectReference{Name: dst.Name, Namespace: "default", UID: "dst-uid"}
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "sent")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got migrationv1alpha1.SwiftMigration
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(mig), &got)
	if got.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Errorf("post-step-1 timeout failed the migration; cutover step 2 must still run. reason=%q", got.Status.FailureReason)
	}
}

// A spec.cancelRequested observed after the source reported complete must be
// ignored, not honored — honoring it would cancel/delete the destination,
// which is the only running copy.
func TestCommitPoint_CancelAfterSrcCompleteIsIgnored(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Spec.CancelRequested = true
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "sent")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	dst.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var got migrationv1alpha1.SwiftMigration
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(mig), &got)
	if got.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Errorf("cancel honored after src=complete; the destination (only running copy) would be torn down")
	}
	var p corev1.Pod
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(dst), &p); apierrors.IsNotFound(err) {
		t.Fatal("destination pod deleted by a post-commit cancel")
	}
}

// Before the commit point (source still running, not complete), a cancel is
// still honored — the pre-commit abort path must keep working.
func TestCommitPoint_CancelBeforeSrcCompleteStillCancels(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Spec.CancelRequested = true
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	// Source is mid-send, NOT complete.
	stamp(src, migrationActionVerbSend, sendActionID(mig), "sending", sendActionID(mig), "")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	dst.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	// One reconcile is enough to enter the cancel transition (write cancel to dst).
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got migrationv1alpha1.SwiftMigration
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(mig), &got)
	if got.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Error("pre-commit cancel was ignored; the source is still running and the cancel must abort the migration")
	}
}

// Deleting the SwiftMigration object mid-transfer, before the source has
// reported complete, is an abort: the destination pod must be deleted so it
// cannot complete the receive into an orphan (which, with runPolicy=Always,
// would boot a second copy from the same disk — split-brain).
func TestCommitPoint_DeletePreCommitDeletesDestination(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	// Source is mid-send, NOT complete → pre-commit.
	stamp(src, migrationActionVerbSend, sendActionID(mig), "sending", sendActionID(mig), "")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	ctx := context.Background()
	if err := r.Delete(ctx, mig); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var p corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(dst), &p); !apierrors.IsNotFound(err) {
		t.Errorf("pre-commit deletion left the destination pod alive; it will complete into an orphan (err=%v)", err)
	}
}

// Deleting the SwiftMigration object after the source has reported complete
// must NOT delete the destination pod — it holds the only running copy.
func TestCommitPoint_DeletePostCommitPreservesDestination(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "sent")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	ctx := context.Background()
	if err := r.Delete(ctx, mig); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var p corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(dst), &p); apierrors.IsNotFound(err) {
		t.Error("post-commit deletion deleted the destination pod — the only running copy of the guest")
	}
}
