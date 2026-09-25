package swiftmigration

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/controller/swiftguest"
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
	// Source is mid-send, NOT complete; the destination is still receiving
	// (a destination that reported running would be committed).
	stamp(src, migrationActionVerbSend, sendActionID(mig), "sending", sendActionID(mig), "")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
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

// Deleting a live migration past its commit point must still finish the
// cutover before the object goes: until step 1 the guest's podRef names the
// source pod, whose launcher has exited. Dropping the finalizer there left the
// migrated VM running in a pod nothing tracked.
func TestCommitPoint_DeletePostCommitFinishesCutover(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	guest.Status.PodRef = &corev1.ObjectReference{Name: src.Name, Namespace: "default", UID: src.UID}
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "sent")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	ctx := context.Background()
	if err := r.Delete(ctx, mig); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 6; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var g swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKeyFromObject(guest), &g); err != nil {
		t.Fatal(err)
	}
	if g.Status.PodRef == nil || g.Status.PodRef.Name != dst.Name {
		t.Fatalf("guest podRef = %+v, want the destination pod %q", g.Status.PodRef, dst.Name)
	}
	var p corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(src), &p); !apierrors.IsNotFound(err) {
		t.Errorf("source pod not removed by cutover (err=%v)", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(dst), &p); err != nil {
		t.Errorf("destination pod lost: %v", err)
	}
}

// The SwiftGuest controller recognises a launcher that handed its VM off by
// this annotation; the two packages must name the same key.
func TestCommitPoint_GuestControllerReadsTheSameStatusKey(t *testing.T) {
	if swiftguest.PodAnnotationMigrationStatus != AnnotationMigrationStatus {
		t.Fatalf("swiftguest reads %q, swiftletd writes %q", swiftguest.PodAnnotationMigrationStatus, AnnotationMigrationStatus)
	}
}

// The destination is the second witness of the commit point. In the lab's
// validation of v0.15.0 (D9) the source launcher exited after a completed send
// without writing complete: its pod Succeeded with migration-status still
// "sending", while the destination reported running with the guest live.
// spec.timeout then failed the migration as pre-cutover and deleted the
// destination, leaving the guest with no VM. The destination's report alone
// must commit the migration and drive it through cutover to Completed.
func TestCommitPoint_TimeoutWithOnlyTheDestinationsReportCutsOver(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	started := metav1.NewTime(time.Now().Add(-31 * time.Minute))
	mig.Status.StartedAt = &started
	mig.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Minute}
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveIssuingSend
	guest.Status.PodRef = &corev1.ObjectReference{Name: src.Name, Namespace: "default", UID: src.UID}
	// swiftletd-on-dst reported the guest running (W16), as it did in the lab.
	guest.Status.Conditions = []metav1.Condition{{
		Type: guestRunningConditionType, Status: metav1.ConditionTrue,
		Reason: "VmRunning", LastTransitionTime: metav1.Now(),
	}}
	stamp(src, migrationActionVerbSend, sendActionID(mig), "sending", sendActionID(mig), "")
	src.Status.Phase = corev1.PodSucceeded
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var got migrationv1alpha1.SwiftMigration
	_ = r.Get(ctx, client.ObjectKeyFromObject(mig), &got)
	if got.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseCompleted {
		t.Errorf("phase = %s (%s: %s), want Completed: the guest runs on the destination",
			got.Status.Phase, got.Status.FailureReason, got.Status.FailureMessage)
	}
	var p corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(dst), &p); apierrors.IsNotFound(err) {
		t.Fatal("destination pod, running the only copy of the guest, was deleted")
	}
	var g swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKeyFromObject(guest), &g); err != nil {
		t.Fatal(err)
	}
	if g.Status.PodRef == nil || g.Status.PodRef.Name != dst.Name {
		t.Errorf("guest podRef = %+v, want the destination pod %q", g.Status.PodRef, dst.Name)
	}
}

// A source that handed the guest over may be gone altogether. The pre-cutover
// source-pod check must not fail the migration (and delete the destination)
// once the destination runs the guest.
func TestCommitPoint_SourcePodGoneAfterTheDestinationRunsCutsOver(t *testing.T) {
	mig, guest, _, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveIssuingSend
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	r := newStopAndCopyReconciler(t, mig, guest, dst) // no source pod

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.FailureReason != "" {
		t.Errorf("failed with %s (%s); the destination runs the guest", res.FailureReason, res.FailureMsg)
	}
}

// The source reporting failure while the destination reports running can only
// mean the hand-over finished after the source gave up on it (its deadline):
// the destination runs the guest, so failing would delete the only copy.
func TestCommitPoint_SourceFailedButDestinationRunningCutsOver(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	stamp(src, migrationActionVerbSend, sendActionID(mig), MigrationStatusFailed, sendActionID(mig), "past the migration deadline")
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	if got := deriveSubstate(mig, src, dst); got != substateDstRunning {
		t.Fatalf("substate = %v, want dst-running", got)
	}
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	status := mig.Status.DeepCopy()
	if res := r.handleStopAndCopyLive(context.Background(), mig, status); res.FailureReason != "" {
		t.Errorf("failed with %s (%s); the destination runs the guest", res.FailureReason, res.FailureMsg)
	}
}

