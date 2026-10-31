package swiftmigration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

const capableFeatures = "artifact-requirements migration-events migration-cancel"

// sourceCancelFixture: a live migration mid-transfer with a cancel
// requested; the source launcher (named after the guest) advertises
// features and carries the given annotations, the destination exists.
func sourceCancelFixture(t *testing.T, features string, srcAnn map[string]string) (*SwiftMigrationReconciler, client.Client, *migrationv1alpha1.SwiftMigration, *record.FakeRecorder) {
	t.Helper()
	mig, guest, dst := cancelFixture(t, 5*time.Minute)
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhaseStopAndCopy
	ann := map[string]string{AnnotationLauncherFeatures: features}
	for k, v := range srcAnn {
		ann[k] = v
	}
	src := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", Annotations: ann},
		Spec:       corev1.PodSpec{NodeName: "worker-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst, src).
		WithStatusSubresource(mig).
		Build()
	rec := record.NewFakeRecorder(20)
	return &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: rec}, c, mig, rec
}

// sendingAnn: the source's send of this migration in progress, with the
// cancel-send already issued at `issued` ago.
func sendingAnn(mig *migrationv1alpha1.SwiftMigration, issued time.Duration) map[string]string {
	return map[string]string{
		AnnotationMigrationAction:         MigrationActionCancelSend,
		AnnotationMigrationActionID:       cancelSendID(mig),
		AnnotationMigrationCancelIssuedAt: time.Now().Add(-issued).UTC().Format(time.RFC3339),
		AnnotationMigrationStatus:         migrationStatusSending,
		AnnotationMigrationStatusID:       sendActionID(mig),
	}
}

func getPod(t *testing.T, c client.Client, name string) (*corev1.Pod, bool) {
	t.Helper()
	var p corev1.Pod
	err := c.Get(context.Background(), client.ObjectKey{Name: name, Namespace: "default"}, &p)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return &p, true
}

func getMig(t *testing.T, c client.Client, mig *migrationv1alpha1.SwiftMigration) *migrationv1alpha1.SwiftMigration {
	t.Helper()
	var m migrationv1alpha1.SwiftMigration
	if err := c.Get(context.Background(), client.ObjectKey{Name: mig.Name, Namespace: "default"}, &m); err != nil {
		t.Fatalf("get migration: %v", err)
	}
	return &m
}

func cancelCount(path, outcome string) float64 {
	return testutil.ToFloat64(metrics.MigrationCancelTotal.WithLabelValues(path, outcome))
}

// A source that can cancel is asked to; the destination is left alone and
// nothing is deleted while the outcome is unknown.
func TestSourceCancel_IssuedOnACapableSource(t *testing.T) {
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, map[string]string{
		AnnotationMigrationAction:     "send",
		AnnotationMigrationActionID:   "m1:send:1",
		AnnotationMigrationActionArgs: `{"target_url":"tcp:127.0.0.1:6790"}`,
		AnnotationMigrationStatus:     migrationStatusSending,
		AnnotationMigrationStatusID:   "m1:send:1",
	})
	handled, res, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil || res.RequeueAfter == 0 {
		t.Fatalf("handled=%v res=%v err=%v", handled, res, err)
	}
	src, _ := getPod(t, c, "guest")
	if src.Annotations[AnnotationMigrationAction] != MigrationActionCancelSend ||
		src.Annotations[AnnotationMigrationActionID] != cancelSendID(mig) {
		t.Errorf("source action: %q/%q", src.Annotations[AnnotationMigrationAction], src.Annotations[AnnotationMigrationActionID])
	}
	if _, ok := src.Annotations[AnnotationMigrationActionArgs]; ok {
		t.Error("the send's args must not stay with the cancel")
	}
	if src.Annotations[AnnotationMigrationCancelIssuedAt] == "" {
		t.Error("cancel issue time not recorded")
	}
	dst, ok := getPod(t, c, "guest-mig-abcdef")
	if !ok {
		t.Fatal("destination deleted while the cancel outcome is unknown")
	}
	if dst.Annotations[AnnotationMigrationAction] != "" {
		t.Errorf("destination got an action: %q", dst.Annotations[AnnotationMigrationAction])
	}
	if p := getMig(t, c, mig).Status.Phase; p == migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Error("cancelled before the source reported")
	}
}

