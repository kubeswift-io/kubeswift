package swiftmigration

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// cutoverStep enumerates which of the 3-step cutover sequence is
// pending at any given reconcile. **Each reconcile in the cutover
// handler executes ONLY the pending step**; the next reconcile reads
// cluster state again and proceeds. This pattern preserves the
// forward-only retry-in-place semantics from §2.3 cutover ordering
// invariant.
type cutoverStep int

const (
	// cutoverStep1Pending: SwiftGuest.status.podRef.name does not yet
	// equal dst-pod-name. Step 1's two patches (SwiftGuest podRef +
	// SwiftMigration cutoverStep1At) need to fire.
	cutoverStep1Pending cutoverStep = iota + 1
	// cutoverStep1TimestampOnly: SwiftGuest patch already succeeded
	// (podRef == dst) but cutoverStep1At timestamp wasn't written
	// (e.g., crash between the two patches). Step 1's timestamp
	// patch only.
	cutoverStep1TimestampOnly
	// cutoverStep2Pending: step 1 fully done; src pod still exists.
	// Step 2's Delete needs to fire.
	cutoverStep2Pending
	// cutoverStep3Pending: steps 1+2 done; SwiftMigration phase
	// still StopAndCopy. Step 3's phase patch needs to fire.
	cutoverStep3Pending
)

// String returns a human-readable label for diagnostic logging +
// test failure messages.
func (s cutoverStep) String() string {
	switch s {
	case cutoverStep1Pending:
		return "step1-podref-and-timestamp"
	case cutoverStep1TimestampOnly:
		return "step1-timestamp-only"
	case cutoverStep2Pending:
		return "step2-delete-src"
	case cutoverStep3Pending:
		return "step3-phase-patch"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// deriveCutoverStep determines the current cutover step from
// observable cluster state alone. Per §2.3 + §3.5 cutover ordering
// invariant: each reconcile reads cluster state, derives the pending
// step, and executes only that step. **No in-memory state survives
// reconcile.**
//
// The returned step is the work that needs to be done NEXT, not the
// work that has been done. Caller dispatches to the corresponding
// step handler.
//
// Inputs:
//   - mig: SwiftMigration with its status fields read from apiserver.
//     The handler reads status.CutoverStep1At to detect partial-step-1
//     completion (SwiftGuest patch succeeded but timestamp wasn't
//     written).
//   - guest: SwiftGuest with status.podRef.Name read fresh from
//     apiserver. This is the cluster-of-truth signal for step 1
//     completion.
//   - srcPodPresent: bool — whether the src pod was found at this
//     reconcile. nil-safe via boolean. Step 2's completion signal is
//     "src pod NotFound."
//   - dstPodName: the deterministic dst pod name (from B2.2's
//     dstPodName helper). The reference value for "podRef matches
//     dst."
func deriveCutoverStep(
	mig *migrationv1alpha1.SwiftMigration,
	guest *swiftv1alpha1.SwiftGuest,
	srcPodPresent bool,
	dstPodName string,
) cutoverStep {
	step1PodRefDone := guest.Status.PodRef != nil && guest.Status.PodRef.Name == dstPodName

	if !step1PodRefDone {
		return cutoverStep1Pending
	}

	// Step 1's SwiftGuest patch succeeded; check if the timestamp
	// patch also succeeded.
	if mig.Status.CutoverStep1At == nil {
		return cutoverStep1TimestampOnly
	}

	if srcPodPresent {
		return cutoverStep2Pending
	}

	// Steps 1+2 done; phase patch is the only remaining step. The
	// caller (handleStopAndCopyLive) only enters this code path when
	// phase is still StopAndCopy, so we don't double-check.
	return cutoverStep3Pending
}

// executeCutover dispatches the per-step work and returns a
// phaseResult for the StopAndCopy live handler to surface. Each step
// is one reconcile's work; the function does NOT loop through all
// steps within a single invocation. Per §2.3 implementation note:
// "the three steps must be issued from a single reconcile invocation,
// in this exact order, with each step's success-check before the
// next" — but in practice "single reconcile invocation" applies to
// the whole 3-step sequence as observed across reconciles, not as a
// single function call. The forward-only retry-in-place property
// requires per-reconcile state derivation.
//
// **Cutover ordering invariant** is enforced by deriveCutoverStep:
// each reconcile reads cluster state and dispatches to exactly one
// step. Out-of-order execution is impossible because each step's
// pending-check is "the previous step's effect is observable in
// cluster state."
//
// **Step 1 is two writes on different resources**:
//   - SwiftGuest.status.podRef.name = dst-pod-name (load-bearing —
//     this is the cutover commit point per §3.5)
//   - SwiftMigration.status.cutoverStep1At = now() (audit timestamp)
//
// These cannot be combined into a single API call (different
// resources, different status subresources). They are issued
// sequentially with the SwiftGuest patch first; if the timestamp
// patch fails, deriveCutoverStep on the next reconcile returns
// cutoverStep1TimestampOnly and only the timestamp is retried.
//
// **Step 2 is asynchronous**: Delete returns when the apiserver has
// accepted the deletion request, not when the pod has actually
// terminated. We do NOT wait for actual termination. The next
// reconcile's "step 2 done" signal is "src pod returns NotFound."
// If the next reconcile fires before kubelet has finished
// termination, src pod still exists and step 2 is re-attempted —
// the apiserver no-ops the second Delete (already-deleting-pod is
// idempotent on the apimachinery side).
//
// **Step 3 trivially succeeds on retry** if the phase patch fails
// transiently.
func (r *SwiftMigrationReconciler) executeCutover(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
	guest *swiftv1alpha1.SwiftGuest,
	srcPod *corev1.Pod, // nil if NotFound
	dstName string,
) *phaseResult {
	// Cutover step 2 looks up src pod BY guest.Name (the original
	// pre-cutover pod name), NOT via canonicalPodName — which post-
	// step-1 resolves to dst pod name and would mis-detect "src
	// exists." Re-fetch src pod by guest.Name here; ignore the
	// caller's srcPod argument (which may be the dst pod due to
	// canonicalPodName's post-step-1 indirection).
	var srcByName corev1.Pod
	_ = srcPod // suppress unused-param hint; caller passes for API symmetry
	srcExistsByName := true
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Name, Namespace: guest.Namespace}, &srcByName); err != nil {
		if apierrors.IsNotFound(err) {
			srcExistsByName = false
		} else {
			return phaseTransient(fmt.Errorf("get src pod by guest.Name during cutover: %w", err))
		}
	}
	step := deriveCutoverStep(mig, guest, srcExistsByName, dstName)

	switch step {
	case cutoverStep1Pending:
		return r.cutoverStep1(ctx, mig, status, guest, dstName)
	case cutoverStep1TimestampOnly:
		return r.cutoverStep1Timestamp(ctx, status)
	case cutoverStep2Pending:
		// Use the by-name src pod, not the caller's srcPod argument
		// (see srcByName fetch above for rationale).
		return r.cutoverStep2(ctx, status, &srcByName)
	case cutoverStep3Pending:
		return r.cutoverStep3(status)
	default:
		// Unreachable: deriveCutoverStep returns one of the four
		// known values. Default-to-explicit per PR #26.
		return phaseFailure(
			fmt.Sprintf("internal: unhandled cutover step %q", step),
			migrationv1alpha1.FailureReasonOther)
	}
}

