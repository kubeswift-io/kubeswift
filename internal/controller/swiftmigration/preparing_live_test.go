package swiftmigration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// preparingLiveFixture builds the standard set of cluster objects
// handlePreparingLive expects: SwiftMigration in Preparing phase
// (mode=live, status.SourcePodUID stamped), SwiftGuest, and the
// source launcher pod.
func preparingLiveFixture(t *testing.T, srcUID types.UID) (*migrationv1alpha1.SwiftMigration, *swiftv1alpha1.SwiftGuest, *corev1.Pod) {
	t.Helper()
	mig := newMigrationWithUID("m1", "default", "abcdef1234567890abcdef1234567890")
	mig.Spec.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Spec.Target.NodeName = "worker-2"
	mig.Status.Phase = migrationv1alpha1.SwiftMigrationPhasePreparing
	mig.Status.Mode = migrationv1alpha1.SwiftMigrationModeLive
	mig.Status.SourcePodUID = srcUID

	guest := &swiftv1alpha1.SwiftGuest{
		ObjectMeta: metav1.ObjectMeta{Name: "guest", Namespace: "default", UID: "guest-uid"},
	}
	src := templateSrcPod("guest", "default")
	src.UID = srcUID
	return mig, guest, src
}

func TestPreparingLive_FirstReconcile_CreatesDstPodAndStampsTimestamp(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("unexpected failure: err=%v msg=%q", res.Err, res.FailureMsg)
	}
	if res.Requeue == 0 {
		t.Errorf("expected phaseRequeue, got %+v", res)
	}
	if status.PreparingStartedAt == nil {
		t.Errorf("PreparingStartedAt not stamped")
	}
	if status.DestinationPodRef == nil || status.DestinationPodRef.Name != "guest-mig-abcdef" {
		t.Errorf("DestinationPodRef: want guest-mig-abcdef, got %+v", status.DestinationPodRef)
	}
	// Pod should now exist on the fake cluster.
	var dst corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Name: "guest-mig-abcdef", Namespace: "default"}, &dst); err != nil {
		t.Fatalf("dst pod not created: %v", err)
	}
	if dst.Spec.NodeName != "worker-2" {
		t.Errorf("dst NodeName: want worker-2, got %q", dst.Spec.NodeName)
	}
	if dst.Annotations[AnnotationMigrationPhase2Ack] != AnnotationMigrationPhase2AckValue {
		t.Errorf("ack annotation missing on created dst pod")
	}
	if dst.Labels[LabelMigrationRole] != MigrationRoleDestination {
		t.Errorf("migration-role label missing")
	}
}

func TestPreparingLive_SecondReconcile_PodNotReady_StaysWithinBudget(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")
	startedAt := metav1.NewTime(time.Now().Add(-10 * time.Second))
	mig.Status.PreparingStartedAt = &startedAt
	dst := preExistingDstPod(mig, guest, scheme, t)
	// Pod exists but status.phase is empty (Pending). Not Ready.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureMsg != "" {
		t.Fatalf("unexpected failure: msg=%q", res.FailureMsg)
	}
	if res.Advanced {
		t.Errorf("should NOT advance when dst pod not Ready and within budget")
	}
	if res.Requeue == 0 {
		t.Errorf("expected phaseRequeue")
	}
	if status.PhaseDetail != phaseDetailLivePreparingWaiting {
		t.Errorf("phaseDetail: want %q, got %q", phaseDetailLivePreparingWaiting, status.PhaseDetail)
	}
}

func TestPreparingLive_PodReady_AdvancesToStopAndCopy(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")
	startedAt := metav1.NewTime(time.Now().Add(-10 * time.Second))
	mig.Status.PreparingStartedAt = &startedAt

	dst := preExistingDstPod(mig, guest, scheme, t)
	dst.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureMsg != "" {
		t.Fatalf("unexpected failure: msg=%q", res.FailureMsg)
	}
	if !res.Advanced {
		t.Errorf("should advance when dst pod Ready; got %+v", res)
	}
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseStopAndCopy {
		t.Errorf("phase: want StopAndCopy, got %q", status.Phase)
	}
}

