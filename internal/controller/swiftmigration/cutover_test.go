package swiftmigration

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// cutoverFixture builds a SwiftMigration in the cutover state: phase
// StopAndCopy with the W1 gate observable on src pod (migration-status=
// complete with matching $SEND_ID). Tests adjust per-scenario (e.g.,
// pre-set podRef to simulate post-step-1 cluster state).
func cutoverFixture(t *testing.T) (*migrationv1alpha1.SwiftMigration, *swiftv1alpha1.SwiftGuest, *corev1.Pod, *corev1.Pod) {
	t.Helper()
	mig, guest, src, dst := stopAndCopyFixture(t, "uid-1")
	mig.Status.RecvAttempts = 1
	mig.Status.SendAttempts = 1
	mig.Status.PhaseDetail = migrationv1alpha1.PhaseDetailLiveSrcCompleted
	// Clear SourcePodUID so the UID check (which fires when phaseDetail
	// is in the pre-cutover vocabulary) is skipped — cutover tests
	// focus on cutover correctness, not UID-replacement detection
	// (covered by TestStopAndCopyLive_SrcPodReplaced and
	// TestPreparingLive_SourcePodUIDChanged_*). By the cutover phase
	// of a real migration, the UID check has fired throughout earlier
	// reconciles; the production handler still runs it defensively.
	mig.Status.SourcePodUID = ""
	stamp(src, migrationActionVerbSend, sendActionID(mig), migrationStatusComplete, sendActionID(mig), "ok")
	return mig, guest, src, dst
}

// --- deriveCutoverStep unit tests ----------------------------------

func TestDeriveCutoverStep_FreshEntry_ReturnsStep1Pending(t *testing.T) {
	mig, guest, _, _ := cutoverFixture(t)
	got := deriveCutoverStep(mig, guest, true, "guest-mig-abcdef")
	if got != cutoverStep1Pending {
		t.Errorf("want step1Pending; got %v", got)
	}
}

func TestDeriveCutoverStep_PodRefDoneTimestampMissing_ReturnsTimestampOnly(t *testing.T) {
	mig, guest, _, _ := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	// CutoverStep1At nil simulates leader-handover-between-the-two-patches
	got := deriveCutoverStep(mig, guest, true, "guest-mig-abcdef")
	if got != cutoverStep1TimestampOnly {
		t.Errorf("want timestampOnly; got %v", got)
	}
}

func TestDeriveCutoverStep_Step1DoneSrcExists_ReturnsStep2Pending(t *testing.T) {
	mig, guest, _, _ := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now
	got := deriveCutoverStep(mig, guest, true, "guest-mig-abcdef")
	if got != cutoverStep2Pending {
		t.Errorf("want step2Pending; got %v", got)
	}
}

func TestDeriveCutoverStep_Step12DoneSrcGone_ReturnsStep3Pending(t *testing.T) {
	mig, guest, _, _ := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now
	got := deriveCutoverStep(mig, guest, false, "guest-mig-abcdef")
	if got != cutoverStep3Pending {
		t.Errorf("want step3Pending; got %v", got)
	}
}

// --- end-to-end cutover correctness tests --------------------------

