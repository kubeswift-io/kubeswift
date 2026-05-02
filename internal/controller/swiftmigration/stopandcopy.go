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

// stopAndCopyPollInterval is the requeue cadence used while polling
// for the destination launcher pod's appearance after the combined
// patch lands. Matches the SwiftGuest controller's typical pod-
// recreation latency (~1-2s after spec change).
const stopAndCopyPollInterval = 2 * time.Second

// handleStopAndCopy implements the StopAndCopy phase per architect Q1.
//
// The phase has two responsibilities, both critical:
//
//  1. Single combined client.MergeFrom patch of spec.runPolicy=Running
//     AND spec.nodeName=target on the SwiftGuest. Atomicity matters:
//     if the patch is split, the SwiftGuest controller's reconciler
//     can race in between and recreate the pod with no NodeSelector
//     (since spec.nodeName would still be empty), landing the pod on
//     the wrong node.
//
//  2. Wait for the SwiftGuest controller to recreate the launcher pod
//     on the destination node. Once the pod is observed with
//     pod.Spec.NodeName == target, advance to Resuming.
//
// Idempotency: re-entry sees the SwiftGuest already patched and the
// patch becomes a no-op (client.MergeFrom against current state
// computes an empty diff). The pod-existence poll picks up where
// the prior reconcile left off.
//
// Drive-forward post-cutover (architect Risk 2): once the patch is
// issued, we never roll back to source. A controller crash mid-
// StopAndCopy resumes from re-entry, observes the patched SwiftGuest,
// and continues polling for the destination pod. If the destination
// fails to come up, commit 10's failure handler surfaces it as
// Failed without resuming source.
func (r *SwiftMigrationReconciler) handleStopAndCopy(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	// Phase 3a per-mode dispatch.
	if isLiveMode(mig) {
		return r.handleStopAndCopyLive(ctx, mig, status)
	}

	var guest swiftv1alpha1.SwiftGuest
	if getErr := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return phaseFailure(fmt.Sprintf("source SwiftGuest %q deleted during StopAndCopy", mig.Spec.GuestRef.Name), "")
		}
		return phaseTransient(fmt.Errorf("get source guest: %w", getErr))
	}

	// Defensive: the in-progress annotation should have been written
	// in Preparing. If absent, something went wrong with phase
	// ordering or the guest was reset; fail-fast rather than
	// continue.
	if guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] != mig.Name {
		return phaseFailure(fmt.Sprintf("SwiftGuest %q is missing the in-progress annotation for this migration; the phase ordering invariant was violated", guest.Name), "")
	}

	target := mig.Spec.Target.NodeName

	// Combined patch: runPolicy=Running AND nodeName=target. Single
	// MergeFrom produces one PATCH request → atomic at the API server.
	// If the spec already matches (re-entry after the patch landed),
	// the patch payload is empty and the API server short-circuits.
	if guest.Spec.RunPolicy != swiftv1alpha1.RunPolicyRunning || guest.Spec.NodeName != target {
		patch := client.MergeFrom(guest.DeepCopy())
		guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
		guest.Spec.NodeName = target
		if patchErr := r.Patch(ctx, &guest, patch); patchErr != nil {
			return phaseTransient(fmt.Errorf("combined patch runPolicy+nodeName: %w", patchErr))
		}
		setPhaseDetail(status, fmt.Sprintf("patched SwiftGuest to run on %q; awaiting destination pod", target))
		if r.Recorder != nil {
			r.Recorder.Event(mig, corev1.EventTypeNormal, "PodScheduling",
				fmt.Sprintf("patched SwiftGuest %q to run on %q (single-patch atomic)", guest.Name, target))
		}
	}

	// Poll for the destination launcher pod. The SwiftGuest controller
	// reacts to the spec change by creating a new pod with
	// pod.Spec.NodeName=target (commit 3's pod-builder integration).
	// The pod's existence is the signal that the cutover is complete
	// from this phase's perspective; the actual VM boot is the
	// Resuming phase's concern (commit 9).
	var pod corev1.Pod
	getErr := r.Get(ctx, client.ObjectKey{Name: guest.Name, Namespace: guest.Namespace}, &pod)
	if apierrors.IsNotFound(getErr) {
		setPhaseDetail(status, "awaiting destination pod creation")
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy, "awaiting destination pod creation")
		return phaseRequeue(stopAndCopyPollInterval)
	}
	if getErr != nil {
		return phaseTransient(fmt.Errorf("get destination pod: %w", getErr))
	}

	// Pod exists. Verify it's pinned to the destination node — if it
	// somehow landed elsewhere (race window we believe is closed but
	// belt-and-suspenders), surface as Failed.
	if pod.Spec.NodeName != target {
		return phaseFailure(fmt.Sprintf("destination pod %q scheduled on %q, expected %q (atomicity invariant violated)",
			pod.Name, pod.Spec.NodeName, target), "")
	}

	// Stamp the destination pod ref. Phase 1 source and destination
	// share the same pod name (Approach A: same SwiftGuest, same
	// derived pod name); the field exists for symmetry with Phase 3
	// where two launcher pods may run concurrently.
	status.DestinationPodRef = &migrationv1alpha1.SwiftMigrationPodRef{Name: pod.Name}

	// Advance to Resuming. The Resuming phase polls for the
	// GuestRunning condition + primaryIP discovery (commit 9).
	setPhase(status, migrationv1alpha1.SwiftMigrationPhaseResuming)
	setReadyCondition(status, metav1.ConditionFalse, ReasonResuming, "awaiting GuestRunning on destination")
	setPhaseDetail(status, "awaiting GuestRunning on destination (boot ~17s on warm cache)")
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, "PodScheduled",
			fmt.Sprintf("destination launcher pod %q scheduled on %q; awaiting boot", pod.Name, pod.Spec.NodeName))
	}
	return phaseAdvance()
}