// TestPreparingLive_BudgetExceeded_FailsWithDstNeverReady is the
// regression guard for Commit C's semantic refinement: when the 60s
// PreparingLive budget expires with the dst pod alive-but-not-Ready,
// the failure reason must be DstNeverReady (NOT PodTerminated as
// Phase 3a reported). A future refactor that collapses these two
// codes back together would silently undo the operator-visible
// distinction; this test fails loudly if that happens.
//
// See FailureReasonDstNeverReady docstring for the semantic-
// refinement rationale and operator-upgrade note.
func TestPreparingLive_BudgetExceeded_FailsWithDstNeverReady(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")
	// PreparingStartedAt is 90s ago — past the 60s budget.
	startedAt := metav1.NewTime(time.Now().Add(-90 * time.Second))
	mig.Status.PreparingStartedAt = &startedAt

	dst := preExistingDstPod(mig, guest, scheme, t)
	// Not Ready (no Conditions).

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureMsg == "" {
		t.Fatalf("expected phaseFailure on budget exceeded; got %+v", res)
	}
	if res.FailureReason != migrationv1alpha1.FailureReasonDstNeverReady {
		t.Errorf("FailureReason: want DstNeverReady (Phase 3b PR 2 semantic refinement from PodTerminated), got %q", res.FailureReason)
	}
	if !strings.Contains(res.FailureMsg, "never reached Ready") {
		t.Errorf("FailureMsg: want 'never reached Ready', got %q", res.FailureMsg)
	}
}

// budgetExceededDst is the DstNeverReady setup: Preparing started 90s ago and
// the destination pod, with UID dst-uid, is not Ready. The events are listed
// through the fake client as APIReader.
func budgetExceededDst(t *testing.T, podStatus corev1.PodStatus, events func(dst, src *corev1.Pod) []client.Object) (*SwiftMigrationReconciler, *record.FakeRecorder, *migrationv1alpha1.SwiftMigration) {
	t.Helper()
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")
	startedAt := metav1.NewTime(time.Now().Add(-90 * time.Second))
	mig.Status.PreparingStartedAt = &startedAt
	dst := preExistingDstPod(mig, guest, scheme, t)
	dst.UID = "dst-uid"
	dst.Status = podStatus

	objs := []client.Object{mig, guest, src, dst}
	if events != nil {
		objs = append(objs, events(dst, src)...)
	}
	c := withEventIndex(fake.NewClientBuilder().WithScheme(scheme)).
		WithObjects(objs...).
		WithStatusSubresource(mig).
		Build()
	rec := record.NewFakeRecorder(10)
	return &SwiftMigrationReconciler{Client: c, APIReader: c, Scheme: scheme, Recorder: rec}, rec, mig
}

// The lab's round-4 failure: the storage would not attach the volume to the
// target node (Longhorn does not live-migrate a degraded volume), so the
// destination pod sat in PodInitializing and the migration said only "never
// reached Ready". The message now carries the pod's FailedAttachVolume event,
// and the same text is recorded as a Warning event on the SwiftMigration.
func TestPreparingLive_BudgetExceeded_MessageNamesAttachFailure(t *testing.T) {
	attach := `AttachVolume.Attach failed for volume "pvc-1" : rpc error: code = Internal desc = volume pvc-1 failed to attach to node worker-2`
	r, rec, mig := budgetExceededDst(t, corev1.PodStatus{
		Phase:                 corev1.PodPending,
		InitContainerStatuses: []corev1.ContainerStatus{waiting("network-init", "PodInitializing", "")},
		ContainerStatuses:     []corev1.ContainerStatus{waiting("launcher", "PodInitializing", "")},
	}, func(dst, src *corev1.Pod) []client.Object {
		now := time.Now()
		return []client.Object{
			podEvent("dst.scheduled", dst, corev1.EventTypeNormal, "Scheduled", "Successfully assigned default/guest-mig-abcdef to worker-2", now),
			podEvent("dst.attach", dst, corev1.EventTypeWarning, "FailedAttachVolume", attach, now.Add(-5*time.Second)),
			podEvent("src.unhealthy", src, corev1.EventTypeWarning, "Unhealthy", "Readiness probe failed", now),
		}
	})

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonDstNeverReady {
		t.Fatalf("FailureReason: want DstNeverReady, got %q (msg %q)", res.FailureReason, res.FailureMsg)
	}
	want := `destination pod "guest-mig-abcdef" never reached Ready within 1m0s budget: ` +
		`init container "network-init" waiting: PodInitializing; Warning FailedAttachVolume: ` + attach
	if res.FailureMsg != want {
		t.Errorf("FailureMsg:\n got %q\nwant %q", res.FailureMsg, want)
	}
	if !recordedEvent(rec, "Warning "+eventReasonDestinationPodNeverReady+" "+want) {
		t.Errorf("no %s Warning event carrying the failure message", eventReasonDestinationPodNeverReady)
	}
}