// The send reports it ended: the source guest runs, so the destination goes
// and the migration is Cancelled. The cancel action is taken off the source.
func TestSourceCancel_SourceReportsFailed_Cancels(t *testing.T) {
	ann := sendingAnn(nil0(), 3*time.Second)
	ann[AnnotationMigrationStatus] = MigrationStatusFailed
	ann[AnnotationMigrationStatusDtl] = "cancelled: Cloud Hypervisor stopped the migration on request; the source guest is running"
	ann[AnnotationMigrationCancelResult] = "requested"
	ann[AnnotationMigrationCancelResultID] = "m1:cancel-send:0"
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, ann)
	before := cancelCount(cancelPathSource, cancelOutcomeCancelled)

	handled, _, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	m := getMig(t, c, mig)
	if m.Status.Phase != migrationv1alpha1.SwiftMigrationPhaseCancelled ||
		m.Status.FailureReason != migrationv1alpha1.FailureReasonCancelled {
		t.Fatalf("phase=%q reason=%q", m.Status.Phase, m.Status.FailureReason)
	}
	if !strings.Contains(m.Status.FailureMessage, "cancelled on the source (Cloud Hypervisor stopped") {
		t.Errorf("message: %q", m.Status.FailureMessage)
	}
	if _, ok := getPod(t, c, "guest-mig-abcdef"); ok {
		t.Error("destination not deleted")
	}
	src, ok := getPod(t, c, "guest")
	if !ok {
		t.Fatal("source pod deleted")
	}
	if src.Annotations[AnnotationMigrationAction] != "" {
		t.Errorf("cancel action left on the source: %q", src.Annotations[AnnotationMigrationAction])
	}
	if got := cancelCount(cancelPathSource, cancelOutcomeCancelled) - before; got != 1 {
		t.Errorf("cancel metric +%v, want +1", got)
	}
}

// A pass working from a stale copy of the migration (cache not yet showing
// Cancelled) after the source cancel finished: it must not start the
// destination cancel on the terminating destination (seen on the lab).
func TestSourceCancel_StalePassAfterFinishDoesNotStartTheDestinationCancel(t *testing.T) {
	ann := sendingAnn(nil0(), 3*time.Second)
	ann[AnnotationMigrationStatus] = MigrationStatusFailed
	ann[AnnotationMigrationStatusDtl] = "cancelled: stopped"
	ann[AnnotationMigrationCancelResult] = "requested"
	ann[AnnotationMigrationCancelResultID] = "m1:cancel-send:0"
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, ann)
	stale := mig.DeepCopy()
	if handled, _, err := r.honorCancel(context.Background(), mig); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	// The destination is still terminating in the cache.
	dst := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "guest-mig-abcdef", Namespace: "default"}}
	if err := c.Create(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	before := cancelCount(cancelPathSource, cancelOutcomeCancelled)
	if _, _, err := r.honorCancel(context.Background(), stale); err != nil {
		t.Fatalf("stale pass: %v", err)
	}
	got, _ := getPod(t, c, "guest-mig-abcdef")
	if got != nil && got.Annotations[AnnotationMigrationAction] == MigrationActionCancel {
		t.Error("stale pass started the destination cancel")
	}
	if p := getMig(t, c, mig).Status.Phase; p != migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Errorf("phase %q", p)
	}
	if d := cancelCount(cancelPathSource, cancelOutcomeCancelled) - before; d != 0 {
		t.Errorf("counted twice: +%v", d)
	}
}

// nil0 builds the migration name sendingAnn needs before the fixture exists.
func nil0() *migrationv1alpha1.SwiftMigration {
	m := newMigrationWithUID("m1", "default", "abcdef1234567890abcdef1234567890")
	return m
}