// Cancel and deletion read the commit point through liveCommitted, which must
// see the destination's report too.
func TestCommitPoint_CancelAndDeleteRespectTheDestinationsReport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delete bool
	}{{"cancel", false}, {"delete", true}} {
		t.Run(tc.name, func(t *testing.T) {
			mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
			mig.Finalizers = []string{FinalizerName}
			mig.Spec.CancelRequested = !tc.delete
			mig.Status.RecvAttempts = 1
			mig.Status.SendAttempts = 1
			mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
			stamp(src, migrationActionVerbSend, sendActionID(mig), "sending", sendActionID(mig), "")
			stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
			dst.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Minute))
			r := newStopAndCopyReconciler(t, mig, guest, src, dst)
			ctx := context.Background()

			committed, err := r.liveCommitted(ctx, mig)
			if err != nil || !committed {
				t.Fatalf("liveCommitted = %v, %v; want committed on the destination's report", committed, err)
			}
			if tc.delete {
				if err := r.Delete(ctx, mig); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 3; i++ {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mig)}); err != nil {
					t.Fatalf("reconcile %d: %v", i, err)
				}
			}
			var p corev1.Pod
			if err := r.Get(ctx, client.ObjectKeyFromObject(dst), &p); apierrors.IsNotFound(err) {
				t.Fatal("destination pod, running the only copy of the guest, was deleted")
			}
		})
	}
}

// The destination commits the migration, but the source normally reports
// complete a few seconds later, and its report carries the pause window. Lab
// validation of v0.15.0 (round 2) saw every live migration cut over on the
// destination's report 3-4 s before the source's, losing
// observedTransferDuration and raising SourceCompleteMissing each time. A live
// source launcher is waited for, briefly.
func TestCommitPoint_DestinationReportWaitsBrieflyForALiveSource(t *testing.T) {
	setup := func(t *testing.T, sinceDstRunning time.Duration) (*SwiftMigrationReconciler, *migrationv1alpha1.SwiftMigration, *migrationv1alpha1.SwiftMigrationStatus, *corev1.Pod) {
		mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
		mig.Finalizers = []string{FinalizerName}
		mig.Status.RecvAttempts = 1
		mig.Status.SendAttempts = 1
		mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
		guest.Status.PodRef = &corev1.ObjectReference{Name: src.Name, Namespace: "default", UID: src.UID}
		stamp(src, migrationActionVerbSend, sendActionID(mig), "sending", sendActionID(mig), "")
		stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
		if sinceDstRunning > 0 {
			mig.Status.Conditions = append(mig.Status.Conditions, metav1.Condition{
				Type: migrationv1alpha1.SwiftMigrationConditionDestinationRunning, Status: metav1.ConditionTrue,
				Reason: "DestinationRunning", LastTransitionTime: metav1.NewTime(time.Now().Add(-sinceDstRunning)),
			})
		}
		return newStopAndCopyReconciler(t, mig, guest, src, dst), mig, mig.Status.DeepCopy(), dst
	}
	podRefOf := func(t *testing.T, r *SwiftMigrationReconciler) string {
		var g swiftv1alpha1.SwiftGuest
		if err := r.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &g); err != nil {
			t.Fatal(err)
		}
		if g.Status.PodRef == nil {
			return ""
		}
		return g.Status.PodRef.Name
	}

	t.Run("waits within the grace", func(t *testing.T) {
		r, mig, status, dst := setup(t, 0)
		res := r.handleStopAndCopyLive(context.Background(), mig, status)
		if res.FailureReason != "" {
			t.Fatalf("failed: %s (%s)", res.FailureReason, res.FailureMsg)
		}
		if got := podRefOf(t, r); got == dst.Name {
			t.Error("cut over at once; the live source had no chance to report")
		}
		if apimeta.FindStatusCondition(status.Conditions, migrationv1alpha1.SwiftMigrationConditionDestinationRunning) == nil {
			t.Error("DestinationRunning not recorded; the grace has nothing to run from")
		}
		// The transfer is over: the wait must not read "transferring guest state".
		if status.PhaseDetail != migrationv1alpha1.PhaseDetailLiveDestRunning {
			t.Errorf("phaseDetail = %q, want %q", status.PhaseDetail, migrationv1alpha1.PhaseDetailLiveDestRunning)
		}
	})
	t.Run("cuts over once the grace has run out", func(t *testing.T) {
		r, mig, status, dst := setup(t, sourceReportGrace+time.Second)
		if res := r.handleStopAndCopyLive(context.Background(), mig, status); res.FailureReason != "" {
			t.Fatalf("failed: %s (%s)", res.FailureReason, res.FailureMsg)
		}
		if got := podRefOf(t, r); got != dst.Name {
			t.Errorf("guest podRef = %q, want the destination %q after the grace", got, dst.Name)
		}
	})
}

// A source that reports within the grace takes the normal path, and its pause
// window is recorded.
func TestCommitPoint_SourceReportingWithinTheGraceKeepsThePauseWindow(t *testing.T) {
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	guest.Status.PodRef = &corev1.ObjectReference{Name: src.Name, Namespace: "default", UID: src.UID}
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "sent")
	src.Annotations[AnnotationMigrationPauseWindowMs] = "1234"
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusRunning, recvActionID(mig), "received")
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)

	status := mig.Status.DeepCopy()
	r.handleStopAndCopyLive(context.Background(), mig, status)
	if status.ObservedTransferDuration == nil || status.ObservedTransferDuration.Duration != 1234*time.Millisecond {
		t.Errorf("observedTransferDuration = %v, want 1.234s from the source's report", status.ObservedTransferDuration)
	}
}
