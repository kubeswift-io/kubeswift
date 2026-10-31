package swiftmigration

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// The abandonment paths of a live migration (#893), driven through
// Reconcile. Every scenario ends by asserting which launcher is the guest:
// the source when the abort won, the destination when the transfer did --
// never neither, never both.

type abortRig struct {
	t   *testing.T
	r   *SwiftMigrationReconciler
	key client.ObjectKey
	mig *migrationv1alpha1.SwiftMigration
	rec *record.FakeRecorder
}

const (
	rigSrc = "guest"
	rigDst = "guest-mig-abcdef"
)

// newAbortRig: a live migration mid-StopAndCopy. capable: the source
// launcher can cancel on the source (Cloud Hypervisor v54); otherwise it is
// an older launcher (v53). sending: the source reports this migration's send
// in progress; otherwise no send has reached it.
func newAbortRig(t *testing.T, capable, sending bool) *abortRig {
	t.Helper()
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Finalizers = []string{FinalizerName}
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveTransferring
	stamp(dst, migrationActionVerbReceive, recvActionID(mig), migrationStatusReceiveReady, recvActionID(mig), "")
	if sending {
		stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusSending, sendActionID(mig), "")
	}
	if capable {
		src.Annotations[AnnotationLauncherFeatures] = capableFeatures
	}
	r := newStopAndCopyReconciler(t, mig, guest, src, dst)
	rec := record.NewFakeRecorder(500)
	r.Recorder = rec
	return &abortRig{t: t, r: r, key: client.ObjectKeyFromObject(mig), mig: mig, rec: rec}
}

// expire makes spec.timeout (60s) long past.
func (g *abortRig) expire() {
	g.t.Helper()
	m := g.migration()
	patch := client.MergeFrom(m.DeepCopy())
	m.Spec.Timeout = &metav1.Duration{Duration: 60 * time.Second}
	if err := g.r.Patch(context.Background(), m, patch); err != nil {
		g.t.Fatal(err)
	}
	m = g.migration()
	spatch := client.MergeFrom(m.DeepCopy())
	started := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	m.Status.StartedAt = &started
	if err := g.r.Status().Patch(context.Background(), m, spatch); err != nil {
		g.t.Fatal(err)
	}
}

func (g *abortRig) delete() {
	g.t.Helper()
	if err := g.r.Delete(context.Background(), g.migration()); err != nil {
		g.t.Fatal(err)
	}
}

func (g *abortRig) reconcile() ctrl.Result {
	g.t.Helper()
	res, err := g.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: g.key})
	if err != nil {
		g.t.Fatalf("reconcile: %v", err)
	}
	return res
}

// restart replaces the reconciler with a fresh one on the same cluster
// state: nothing carried in memory.
func (g *abortRig) restart() {
	g.r = &SwiftMigrationReconciler{Client: g.r.Client, Scheme: g.r.Scheme, Recorder: g.rec}
}

func (g *abortRig) migration() *migrationv1alpha1.SwiftMigration {
	g.t.Helper()
	var m migrationv1alpha1.SwiftMigration
	if err := g.r.Get(context.Background(), g.key, &m); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		g.t.Fatal(err)
	}
	return &m
}

func (g *abortRig) pod(name string) *corev1.Pod {
	g.t.Helper()
	var p corev1.Pod
	if err := g.r.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		g.t.Fatal(err)
	}
	return &p
}

func (g *abortRig) mustPod(name string) *corev1.Pod {
	g.t.Helper()
	p := g.pod(name)
	if p == nil {
		g.t.Fatalf("%s is gone: deleted before the transfer's outcome was known", name)
	}
	return p
}

func (g *abortRig) annotate(name string, ann map[string]string) {
	g.t.Helper()
	p := g.pod(name)
	if p == nil {
		g.t.Fatalf("%s is gone: deleted before the transfer's outcome was known", name)
	}
	patch := client.MergeFrom(p.DeepCopy())
	for k, v := range ann {
		if v == "" {
			delete(p.Annotations, k)
		} else {
			p.Annotations[k] = v
		}
	}
	if err := g.r.Patch(context.Background(), p, patch); err != nil {
		g.t.Fatal(err)
	}
}