// The cancel lost the race: the send completed. The migration is committed
// and completes; the destination -- now the only guest -- is kept.
func TestSourceCancel_SourceReportsComplete_IsIgnored(t *testing.T) {
	ann := sendingAnn(nil0(), 3*time.Second)
	ann[AnnotationMigrationStatus] = migrationStatusComplete
	ann[AnnotationMigrationCancelResult] = "requested"
	ann[AnnotationMigrationCancelResultID] = "m1:cancel-send:0"
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, ann)
	before := cancelCount(cancelPathSource, cancelOutcomeIgnored)

	handled, _, err := r.honorCancel(context.Background(), mig)
	if handled || err != nil {
		t.Fatalf("a committed migration must continue: handled=%v err=%v", handled, err)
	}
	if _, ok := getPod(t, c, "guest-mig-abcdef"); !ok {
		t.Fatal("destination deleted after the commit point")
	}
	m := getMig(t, c, mig)
	if m.Status.Phase == migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Fatal("a completed transfer reported Cancelled")
	}
	ignored := false
	for _, cond := range m.Status.Conditions {
		if cond.Type == migrationv1alpha1.SwiftMigrationConditionCancelIgnored && cond.Status == metav1.ConditionTrue {
			ignored = true
		}
	}
	if !ignored {
		t.Error("CancelIgnored not set")
	}
	if got := cancelCount(cancelPathSource, cancelOutcomeIgnored) - before; got != 1 {
		t.Errorf("ignored metric +%v, want +1", got)
	}
}

// The destination reports the guest running while the source has not
// reported: committed too. Never delete it.
func TestSourceCancel_DestinationRunning_IsIgnored(t *testing.T) {
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, sendingAnn(nil0(), 3*time.Second))
	dst, _ := getPod(t, c, "guest-mig-abcdef")
	patch := client.MergeFrom(dst.DeepCopy())
	dst.Annotations = map[string]string{
		AnnotationMigrationStatus:   migrationStatusRunning,
		AnnotationMigrationStatusID: recvActionID(mig),
	}
	if err := c.Patch(context.Background(), dst, patch); err != nil {
		t.Fatal(err)
	}
	handled, _, err := r.honorCancel(context.Background(), mig)
	if handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if _, ok := getPod(t, c, "guest-mig-abcdef"); !ok {
		t.Fatal("running destination deleted")
	}
}

// Still sending within the budget: wait, delete nothing.
func TestSourceCancel_StillSending_Waits(t *testing.T) {
	ann := sendingAnn(nil0(), 3*time.Second)
	ann[AnnotationMigrationCancelResult] = "requested"
	ann[AnnotationMigrationCancelResultID] = "m1:cancel-send:0"
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, ann)
	handled, res, err := r.honorCancel(context.Background(), mig)
	if !handled || err != nil || res.RequeueAfter == 0 {
		t.Fatalf("handled=%v res=%v err=%v", handled, res, err)
	}
	if _, ok := getPod(t, c, "guest-mig-abcdef"); !ok {
		t.Fatal("destination deleted while the source is still sending")
	}
	if p := getMig(t, c, mig).Status.Phase; p == migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Fatal("cancelled while sending")
	}
}

// No send in progress on the source (the cancel came before it started):
// nothing left the source; cancel.
func TestSourceCancel_NotActive_BeforeTheSend_Cancels(t *testing.T) {
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, map[string]string{
		AnnotationMigrationAction:         MigrationActionCancelSend,
		AnnotationMigrationActionID:       "m1:cancel-send:0",
		AnnotationMigrationCancelIssuedAt: time.Now().UTC().Format(time.RFC3339),
		AnnotationMigrationCancelResult:   "not-active",
		AnnotationMigrationCancelResultID: "m1:cancel-send:0",
		// An earlier migration's status (this pod was its destination).
		AnnotationMigrationStatus:   migrationStatusRunning,
		AnnotationMigrationStatusID: "m0:recv:1",
	})
	if handled, _, err := r.honorCancel(context.Background(), mig); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if p := getMig(t, c, mig).Status.Phase; p != migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Fatalf("phase %q", p)
	}
	if _, ok := getPod(t, c, "guest-mig-abcdef"); ok {
		t.Error("destination not deleted")
	}
}

// not-active, but the source's send of this migration says sending: the
// send was accepted just after the cancel looked. Wait for the send.
func TestSourceCancel_NotActive_ButSending_Waits(t *testing.T) {
	ann := sendingAnn(nil0(), 3*time.Second)
	ann[AnnotationMigrationCancelResult] = "not-active"
	ann[AnnotationMigrationCancelResultID] = "m1:cancel-send:0"
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, ann)
	if handled, _, err := r.honorCancel(context.Background(), mig); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if _, ok := getPod(t, c, "guest-mig-abcdef"); !ok {
		t.Fatal("destination deleted while the send runs")
	}
}