// An unschedulable destination pod: the scheduler's message names why.
func TestPreparingLive_BudgetExceeded_MessageNamesSchedulingFailure(t *testing.T) {
	unschedulable := "0/3 nodes are available: 1 node(s) didn't match Pod's node affinity/selector, 2 node(s) had untolerated taint {node.kubernetes.io/unreachable: }."
	r, _, mig := budgetExceededDst(t, corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, Message: unschedulable,
		}},
	}, nil)

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonDstNeverReady {
		t.Fatalf("FailureReason: want DstNeverReady, got %q (msg %q)", res.FailureReason, res.FailureMsg)
	}
	if !strings.Contains(res.FailureMsg, "never reached Ready within 1m0s budget: not scheduled: Unschedulable: "+unschedulable) {
		t.Errorf("FailureMsg does not carry the scheduling failure: %q", res.FailureMsg)
	}
}

// When neither the pod's status nor its Warning events say anything, the
// message is what it always was.
func TestPreparingLive_BudgetExceeded_NoCauseLeavesMessageUnchanged(t *testing.T) {
	r, rec, mig := budgetExceededDst(t, corev1.PodStatus{}, func(dst, _ *corev1.Pod) []client.Object {
		return []client.Object{
			podEvent("dst.scheduled", dst, corev1.EventTypeNormal, "Scheduled", "Successfully assigned default/guest-mig-abcdef to worker-2", time.Now()),
		}
	})

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	want := `destination pod "guest-mig-abcdef" never reached Ready within 1m0s budget`
	if res.FailureMsg != want || res.FailureReason != migrationv1alpha1.FailureReasonDstNeverReady {
		t.Errorf("got %q / %q, want %q / DstNeverReady", res.FailureMsg, res.FailureReason, want)
	}
	if !recordedEvent(rec, "Warning "+eventReasonDestinationPodNeverReady+" "+want) {
		t.Errorf("no %s Warning event", eventReasonDestinationPodNeverReady)
	}
}

func TestPreparingLive_IdempotentReentry_ExistingPodNotRecreated(t *testing.T) {
	// Simulates leader handover: dst pod already exists with correct
	// shape. Reconcile must skip Create (no AlreadyExists error
	// surfaces on the fake client even if it tried, but verify via
	// counter on selectiveFailingClient).
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")
	startedAt := metav1.NewTime(time.Now().Add(-10 * time.Second))
	mig.Status.PreparingStartedAt = &startedAt

	dst := preExistingDstPod(mig, guest, scheme, t)

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, dst).
		WithStatusSubresource(mig).
		Build()
	c := newSelectiveFailingClient(base)
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureMsg != "" {
		t.Fatalf("unexpected failure: msg=%q", res.FailureMsg)
	}
	// Create should NOT have been issued since the pod already exists.
	createCount := c.Count(typeKeyOf(&corev1.Pod{}), VerbCreate)
	if createCount != 0 {
		t.Errorf("Create on Pod should be 0 (idempotent re-entry); got %d", createCount)
	}
}

// TestPreparingLive_ExistingPodWrongShape_Fails is the regression
// guard for Commit C's DstPodConflict wiring: a dst pod with the
// expected deterministic name but unrelated labels (foreign
// collision, not a leader-handover idempotent re-entry) must fail
// with DstPodConflict, not the catch-all Other.
func TestPreparingLive_ExistingPodWrongShape_Fails(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")
	// Pod with the correct deterministic name but no migration labels —
	// simulates a name collision with an unrelated workload.
	bogusDst := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "guest-mig-abcdef",
			Namespace: "default",
			Labels:    map[string]string{"unrelated": "yes"},
		},
		Spec: corev1.PodSpec{NodeName: "worker-2"},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src, bogusDst).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if !strings.Contains(res.FailureMsg, "does not match expected") {
		t.Errorf("FailureMsg: want collision message, got %q", res.FailureMsg)
	}
	if res.FailureReason != migrationv1alpha1.FailureReasonDstPodConflict {
		t.Errorf("FailureReason: want DstPodConflict (Phase 3b PR 2; refined from Other), got %q", res.FailureReason)
	}
}