// sourceReports sets the source's status for this migration's send.
func (g *abortRig) sourceReports(status, detail string) {
	g.annotate(rigSrc, map[string]string{
		AnnotationMigrationStatus:    status,
		AnnotationMigrationStatusID:  sendActionID(g.mig),
		AnnotationMigrationStatusDtl: detail,
	})
}

func (g *abortRig) phase() migrationv1alpha1.SwiftMigrationPhase {
	if m := g.migration(); m != nil {
		return m.Status.Phase
	}
	return "<deleted>"
}

// sourceIsTheGuest: the abort won. The source launcher is there; the
// destination is gone.
func (g *abortRig) sourceIsTheGuest() {
	g.t.Helper()
	if g.pod(rigSrc) == nil {
		g.t.Fatal("no source launcher: no guest")
	}
	if g.pod(rigDst) != nil {
		g.t.Fatal("destination launcher still there after the abort won")
	}
}

// destinationKept: nothing destroyed yet, or the transfer won.
func (g *abortRig) destinationKept(when string) {
	g.t.Helper()
	if g.pod(rigDst) == nil {
		g.t.Fatalf("%s: destination deleted before the transfer's outcome was known", when)
	}
}

func (g *abortRig) noCancelActionLeft() {
	g.t.Helper()
	if p := g.pod(rigSrc); p != nil && strings.Contains(p.Annotations[AnnotationMigrationActionID], g.mig.Name+":") {
		g.t.Errorf("this migration's action left on the source: %q", p.Annotations[AnnotationMigrationActionID])
	}
}

func (g *abortRig) sawEvent(reason string) bool {
	for {
		select {
		case e := <-g.rec.Events:
			if strings.Contains(e, reason) {
				return true
			}
		default:
			return false
		}
	}
}

const cancelledDetailText = "cancelled: Cloud Hypervisor stopped the migration on request; the source guest is running"

// 1. Timeout before the send began: nothing to resolve, fail and clean up.
func TestAbort_TimeoutBeforeSendFailsAndCleansUp(t *testing.T) {
	g := newAbortRig(t, true, false)
	g.expire()
	g.reconcile()
	if m := g.migration(); m.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseFailed ||
		m.Status.FailureReason != migrationv1alpha1.FailureReasonTimeout {
		t.Fatalf("phase=%q reason=%q", m.Status.Phase, m.Status.FailureReason)
	}
	if p := g.pod(rigSrc); p.Annotations[AnnotationMigrationAction] == MigrationActionCancelSend {
		t.Error("cancel-send written although nothing was sent")
	}
	g.sourceIsTheGuest()
}

// 2. Timeout while the source sends (v54): the timeout asks the source to
// cancel; the destination stays until the source reports; then Failed
// (Timeout), destination gone, source the guest.
func TestAbort_TimeoutWhileSendingCancelsOnTheSourceFirst(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.expire()
	before := countAbort(abortDeadline, cancelPathSource, "cancelled")
	g.reconcile()
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy {
		t.Fatalf("failed before the transfer stopped: %q", g.phase())
	}
	if !sourceCancelIssued(g.mig, g.mustPod(rigSrc)) {
		t.Fatalf("the timeout did not cancel on the source: %v", g.mustPod(rigSrc).Annotations)
	}
	g.destinationKept("timeout, cancel pending")
	g.reconcile() // still pending: nothing changes
	g.destinationKept("timeout, cancel pending, second pass")

	g.sourceReports(MigrationStatusFailed, cancelledDetailText)
	g.reconcile()
	m := g.migration()
	if m.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseFailed || m.Status.FailureReason != migrationv1alpha1.FailureReasonTimeout {
		t.Fatalf("phase=%q reason=%q", m.Status.Phase, m.Status.FailureReason)
	}
	g.sourceIsTheGuest()
	g.noCancelActionLeft() // 13: a retry is not blocked by a leftover action
	if d := countAbort(abortDeadline, cancelPathSource, "cancelled") - before; d != 1 {
		t.Errorf("deadline/source/cancelled +%v, want +1", d)
	}
}