// TestCutover_Step1RetryInPlace: SwiftGuest podRef status patch fails
// on first attempt → phaseTransient → next reconcile retries step 1.
// Verifies forward-only retry semantics: the same step is re-attempted,
// not skipped.
func TestCutover_Step1RetryInPlace_SwiftGuestPatchFails(t *testing.T) {
	mig, guest, src, dst := cutoverFixture(t)
	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig, guest).
		Build()
	c := newSelectiveFailingClient(base)
	c.FailNext(typeKeyOf(&swiftv1alpha1.SwiftGuest{}), VerbStatusPatch, 1,
		apierrors.NewConflict(schema.GroupResource{Group: "swift.kubeswift.io", Resource: "swiftguests"}, "guest", errors.New("simulated")))

	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	// First reconcile: SwiftGuest patch fails → transient.
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err == nil {
		t.Fatalf("first reconcile should yield phaseTransient; got %+v", res)
	}

	// Verify SwiftGuest podRef.name was NOT patched.
	var got swiftv1alpha1.SwiftGuest
	_ = c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got)
	if got.Status.PodRef != nil && got.Status.PodRef.Name == "guest-mig-abcdef" {
		t.Errorf("SwiftGuest podRef should NOT be patched after failed attempt")
	}

	// Second reconcile: succeeds. Re-fetch mig from cluster (status is
	// re-derived from cluster state).
	var refreshed migrationv1alpha1.SwiftMigration
	_ = c.Get(context.Background(), client.ObjectKey{Name: mig.Name, Namespace: "default"}, &refreshed)
	status2 := refreshed.Status.DeepCopy()
	res = r.handleStopAndCopyLive(context.Background(), &refreshed, status2)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("second reconcile failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	if status2.CutoverStep1At == nil {
		t.Errorf("CutoverStep1At should be stamped after successful retry")
	}
	_ = c.Get(context.Background(), client.ObjectKey{Name: "guest", Namespace: "default"}, &got)
	if got.Status.PodRef == nil || got.Status.PodRef.Name != "guest-mig-abcdef" {
		t.Errorf("SwiftGuest podRef.name: want guest-mig-abcdef, got %+v", got.Status.PodRef)
	}
}

// TestCutover_Step1PartialCompletion: SwiftGuest podRef patch
// succeeds, but timestamp persistence (via dispatchResult.persist())
// fails — leader handover. Next reconcile observes podRef==dst but no
// CutoverStep1At → cutoverStep1TimestampOnly → re-stamps timestamp
// without re-patching SwiftGuest.
func TestCutover_Step1PartialCompletion_TimestampOnlyRetry(t *testing.T) {
	mig, guest, src, dst := cutoverFixture(t)
	// Pre-set podRef to dst-pod-name simulating "previous reconcile's
	// SwiftGuest patch landed but the SwiftMigration timestamp persist
	// failed."
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}

	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig, guest).
		Build()
	c := newSelectiveFailingClient(base)

	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	if status.CutoverStep1At == nil {
		t.Errorf("CutoverStep1At should be stamped on timestamp-only retry")
	}

	// Verify SwiftGuest patch was NOT re-issued (already at correct
	// value). Count status patches on SwiftGuest = 0.
	if c.Count(typeKeyOf(&swiftv1alpha1.SwiftGuest{}), VerbStatusPatch) != 0 {
		t.Errorf("SwiftGuest should NOT be re-patched in timestamp-only branch; got %d patches",
			c.Count(typeKeyOf(&swiftv1alpha1.SwiftGuest{}), VerbStatusPatch))
	}
}

// W21 (PR #46 walkthrough): cutoverStep1Timestamp must write BOTH
// status.CutoverStep1At AND the PodRefSwapped=True condition. The
// condition is the safety gate for cancel-post-cutover; without it,
// cancel during the narrow Resuming window would route through the
// pre-cutover code path and destroy the just-migrated guest.
func TestCutover_Step1WritesPodRefSwappedCondition_W21(t *testing.T) {
	mig, guest, src, dst := cutoverFixture(t)
	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig, guest).
		Build()
	r := &SwiftMigrationReconciler{Client: base, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}

	// Both signals must be set on the in-memory status (persisted by
	// dispatchResult at the end of the reconcile).
	if status.CutoverStep1At == nil {
		t.Errorf("CutoverStep1At should be stamped after cutoverStep1")
	}
	var podRefSwapped *metav1.Condition
	for i := range status.Conditions {
		c := &status.Conditions[i]
		if c.Type == migrationv1alpha1.SwiftMigrationConditionPodRefSwapped {
			podRefSwapped = c
			break
		}
	}
	if podRefSwapped == nil {
		t.Fatalf("PodRefSwapped condition not written; W21 regression")
	}
	if podRefSwapped.Status != metav1.ConditionTrue {
		t.Errorf("PodRefSwapped status: want True, got %q", podRefSwapped.Status)
	}
	if podRefSwapped.Reason != migrationv1alpha1.ReasonCutoverStep1Complete {
		t.Errorf("PodRefSwapped reason: want %q, got %q",
			migrationv1alpha1.ReasonCutoverStep1Complete, podRefSwapped.Reason)
	}

	// isPostCutover() reads the Conditions list — verify the gate
	// flips True so downstream call-sites (honorCancel,
	// shouldCheckSourcePodUID) see the post-cutover signal.
	migWithStatus := mig.DeepCopy()
	migWithStatus.Status = *status
	if !isPostCutover(migWithStatus) {
		t.Errorf("isPostCutover should return true after W21 condition write")
	}
}