// The cancel could not be delivered (RPC failure): the destination-kill
// cancel takes over at once, and the source cancel is not issued again.
func TestSourceCancel_RPCFailure_FallsBackToTheDestination(t *testing.T) {
	ann := sendingAnn(nil0(), 3*time.Second)
	ann[AnnotationMigrationCancelResult] = cancelResultFailed
	ann[AnnotationMigrationCancelResultID] = "m1:cancel-send:0"
	ann[AnnotationMigrationCancelResultDetail] = "cancel_migration: ch_status_error"
	r, c, mig, rec := sourceCancelFixture(t, capableFeatures, ann)
	before := cancelCount(cancelPathSource, cancelOutcomeFallback)
	if handled, _, err := r.honorCancel(context.Background(), mig); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	dst, ok := getPod(t, c, "guest-mig-abcdef")
	if !ok {
		t.Fatal("destination deleted without its cancel")
	}
	if dst.Annotations[AnnotationMigrationAction] != MigrationActionCancel {
		t.Errorf("destination cancel not issued: %v", dst.Annotations)
	}
	if !sawEvent(rec, "CancelFallback") {
		t.Error("no CancelFallback event")
	}
	if got := cancelCount(cancelPathSource, cancelOutcomeFallback) - before; got != 1 {
		t.Errorf("fallback metric +%v, want +1", got)
	}
	// Next pass: the destination path carries on; nothing re-issued on the source.
	before = cancelCount(cancelPathSource, cancelOutcomeFallback)
	if handled, _, err := r.honorCancel(context.Background(), getMig(t, c, mig)); !handled || err != nil {
		t.Fatalf("second pass handled=%v err=%v", handled, err)
	}
	if got := cancelCount(cancelPathSource, cancelOutcomeFallback) - before; got != 0 {
		t.Errorf("fallback counted again: +%v", got)
	}
}

// No report within the budget (source launcher stuck): fall back.
func TestSourceCancel_NoReportWithinBudget_FallsBack(t *testing.T) {
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, sendingAnn(nil0(), sourceCancelTimeout+time.Second))
	if handled, _, err := r.honorCancel(context.Background(), mig); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	dst, _ := getPod(t, c, "guest-mig-abcdef")
	if dst.Annotations[AnnotationMigrationAction] != MigrationActionCancel {
		t.Errorf("destination cancel not issued: %v", dst.Annotations)
	}
	src, _ := getPod(t, c, "guest")
	if src.Annotations[AnnotationMigrationCancelResult] != cancelResultFailed ||
		!strings.Contains(src.Annotations[AnnotationMigrationCancelResultDetail], "did not report") {
		t.Errorf("fallback not recorded on the source: %v", src.Annotations)
	}
}