func TestPreparingLive_SourcePodUIDChanged_FailsWithSourcePodReplaced(t *testing.T) {
	scheme := testScheme(t)
	// Status has UID-A, but the actual source pod has UID-B.
	mig, guest, src := preparingLiveFixture(t, "uid-A")
	src.UID = "uid-B" // pod was K8s-recreated mid-migration

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("FailureReason: want SourcePodReplaced, got %q", res.FailureReason)
	}
}

func TestPreparingLive_SourcePodGone_FailsWithSourcePodReplaced(t *testing.T) {
	scheme := testScheme(t)
	mig, guest, _ := preparingLiveFixture(t, "uid-A")
	// No src pod added — simulates K8s-deletion mid-migration.

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest).
		WithStatusSubresource(mig).
		Build()
	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme}

	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.FailureReason != migrationv1alpha1.FailureReasonSourcePodReplaced {
		t.Errorf("FailureReason: want SourcePodReplaced, got %q", res.FailureReason)
	}
}

func TestPreparingLive_DefensiveGuard_NotLiveMode(t *testing.T) {
	r := &SwiftMigrationReconciler{}
	mig := &migrationv1alpha1.SwiftMigration{
		Status: migrationv1alpha1.SwiftMigrationStatus{Mode: migrationv1alpha1.SwiftMigrationModeOffline},
	}
	res := r.handlePreparingLive(context.Background(), mig, &mig.Status)
	if !strings.Contains(res.FailureMsg, "without live mode") {
		t.Errorf("guard message: got %q", res.FailureMsg)
	}
}

func TestPreparingLive_TransientCreateFailure_RequeuesAndRetries(t *testing.T) {
	// First Create attempt fails with apiserver Conflict; reconcile
	// must retry-in-place via phaseTransient (controller-runtime backs
	// off and re-queues).
	scheme := testScheme(t)
	mig, guest, src := preparingLiveFixture(t, "uid-1")

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mig, guest, src).
		WithStatusSubresource(mig).
		Build()
	c := newSelectiveFailingClient(base)
	c.FailNext(typeKeyOf(&corev1.Pod{}), VerbCreate, 1,
		apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "guest-mig-abcdef", errors.New("simulated conflict")))

	r := &SwiftMigrationReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

	// First reconcile: Create fails → phaseTransient.
	status := mig.Status.DeepCopy()
	res := r.handlePreparingLive(context.Background(), mig, status)
	if res.Err == nil {
		t.Fatalf("expected phaseTransient error on first attempt; got %+v", res)
	}

	// Second reconcile: Create succeeds.
	res = r.handlePreparingLive(context.Background(), mig, status)
	if res.Err != nil || res.FailureMsg != "" {
		t.Fatalf("retry should succeed: err=%v msg=%q", res.Err, res.FailureMsg)
	}
}

// preExistingDstPod builds a dst pod fixture mimicking what
// newDstPod would have produced. Used by tests that simulate
// leader-handover (pod already exists at reconcile entry).
func preExistingDstPod(mig *migrationv1alpha1.SwiftMigration, guest *swiftv1alpha1.SwiftGuest, _ interface{}, t *testing.T) *corev1.Pod {
	t.Helper()
	tru := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "guest-mig-abcdef",
			Namespace: "default",
			Labels: map[string]string{
				LabelGuestName:     guest.Name,
				LabelMigrationRole: MigrationRoleDestination,
				LabelMigrationName: mig.Name,
			},
			Annotations: map[string]string{
				AnnotationMigrationPhase2Ack: AnnotationMigrationPhase2AckValue,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "swift.kubeswift.io/v1alpha1",
				Kind:       "SwiftGuest",
				Name:       guest.Name,
				UID:        guest.UID,
				Controller: &tru,
			}},
		},
		Spec: corev1.PodSpec{
			NodeName:      "worker-2",
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  LauncherContainerName,
				Image: "kubeswift/swiftletd:latest",
			}},
		},
	}
}
