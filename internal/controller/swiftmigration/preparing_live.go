package swiftmigration

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// preparingLiveReadyBudget is the wall-clock window for the dst pod
// to reach Ready before we transition the migration to Failed with
// FailureReason=PodTerminated. Per design §2.3 Preparing actions
// step 5: "Up to ~60s budget; if not Ready in 60s, transition to
// Failed".
const preparingLiveReadyBudget = 60 * time.Second

// preparingLivePollInterval is the requeue cadence while waiting for
// the dst pod to reach Ready. The labeled informer (§5.1) wakes the
// controller on Ready transitions much faster than this; the periodic
// requeue is the safety net for missed events and the budget-
// exceeded check.
const preparingLivePollInterval = 2 * time.Second

// Phase 3a phaseDetail vocabulary additions for Preparing-live. Per
// §2.3 sub-states: "creating destination pod" while Create is in
// flight, "waiting for destination pod ready" while polling.
const (
	phaseDetailLivePreparingCreating = "creating destination pod"
	phaseDetailLivePreparingWaiting  = "waiting for destination pod ready"
)

// handlePreparingLive implements the live-mode Preparing phase per
// design §2.3 and §3.2.
//
// **Responsibilities** (B2.2 scope):
//
//  1. Re-resolve source SwiftGuest (defense in depth — Validating
//     touched it, but a mutation between then and now is possible).
//  2. F4.2 source-pod-replacement detection: shouldCheckSourcePodUID
//     fires here (pre-cutover phase). UID mismatch → Failed with
//     FailureReason=SourcePodReplaced.
//  3. Stamp status.PreparingStartedAt on first Preparing reconcile
//     (anchor for the 60s budget).
//  4. Look up the deterministic dst pod by name:
//     - exists + correct shape → skip Create, proceed to readiness check
//     - exists + wrong shape → Failed (collision; see §3.2)
//     - does not exist → construct via newDstPod and Create
//  5. Check dst pod readiness:
//     - Ready → advance to StopAndCopy
//     - Not Ready + within 60s budget → phaseRequeue
//     - Not Ready + budget exceeded → Failed with PodTerminated
//
// **Idempotency**: dst pod name is deterministic from
// SwiftMigration.UID. Re-entry on leader handover observes the
// existing pod and skips Create (per §3.2 idempotency note + spike
// finding G).
//
// **Defensive guard**: assert isLiveMode at entry. Architect-discipline
// review answer to Q1.
func (r *SwiftMigrationReconciler) handlePreparingLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig, status) {
		return phaseFailure(
			"internal: handlePreparingLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}

	// Re-resolve source guest (defense in depth).
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source SwiftGuest %q deleted during Preparing", mig.Spec.GuestRef.Name),
				migrationv1alpha1.FailureReasonOther)
		}
		return phaseTransient(fmt.Errorf("get source guest: %w", err))
	}

	// F4.2: source-pod-replacement detection. shouldCheckSourcePodUID
	// returns true here (Preparing is a pre-cutover phase per the
	// triple-gate). Compare the current src pod UID against the value
	// stamped by Validating-live; mismatch indicates the src pod was
	// K8s-terminated and recreated by an external actor (drain,
	// eviction, etc), which fails the migration.
	if shouldCheckSourcePodUID(mig) && status.SourcePodUID != "" {
		var srcPod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, &guest), Namespace: guest.Namespace}, &srcPod)
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source pod for SwiftGuest %q no longer exists during Preparing", guest.Name),
				migrationv1alpha1.FailureReasonSourcePodReplaced)
		}
		if err != nil {
			return phaseTransient(fmt.Errorf("get source pod for UID check: %w", err))
		}
		if srcPod.UID != status.SourcePodUID {
			return phaseFailure(
				fmt.Sprintf("source pod for SwiftGuest %q was replaced (UID changed from %q to %q)", guest.Name, status.SourcePodUID, srcPod.UID),
				migrationv1alpha1.FailureReasonSourcePodReplaced)
		}
	}

	// Stamp PreparingStartedAt on first Preparing reconcile.
	if status.PreparingStartedAt == nil {
		now := metav1.Now()
		status.PreparingStartedAt = &now
	}

	// Resolve src pod for the cloning step in newDstPod. (We just
	// fetched it for the UID check, but re-fetching is cheaper than
	// threading it through; this code path runs once per reconcile.)
	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, &guest), Namespace: guest.Namespace}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source pod for SwiftGuest %q no longer exists; cannot template dst pod", guest.Name),
				migrationv1alpha1.FailureReasonSourcePodReplaced)
		}
		return phaseTransient(fmt.Errorf("get source pod: %w", err))
	}

	// Compute deterministic dst pod name and look up any existing pod
	// with that name.
	expectedDstName, err := dstPodName(mig, guest.Name)
	if err != nil {
		return phaseFailure(err.Error(), migrationv1alpha1.FailureReasonOther)
	}

	var existingDst corev1.Pod
	getErr := r.Get(ctx, client.ObjectKey{Name: expectedDstName, Namespace: guest.Namespace}, &existingDst)
	switch {
	case apierrors.IsNotFound(getErr):
		// No dst pod yet — construct and Create.
		//
		// Phase 3c: when mTLS is enabled, newDstPod injects the dst
		// stunnel sidecar. srcNodeName is the node the source guest runs
		// on right now (its identity is what the dst pins via CHECK_HOST);
		// dstNodeName is the migration target (whose identity Secret the
		// dst sidecar presents). Both are guaranteed non-empty here:
		// Validating-live verified the guest is scheduled and the target
		// node exists, and (when mTLS is on) distributed both identity
		// Secrets into this namespace.
		dst, buildErr := newDstPod(mig, &guest, &srcPod, r.Scheme, dstSidecarConfig{
			mtlsEnabled: r.MigrationMTLSEnabled,
			srcNodeName: guest.Status.NodeName,
			dstNodeName: mig.Spec.Target.NodeName,
		})
		if buildErr != nil {
			return phaseFailure(
				fmt.Sprintf("construct dst pod: %v", buildErr),
				migrationv1alpha1.FailureReasonOther)
		}
		setPhaseDetail(status, phaseDetailLivePreparingCreating)
		if createErr := r.Create(ctx, dst); createErr != nil {
			// AlreadyExists: race between our Get above and another
			// reconcile creating the pod. Treat as success — the next
			// reconcile observes the existing pod.
			if apierrors.IsAlreadyExists(createErr) {
				return phaseRequeue(preparingLivePollInterval)
			}
			return phaseTransient(fmt.Errorf("create dst pod: %w", createErr))
		}
		// Update status.DestinationPodRef so operators see the dst pod
		// name in `kubectl get smig`.
		status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: dst.Name}
		setReadyCondition(status, metav1.ConditionFalse, ReasonPreparing, phaseDetailLivePreparingWaiting)
		setPhaseDetail(status, phaseDetailLivePreparingWaiting)
		if r.Recorder != nil {
			r.Recorder.Event(mig, corev1.EventTypeNormal, "DestinationPodCreated",
				fmt.Sprintf("created destination pod %q on node %q", dst.Name, mig.Spec.Target.NodeName))
		}
		return phaseRequeue(preparingLivePollInterval)

	case getErr != nil:
		return phaseTransient(fmt.Errorf("get dst pod: %w", getErr))

	default:
		// Pod exists. Verify shape matches our expectations.
		if !dstPodMatches(&existingDst, mig, &guest) {
			return phaseFailure(
				fmt.Sprintf("destination pod %q exists but does not match expected ownership/labels (possible name collision)", existingDst.Name),
				migrationv1alpha1.FailureReasonDstPodConflict)
		}
		// Persist DestinationPodRef on re-entry too (status may have
		// been lost across leader handover or status patches).
		if status.DestinationPodRef == nil || status.DestinationPodRef.Name != existingDst.Name {
			status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: existingDst.Name}
		}
	}

	// Pod exists with correct shape (either we just created it on a
	// prior reconcile, or another leader did). Check readiness.
	if dstPodReady(&existingDst) {
		setPhase(status, migrationv1alpha1.SwiftMigrationPhaseStopAndCopy)
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy,
			"destination pod ready; starting state copy")
		setPhaseDetail(status, "destination pod ready; advancing to StopAndCopy")
		if r.Recorder != nil {
			r.Recorder.Event(mig, corev1.EventTypeNormal, "DestinationPodReady",
				fmt.Sprintf("destination pod %q reached Ready; advancing to StopAndCopy", existingDst.Name))
		}
		return phaseAdvance()
	}

	// Not Ready yet — check 60s budget.
	if status.PreparingStartedAt != nil &&
		time.Since(status.PreparingStartedAt.Time) > preparingLiveReadyBudget {
		return phaseFailure(
			fmt.Sprintf("destination pod %q never reached Ready within %s budget", existingDst.Name, preparingLiveReadyBudget),
			migrationv1alpha1.FailureReasonDstNeverReady)
	}

	// Within budget; surface waiting state and requeue.
	setReadyCondition(status, metav1.ConditionFalse, ReasonPreparing, phaseDetailLivePreparingWaiting)
	setPhaseDetail(status, phaseDetailLivePreparingWaiting)
	return phaseRequeue(preparingLivePollInterval)
}