// 3. Timeout racing completion: the cancel was asked, the transfer finished
// first. The migration completes; the destination is the guest; nothing
// reports a cancellation.
func TestAbort_TimeoutRacingCompletionCompletes(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.expire()
	g.reconcile() // cancel-send issued
	g.sourceReports(migrationStatusComplete, "sent")
	g.annotate(rigDst, map[string]string{
		AnnotationMigrationStatus:   migrationStatusRunning,
		AnnotationMigrationStatusID: recvActionID(g.mig),
	})
	for i := 0; i < 6 && g.phase() == migrationv1alpha1.SwiftMigrationPhaseStopAndCopy; i++ {
		g.reconcile()
	}
	if p := g.phase(); p == migrationv1alpha1.SwiftMigrationPhaseFailed || p == migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Fatalf("a completed transfer was turned into %q", p)
	}
	g.destinationKept("the transfer won")
	var guest swiftv1alpha1.SwiftGuest
	if err := g.r.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &guest); err != nil {
		t.Fatal(err)
	}
	if guest.Status.PodRef == nil || guest.Status.PodRef.Name != rigDst {
		t.Errorf("cutover did not move the guest to the destination: %+v", guest.Status.PodRef)
	}
}

// 4. Deletion before the send began.
func TestAbort_DeletionBeforeSend(t *testing.T) {
	g := newAbortRig(t, true, false)
	g.delete()
	g.reconcile()
	if g.migration() != nil {
		t.Fatal("deletion held although nothing was sent")
	}
	g.sourceIsTheGuest()
}

// 5. Deletion while the source sends (v54): cancel on the source first, the
// finalizer holds the SwiftMigration; then the destination goes and the
// deletion completes.
func TestAbort_DeletionWhileSendingCancelsOnTheSourceFirst(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.delete()
	before := countAbort(abortDeletion, cancelPathSource, "cancelled")
	g.reconcile()
	if g.migration() == nil {
		t.Fatal("deleted before the transfer stopped")
	}
	if !sourceCancelIssued(g.mig, g.mustPod(rigSrc)) {
		t.Fatal("deletion did not cancel on the source")
	}
	g.destinationKept("deletion, cancel pending")
	g.sourceReports(MigrationStatusFailed, cancelledDetailText)
	g.reconcile()
	if g.migration() != nil {
		t.Fatal("deletion did not complete after the transfer stopped")
	}
	g.sourceIsTheGuest()
	g.noCancelActionLeft()
	if d := countAbort(abortDeletion, cancelPathSource, "cancelled") - before; d != 1 {
		t.Errorf("deletion/source/cancelled +%v, want +1", d)
	}
}

// 6. Deletion racing completion: the transfer won; the destination is kept
// and the cutover is driven forward before the finalizer goes.
func TestAbort_DeletionRacingCompletionKeepsTheDestination(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.delete()
	g.reconcile() // cancel-send issued
	g.sourceReports(migrationStatusComplete, "sent")
	g.annotate(rigDst, map[string]string{
		AnnotationMigrationStatus:   migrationStatusRunning,
		AnnotationMigrationStatusID: recvActionID(g.mig),
	})
	for i := 0; i < 8 && g.migration() != nil; i++ {
		g.reconcile()
	}
	g.destinationKept("deletion lost to completion")
	var guest swiftv1alpha1.SwiftGuest
	if err := g.r.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &guest); err != nil {
		t.Fatal(err)
	}
	if guest.Status.PodRef == nil || guest.Status.PodRef.Name != rigDst {
		t.Errorf("the guest does not point at the destination that holds it: %+v", guest.Status.PodRef)
	}
}

// 8. The cancel could not be delivered (deadline): the destination cancel
// takes over, and the failure lands only after it acknowledges.
func TestAbort_TimeoutCancelUndeliverableUsesTheDestinationCancel(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.expire()
	g.reconcile() // cancel-send
	g.annotate(rigSrc, map[string]string{
		AnnotationMigrationCancelResult:       cancelResultFailed,
		AnnotationMigrationCancelResultID:     cancelSendID(g.mig),
		AnnotationMigrationCancelResultDetail: "cancel_migration: ch_status_error",
	})
	fb := countAbort(abortDeadline, cancelPathSource, cancelOutcomeFallback)
	g.reconcile()
	if d := countAbort(abortDeadline, cancelPathSource, cancelOutcomeFallback) - fb; d != 1 {
		t.Errorf("fallback +%v, want +1", d)
	}
	if !destinationCancelIssued(g.mig, g.mustPod(rigDst)) {
		t.Fatal("destination cancel not issued")
	}
	g.reconcile()
	g.destinationKept("waiting for the destination's acknowledgement")
	if g.phase() == migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatal("failed before the destination acknowledged")
	}
	g.annotate(rigDst, map[string]string{
		AnnotationMigrationStatus:    MigrationStatusFailed,
		AnnotationMigrationStatusID:  cancelID(g.mig),
		AnnotationMigrationStatusDtl: MigrationStatusFailedCancelDt,
	})
	g.reconcile()
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatalf("phase %q", g.phase())
	}
	g.sourceIsTheGuest()
	if c := countAbort(abortDeadline, cancelPathSource, cancelOutcomeFallback) - fb; c != 1 {
		t.Errorf("fallback counted again: %v", c)
	}
}