// v53 / older launchers: the destination-kill cancel, as before.
func TestSourceCancel_LegacyLauncher_UsesTheDestinationCancel(t *testing.T) {
	for name, features := range map[string]string{
		"no annotation":       "",
		"older swiftletd":     "artifact-requirements",
		"events but no v54":   "artifact-requirements migration-events",
		"unrelated substring": "migration-cancelled",
	} {
		t.Run(name, func(t *testing.T) {
			r, c, mig, _ := sourceCancelFixture(t, features, map[string]string{
				AnnotationMigrationStatus:   migrationStatusSending,
				AnnotationMigrationStatusID: "m1:send:1",
			})
			if handled, _, err := r.honorCancel(context.Background(), mig); !handled || err != nil {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			src, _ := getPod(t, c, "guest")
			if src.Annotations[AnnotationMigrationAction] == MigrationActionCancelSend {
				t.Error("cancel-send sent to a launcher that cannot run it")
			}
			dst, _ := getPod(t, c, "guest-mig-abcdef")
			if dst.Annotations[AnnotationMigrationAction] != MigrationActionCancel {
				t.Errorf("destination cancel not issued: %v", dst.Annotations)
			}
		})
	}
}

// Controller restart while a cancel is pending: a new reconciler (no memory)
// picks up from the annotations, issuing nothing twice.
func TestSourceCancel_ControllerRestartWhilePending(t *testing.T) {
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, map[string]string{
		AnnotationMigrationStatus:   migrationStatusSending,
		AnnotationMigrationStatusID: "m1:send:1",
	})
	if _, _, err := r.honorCancel(context.Background(), mig); err != nil {
		t.Fatal(err)
	}
	src, _ := getPod(t, c, "guest")
	issuedAt := src.Annotations[AnnotationMigrationCancelIssuedAt]

	// "Restart": a fresh reconciler on the same cluster state.
	r2 := &SwiftMigrationReconciler{Client: c, Scheme: r.Scheme, Recorder: record.NewFakeRecorder(10)}
	if handled, _, err := r2.honorCancel(context.Background(), getMig(t, c, mig)); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	src, _ = getPod(t, c, "guest")
	if src.Annotations[AnnotationMigrationCancelIssuedAt] != issuedAt {
		t.Error("cancel re-issued after restart")
	}
	if _, ok := getPod(t, c, "guest-mig-abcdef"); !ok {
		t.Fatal("destination deleted while pending")
	}

	// swiftletd reports; the restarted controller finishes the cancel.
	patch := client.MergeFrom(src.DeepCopy())
	src.Annotations[AnnotationMigrationStatus] = MigrationStatusFailed
	src.Annotations[AnnotationMigrationStatusDtl] = "cancelled: stopped"
	if err := c.Patch(context.Background(), src, patch); err != nil {
		t.Fatal(err)
	}
	if handled, _, err := r2.honorCancel(context.Background(), getMig(t, c, mig)); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if p := getMig(t, c, mig).Status.Phase; p != migrationv1alpha1.SwiftMigrationPhaseCancelled {
		t.Fatalf("phase %q", p)
	}
}

// A source pod being deleted is not asked to cancel.
func TestSourceCancel_TerminatingSource_UsesTheDestinationCancel(t *testing.T) {
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, nil)
	src, _ := getPod(t, c, "guest")
	src.Finalizers = []string{"test/hold"}
	if err := c.Update(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if handled, _, err := r.honorCancel(context.Background(), mig); !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	dst, _ := getPod(t, c, "guest-mig-abcdef")
	if dst.Annotations[AnnotationMigrationAction] != MigrationActionCancel {
		t.Errorf("destination cancel not issued: %v", dst.Annotations)
	}
}

// A terminal migration's cancel-send is cleared from the source even if the
// pass that finished the cancel stopped short of it (#823's busy namespace).
func TestSourceCancel_TerminalPassClearsTheCancelAction(t *testing.T) {
	r, c, mig, _ := sourceCancelFixture(t, capableFeatures, sendingAnn(nil0(), time.Second))
	if err := r.clearLauncherCancel(context.Background(), mig); err != nil {
		t.Fatal(err)
	}
	src, _ := getPod(t, c, "guest")
	if src.Annotations[AnnotationMigrationAction] != "" || src.Annotations[AnnotationMigrationActionID] != "" {
		t.Errorf("cancel action not cleared: %v", src.Annotations)
	}
	if src.Annotations[AnnotationMigrationStatus] != migrationStatusSending {
		t.Error("status annotations must be left alone")
	}
}

func TestIsConnectionNeverEstablished(t *testing.T) {
	yes := "the migration failed before its connection to the destination was established; the source guest is running"
	if !isConnectionNeverEstablished(yes) || !isLocalStunnelNotReady(yes) {
		t.Error("event-reported never-established failure not recognised")
	}
	for _, no := range []string{
		"the migration failed during the transfer; Cloud Hypervisor resumed the source guest, which is running",
		"cancelled: Cloud Hypervisor stopped the migration on request; the source guest is running",
		"Cloud Hypervisor exited after the migration failed",
	} {
		if isConnectionNeverEstablished(no) || isLocalStunnelNotReady(no) {
			t.Errorf("retried a failure that moved guest state: %q", no)
		}
	}
}

func sawEvent(rec *record.FakeRecorder, reason string) bool {
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, reason) {
				return true
			}
		default:
			return false
		}
	}
}