// W21 idempotency: cutoverStep1TimestampOnly recovery branch (when
// SwiftGuest patch landed but timestamp persist failed) must also
// write the condition, AND must be idempotent when re-fired.
func TestCutover_Step1TimestampOnly_WritesPodRefSwappedCondition_W21(t *testing.T) {
	mig, guest, src, dst := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	// CutoverStep1At nil → cutoverStep1TimestampOnly path

	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig, guest).
		Build()
	r := &SwiftMigrationReconciler{Client: base, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	status := mig.Status.DeepCopy()
	_ = r.handleStopAndCopyLive(context.Background(), mig, status)

	if status.CutoverStep1At == nil {
		t.Errorf("CutoverStep1At should be stamped on timestamp-only retry")
	}
	var found bool
	for _, c := range status.Conditions {
		if c.Type == migrationv1alpha1.SwiftMigrationConditionPodRefSwapped &&
			c.Status == metav1.ConditionTrue {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("PodRefSwapped=True must be written on timestamp-only path too; W21 regression")
	}
}

// TestCutover_Step2RetryInPlace: src pod Delete fails → next reconcile
// observes step 1 done + src exists → retries Delete.
func TestCutover_Step2RetryInPlace_DeleteFails(t *testing.T) {
	mig, guest, src, dst := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now

	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig, guest).
		Build()
	c := newSelectiveFailingClient(base)
	c.FailNext(typeKeyOf(&corev1.Pod{}), VerbDelete, 1,
		apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "delete", 1))

	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err == nil {
		t.Fatalf("first reconcile should yield phaseTransient on Delete failure; got %+v", res)
	}

	// Verify src pod still exists (Delete did fail).
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: src.Name, Namespace: "default"}, &pod); err != nil {
		t.Errorf("src pod should still exist after failed Delete: %v", err)
	}

	// Second reconcile: Delete succeeds.
	res = r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("second reconcile failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	// Verify src pod gone.
	if err := c.Get(context.Background(), client.ObjectKey{Name: src.Name, Namespace: "default"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("src pod should be deleted; got err=%v", err)
	}
}

// TestCutover_Step2NotFoundTreatedAsSuccess: src pod was deleted by a
// previous reconcile but the controller crashed before observing.
// Next reconcile observes step 1 done + src NotFound → step 3 derived,
// not step 2.
func TestCutover_Step2NotFoundDerivedAsStep3(t *testing.T) {
	mig, guest, _, dst := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now

	// No src pod fixture.
	r := newStopAndCopyReconciler(t, mig, guest, dst)

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	// deriveCutoverStep returns step3 when src is gone; handler
	// transitions to Resuming.
	if !res.Advanced {
		t.Errorf("expected phaseAdvance; got %+v", res)
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseResuming {
		t.Errorf("phase: want Resuming, got %q", status.Phase)
	}
}

// TestCutover_Step3PhasePatch: steps 1+2 done; reconcile transitions
// phase to Resuming via phaseAdvance (dispatchResult.persist persists
// the phase change).
func TestCutover_Step3CompletesWithPhaseAdvance(t *testing.T) {
	mig, guest, _, dst := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now

	r := newStopAndCopyReconciler(t, mig, guest, dst) // no src
	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if !res.Advanced {
		t.Fatalf("expected Advanced; got %+v", res)
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseResuming {
		t.Errorf("phase: want Resuming, got %q", status.Phase)
	}
}

// TestCutover_Step1NotReAttempted_OnLeaderHandover: simulate
// leader-handover-after-step-1: cluster state has podRef==dst AND
// CutoverStep1At set. Reconcile must derive step 2 and NOT re-issue
// SwiftGuest patches.
func TestCutover_Step1NotReAttempted_AfterLeaderHandover(t *testing.T) {
	mig, guest, src, dst := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now

	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig, guest).
		Build()
	c := newSelectiveFailingClient(base)
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}

	// Step 1 should NOT be re-attempted: 0 SwiftGuest status patches.
	if c.Count(typeKeyOf(&swiftv1alpha1.SwiftGuest{}), VerbStatusPatch) != 0 {
		t.Errorf("SwiftGuest should NOT be re-patched after leader handover; got %d patches",
			c.Count(typeKeyOf(&swiftv1alpha1.SwiftGuest{}), VerbStatusPatch))
	}
	// Step 2 SHOULD have fired: 1 Pod Delete.
	if c.Count(typeKeyOf(&corev1.Pod{}), VerbDelete) != 1 {
		t.Errorf("Pod Delete count: want 1, got %d", c.Count(typeKeyOf(&corev1.Pod{}), VerbDelete))
	}
}

// TestCutover_Steps12NotReAttempted_OnLeaderHandover: simulate
// leader-handover-after-step-2: cluster state has podRef==dst AND
// CutoverStep1At set AND src pod NotFound. Reconcile must derive
// step 3 directly and re-attempt neither SwiftGuest patch nor Pod
// Delete.
func TestCutover_Steps12NotReAttempted_DerivedToStep3(t *testing.T) {
	mig, guest, _, dst := cutoverFixture(t)
	guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef", Namespace: "default"}
	now := metav1.Now()
	mig.Status.CutoverStep1At = &now

	scheme := testScheme(t)
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, dst). // no src pod
		WithStatusSubresource(mig, guest).
		Build()
	c := newSelectiveFailingClient(base)
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(20)}

	status := mig.Status.DeepCopy()
	res := r.handleStopAndCopyLive(context.Background(), mig, status)
	if !res.Advanced {
		t.Fatalf("expected Advanced; got %+v", res)
	}

	if c.Count(typeKeyOf(&swiftv1alpha1.SwiftGuest{}), VerbStatusPatch) != 0 {
		t.Errorf("SwiftGuest should NOT be re-patched")
	}
	if c.Count(typeKeyOf(&corev1.Pod{}), VerbDelete) != 0 {
		t.Errorf("Pod Delete should NOT be re-issued; got %d",
			c.Count(typeKeyOf(&corev1.Pod{}), VerbDelete))
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseResuming {
		t.Errorf("phase: want Resuming, got %q", status.Phase)
	}
}