// 9. The outcome is delayed (cancel asked, no report yet, inside the
// budget): everything is held.
func TestAbort_DelayedOutcomeHoldsEverything(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.expire()
	g.reconcile()
	g.annotate(rigSrc, map[string]string{
		AnnotationMigrationCancelIssuedAt: time.Now().Add(-45 * time.Second).UTC().Format(time.RFC3339),
	})
	for i := 0; i < 3; i++ {
		g.reconcile()
	}
	g.destinationKept("delayed outcome")
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy {
		t.Fatalf("phase %q", g.phase())
	}
	if destinationCancelIssued(g.mig, g.mustPod(rigDst)) {
		t.Error("fell back inside the source budget")
	}
}

// 10. The event stream is silent on both sides (no source report, no
// destination acknowledgement): after both budgets the outcome is not known
// and the long-standing fallback cleanup runs, recorded as unresolved.
func TestAbort_SilentEverywhereIsUnresolved(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.expire()
	g.reconcile()
	g.annotate(rigSrc, map[string]string{
		AnnotationMigrationCancelIssuedAt: time.Now().Add(-sourceCancelTimeout - time.Second).UTC().Format(time.RFC3339),
	})
	g.reconcile() // fallback: destination cancel
	g.destinationKept("destination cancel just issued")
	g.annotate(rigDst, map[string]string{
		AnnotationMigrationCancelIssuedAt: time.Now().Add(-cancelAckTimeout - time.Second).UTC().Format(time.RFC3339),
	})
	before := countAbort(abortDeadline, cancelPathDestination, "unresolved")
	g.reconcile()
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatalf("phase %q", g.phase())
	}
	if !g.sawEvent("TransferOutcomeUnknown") {
		t.Error("the unknown outcome was not reported")
	}
	if d := countAbort(abortDeadline, cancelPathDestination, "unresolved") - before; d != 1 {
		t.Errorf("unresolved +%v, want +1", d)
	}
}

// 11 and 12. Controller restart while a timeout's or a deletion's cancel is
// pending: a fresh reconciler resumes from the annotations and issues
// nothing twice.
func TestAbort_RestartWhileCancelPending(t *testing.T) {
	for _, trigger := range []string{"timeout", "deletion"} {
		t.Run(trigger, func(t *testing.T) {
			g := newAbortRig(t, true, true)
			if trigger == "timeout" {
				g.expire()
			} else {
				g.delete()
			}
			g.reconcile()
			issued := g.mustPod(rigSrc).Annotations[AnnotationMigrationCancelIssuedAt]
			g.restart()
			g.reconcile()
			if got := g.mustPod(rigSrc).Annotations[AnnotationMigrationCancelIssuedAt]; got != issued {
				t.Error("cancel re-issued after restart")
			}
			g.destinationKept("after restart")
			g.sourceReports(MigrationStatusFailed, cancelledDetailText)
			g.restart()
			g.reconcile()
			g.sourceIsTheGuest()
			if trigger == "deletion" && g.migration() != nil {
				t.Error("deletion did not complete")
			}
			if trigger == "timeout" && g.phase() != migrationv1alpha1.SwiftMigrationPhaseFailed {
				t.Errorf("phase %q", g.phase())
			}
		})
	}
}

// Restart after the source confirmed, while the destination is being
// cleaned up: the stale pass finishes, nothing is undone or doubled.
func TestAbort_RestartDuringCleanupConverges(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.expire()
	g.reconcile()
	g.sourceReports(MigrationStatusFailed, cancelledDetailText)
	stale := g.migration()
	g.reconcile() // Failed
	g.restart()
	if _, err := g.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(stale)}); err != nil {
		t.Fatal(err)
	}
	g.sourceIsTheGuest()
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Errorf("phase %q", g.phase())
	}
}