// cutoverStep1 issues the SwiftGuest podRef.name patch and the
// SwiftMigration cutoverStep1At timestamp patch. Two patches on
// different resources; both via status subresource.
//
// Order: SwiftGuest first (cutover commit point); SwiftMigration
// timestamp second. If the SwiftGuest patch fails, the timestamp
// is not attempted — next reconcile re-derives cutoverStep1Pending
// and retries both. If SwiftGuest succeeds and the timestamp fails,
// next reconcile re-derives cutoverStep1TimestampOnly and retries
// only the timestamp.
//
// **Idempotency on SwiftGuest patch**: the patch sets podRef.name
// to dst-pod-name. If already set (re-entry after partial completion
// where SwiftGuest was patched but cutoverStep1At wasn't), the
// MergeFrom diff is empty and the apiserver short-circuits.
// deriveCutoverStep skips us to cutoverStep1TimestampOnly in that
// case, but if a future code change accidentally re-routes here, the
// SwiftGuest patch is still safe.
func (r *SwiftMigrationReconciler) cutoverStep1(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
	guest *swiftv1alpha1.SwiftGuest,
	dstName string,
) *phaseResult {
	setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveCutoverPodRef)

	// Step 1a: patch SwiftGuest.status.podRef.name = dst-pod-name.
	guestPatch := client.MergeFrom(guest.DeepCopy())
	if guest.Status.PodRef == nil {
		guest.Status.PodRef = &corev1.ObjectReference{}
	}
	guest.Status.PodRef.Name = dstName
	guest.Status.PodRef.Namespace = guest.Namespace
	if err := r.Status().Patch(ctx, guest, guestPatch); err != nil {
		return phaseTransient(fmt.Errorf("cutover step 1a (SwiftGuest podRef patch): %w", err))
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(mig, corev1.EventTypeNormal, "CutoverStep1",
			"patched SwiftGuest %q status.podRef.name = %q", guest.Name, dstName)
	}

	// Step 1b: stamp cutoverStep1At on SwiftMigration. Tail-call into
	// the timestamp-only path; partial-completion recovery treats
	// this identically.
	return r.cutoverStep1Timestamp(ctx, status)
}