// TestCutover_StateDrivenRecovery: each cutover step has a
// corresponding cluster-state fixture; reconcile picks the correct
// next step from observation alone. This is the load-bearing
// reconcile-loop-recovery property per §2.4.
func TestCutover_StateDrivenRecovery_AllStepsCovered(t *testing.T) {
	cases := []struct {
		name         string
		setup        func(mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, src **corev1.Pod)
		expectedStep cutoverStep
	}{
		{
			name: "fresh entry → step 1",
			setup: func(mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, src **corev1.Pod) {
				// nothing
			},
			expectedStep: cutoverStep1Pending,
		},
		{
			name: "podRef done, timestamp missing → step 1 timestamp",
			setup: func(mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, src **corev1.Pod) {
				guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef"}
			},
			expectedStep: cutoverStep1TimestampOnly,
		},
		{
			name: "step 1 fully done, src exists → step 2",
			setup: func(mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, src **corev1.Pod) {
				guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef"}
				now := metav1.Now()
				mig.Status.CutoverStep1At = &now
			},
			expectedStep: cutoverStep2Pending,
		},
		{
			name: "step 1+2 done, src gone → step 3",
			setup: func(mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, src **corev1.Pod) {
				guest.Status.PodRef = &corev1.ObjectReference{Name: "guest-mig-abcdef"}
				now := metav1.Now()
				mig.Status.CutoverStep1At = &now
				*src = nil
			},
			expectedStep: cutoverStep3Pending,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mig, guest, src, _ := cutoverFixture(t)
			tc.setup(mig, guest, &src)
			srcPresent := src != nil
			got := deriveCutoverStep(mig, guest, srcPresent, "guest-mig-abcdef")
			if got != tc.expectedStep {
				t.Errorf("want %v, got %v", tc.expectedStep, got)
			}
		})
	}
}