// 15. Cloud Hypervisor v53 launcher: the timeout uses the destination
// cancel; nothing is deleted before it acknowledges; never a cancel-send.
func TestAbort_V53TimeoutUsesTheDestinationCancel(t *testing.T) {
	g := newAbortRig(t, false, true)
	g.expire()
	g.reconcile()
	if g.mustPod(rigSrc).Annotations[AnnotationMigrationAction] == MigrationActionCancelSend {
		t.Fatal("cancel-send sent to a launcher that cannot run it")
	}
	if !destinationCancelIssued(g.mig, g.mustPod(rigDst)) {
		t.Fatal("destination cancel not issued")
	}
	g.destinationKept("v53 timeout, waiting for the acknowledgement")
	g.annotate(rigDst, map[string]string{
		AnnotationMigrationStatus:    MigrationStatusFailed,
		AnnotationMigrationStatusID:  cancelID(g.mig),
		AnnotationMigrationStatusDtl: MigrationStatusFailedCancelDt,
	})
	g.reconcile()
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatalf("phase %q", g.phase())
	}
	g.sourceIsTheGuest()
}

// A v53 destination refusing the cancel because its VM already runs: the
// destination's `running` commits the migration, and it completes.
func TestAbort_V53TimeoutLosingToCompletionCompletes(t *testing.T) {
	g := newAbortRig(t, false, true)
	g.expire()
	g.reconcile() // destination cancel issued
	// swiftletd refused (writes no status); the receive completed.
	g.annotate(rigDst, map[string]string{
		AnnotationMigrationStatus:   migrationStatusRunning,
		AnnotationMigrationStatusID: recvActionID(g.mig),
	})
	for i := 0; i < 6 && g.phase() == migrationv1alpha1.SwiftMigrationPhaseStopAndCopy; i++ {
		g.reconcile()
	}
	if p := g.phase(); p == migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatalf("a completed transfer failed: %q", p)
	}
	g.destinationKept("v53 transfer won")
}

// Any other failure while the source sends (here: the destination reports
// its receive failed) is resolved on the source before the destination goes.
func TestAbort_OtherFailureWhileSendingIsResolvedFirst(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.annotate(rigDst, map[string]string{
		AnnotationMigrationStatus:    MigrationStatusFailed,
		AnnotationMigrationStatusID:  recvActionID(g.mig),
		AnnotationMigrationStatusDtl: "receive_migration: transport_error",
	})
	g.reconcile()
	if g.phase() == migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatal("failed while the source may still be sending")
	}
	if !sourceCancelIssued(g.mig, g.mustPod(rigSrc)) {
		t.Fatal("failure did not resolve on the source")
	}
	g.sourceReports(MigrationStatusFailed, "the migration failed during the transfer; Cloud Hypervisor resumed the source guest, which is running")
	g.reconcile()
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatalf("phase %q", g.phase())
	}
	g.sourceIsTheGuest()
}

// A source that already reported its send failed needs no cancel: the
// failure lands at once (no extra round trip).
func TestAbort_SourceAlreadyFailedNeedsNoCancel(t *testing.T) {
	g := newAbortRig(t, true, true)
	g.sourceReports(MigrationStatusFailed, "the migration failed during the transfer; Cloud Hypervisor resumed the source guest, which is running")
	g.reconcile()
	if g.phase() != migrationv1alpha1.SwiftMigrationPhaseFailed {
		t.Fatalf("phase %q", g.phase())
	}
	if g.mustPod(rigSrc).Annotations[AnnotationMigrationAction] == MigrationActionCancelSend {
		t.Error("cancelled a send that had already ended")
	}
	g.sourceIsTheGuest()
}

// 16. The cleanup primitive itself refuses to run before ownership is
// settled.
func TestAbort_DestinationCleanupRefusesAnUnsettledOutcome(t *testing.T) {
	g := newAbortRig(t, true, true)
	for _, o := range []abortOutcome{abortPending, abortCommitted} {
		if err := g.r.deleteAbandonedDestination(context.Background(), g.mig, abortResolution{outcome: o}); err == nil {
			t.Errorf("%s: cleanup allowed", o)
		}
	}
	g.destinationKept("refused cleanup")
}