// cutoverStep1Timestamp stamps status.CutoverStep1At = now() AND
// writes the PodRefSwapped=True condition. Called from cutoverStep1
// (after SwiftGuest patch succeeded) and from the dispatcher when
// deriveCutoverStep returns cutoverStep1TimestampOnly (recovery from
// leader-handover-between-the-two-patches).
//
// The status update happens via the in-memory `status` pointer the
// dispatchResult will persist. We don't issue a separate Patch here
// because dispatchResult's persist() handles the SwiftMigration
// status diff at the end of every reconcile invocation. This means
// a transient-fail-on-persist will leave the timestamp + condition
// unset and the next reconcile re-runs cutoverStep1TimestampOnly
// cleanly (idempotent: `if CutoverStep1At == nil` short-circuits the
// timestamp write; setCondition's same-value short-circuit handles
// the condition).
//
// W21 (PR #46 cluster walkthrough): the PodRefSwapped condition is
// the safety gate against data-loss in cancel-post-cutover (cancel
// during the narrow Resuming window where the dst pod IS the live
// migrated guest; without the gate, transitionCancelLive would
// destroy the just-migrated guest). The architect's Q3.3(c)
// decision was that PodRefSwapped is DERIVED from cluster state
// (guest.Status.PodRef.Name == dstName) on every cutover-substate
// reconcile — derivation remains primary for state-machine logic
// (the cutover-in-progress short-circuit in stopandcopy_live reads
// directly from guest.status.podRef). The explicit write here is
// ADDITIONAL DEFENSE so that downstream call-sites that read the
// SwiftMigration's Conditions list (isPostCutover, honorCancel,
// shouldCheckSourcePodUID) get a consistent signal even when the
// SwiftGuest informer cache lags by a reconcile. **Do not remove
// the derivation; do not remove this explicit write — both are
// load-bearing.**
func (r *SwiftMigrationReconciler) cutoverStep1Timestamp(
	ctx context.Context,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if status.CutoverStep1At == nil {
		now := metav1.Now()
		status.CutoverStep1At = &now
	}
	setCondition(status,
		migrationv1alpha1.SwiftMigrationConditionPodRefSwapped,
		metav1.ConditionTrue,
		migrationv1alpha1.ReasonCutoverStep1Complete,
		"cutover step 1 complete: SwiftGuest.status.podRef.name patched to destination pod")
	setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveCutoverDeleteSrc)
	// Requeue so the next reconcile observes step 1 done and
	// proceeds to step 2. Short interval — no external state to
	// wait for.
	return phaseRequeue(stopAndCopyLivePollInterval)
}

// cutoverStep2 issues Delete on the src pod. NotFound is treated as
// success per the standard apimachinery pattern (previous reconcile's
// Delete may have succeeded with the controller crashing before
// observing). Any other error is transient and triggers retry-in-
// place.
//
// **Delete semantics**: returns when the apiserver has accepted the
// deletion request. The pod's actual termination happens via kubelet
// asynchronously. We do NOT wait for actual termination; the next
// reconcile's "step 2 done" signal is "src pod NotFound." If kubelet
// is slow, the next reconcile may still see src pod existing and
// re-attempt Delete — apiserver no-ops the second Delete (idempotent).
func (r *SwiftMigrationReconciler) cutoverStep2(
	ctx context.Context,
	status *migrationv1alpha1.SwiftMigrationStatus,
	srcPod *corev1.Pod,
) *phaseResult {
	setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveCutoverDeleteSrc)

	if srcPod == nil {
		// deriveCutoverStep wouldn't return cutoverStep2Pending if
		// srcPod is nil, but defensive: treat as already-done.
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveCutoverCompleting)
		return phaseRequeue(stopAndCopyLivePollInterval)
	}

	if err := r.Delete(ctx, srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			// Already gone — previous reconcile's Delete succeeded
			// but controller crashed before observing. Treat as
			// success.
			setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveCutoverCompleting)
			return phaseRequeue(stopAndCopyLivePollInterval)
		}
		return phaseTransient(fmt.Errorf("cutover step 2 (Delete src pod): %w", err))
	}

	setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveCutoverCompleting)
	return phaseRequeue(stopAndCopyLivePollInterval)
}

// cutoverStep3 transitions the SwiftMigration phase from StopAndCopy
// to Resuming. Resuming-live (B2.3) takes over from there.
//
// No explicit API call here: the phase transition is recorded in the
// in-memory `status` pointer and persisted by dispatchResult.persist()
// at the end of the reconcile. phaseAdvance() returns the
// transition-immediate-requeue signal.
func (r *SwiftMigrationReconciler) cutoverStep3(
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	setPhase(status, migrationv1alpha1.SwiftMigrationPhaseResuming)
	setReadyCondition(status, metav1.ConditionFalse, ReasonResuming,
		"cutover complete; awaiting destination guest health")
	setPhaseDetail(status, phaseDetailLiveResumingWaiting)
	return phaseAdvance()
}
