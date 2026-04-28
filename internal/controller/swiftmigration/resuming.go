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

// resumingPollInterval is the requeue cadence while waiting for
// GuestRunning=True. The watch on SwiftGuest in SetupWithManager
// (commit 5) wakes the controller on actual condition transitions;
// the periodic requeue is a safety net for missed events.
const resumingPollInterval = 5 * time.Second

// guestRunningConditionType matches the SwiftGuest controller's
// constant. Duplicated here (string literal) to avoid importing the
// SwiftGuest controller package.
const guestRunningConditionType = "GuestRunning"

// handleResuming implements the Resuming + Completed transition.
//
// The guest's launcher pod has been recreated on the destination node
// by the SwiftGuest controller (commit 8 ended with the pod scheduled).
// This phase waits for the VM to actually boot — observed via the
// GuestRunning=True condition on the SwiftGuest, set by swiftletd
// once Cloud Hypervisor reports the VM is running and dnsmasq has
// dispensed an IP.
//
// The boot wait dominates Phase 1 downtime on RWO storage when the
// detach is fast (CoW drivers): the VM cold-boot from the existing
// disk takes ~17s on a warm cache. On Longhorn full-copy the detach
// already absorbed ~45s before this phase started, so the boot adds
// to that. operator-facing docs flag this as expected, not "stuck".
//
// Idempotency: re-entry just polls for GuestRunning=True. The
// annotation is still set on the source guest; we clear it on the
// transition to Completed.
func (r *SwiftMigrationReconciler) handleResuming(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) (advanced bool, requeue time.Duration, errMsg string, err error) {
	var guest swiftv1alpha1.SwiftGuest
	if getErr := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); getErr != nil {
		if apierrors.IsNotFound(getErr) {
			return false, 0, fmt.Sprintf("source SwiftGuest %q deleted during Resuming", mig.Spec.GuestRef.Name), nil
		}
		return false, 0, "", fmt.Errorf("get source guest: %w", getErr)
	}

	// Poll for GuestRunning=True. Observe + primaryIP also populated
	// — operators expect the IP to show up at completion in
	// `kubectl get swiftmigration -o wide`.
	running := false
	for _, c := range guest.Status.Conditions {
		if c.Type == guestRunningConditionType && c.Status == metav1.ConditionTrue {
			running = true
			break
		}
	}
	hasIP := guest.Status.Network != nil && guest.Status.Network.PrimaryIP != ""

	if !running {
		setPhaseDetail(status, "awaiting GuestRunning=True on destination")
		setReadyCondition(status, metav1.ConditionFalse, ReasonResuming, "awaiting GuestRunning on destination (boot ~17s on warm cache)")
		return false, resumingPollInterval, "", nil
	}
	if !hasIP {
		// GuestRunning is True but the primaryIP hasn't been reported
		// yet — wait. swiftletd's lease poller runs after CH starts;
		// there's a small window between condition flip and IP
		// reporting. Operators reading the SwiftMigration during
		// this window should see "awaiting IP" rather than the
		// (already-stale) "awaiting boot".
		setPhaseDetail(status, "awaiting primaryIP discovery on destination")
		setReadyCondition(status, metav1.ConditionFalse, ReasonResuming, "awaiting primaryIP discovery")
		return false, resumingPollInterval, "", nil
	}

	// Cutover complete. Clear the in-progress annotation on the
	// source guest, stamp completion timestamps, set Ready=True.
	if guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] == mig.Name {
		patch := client.MergeFrom(guest.DeepCopy())
		delete(guest.Annotations, migrationv1alpha1.AnnotationMigrationInProgress)
		if patchErr := r.Patch(ctx, &guest, patch); patchErr != nil {
			// Annotation clear failed — not fatal, the migration
			// completed successfully. Surface as a warning event
			// and proceed; the annotation is stale but doesn't
			// block another migration (the conflict check would
			// fire on a stale value, but operators can clear the
			// annotation manually). Phase 1 acceptable.
			if r.Recorder != nil {
				r.Recorder.Eventf(mig, corev1.EventTypeWarning, "AnnotationCleanupFailed",
					"failed to clear migration-in-progress annotation on SwiftGuest %q: %v", guest.Name, patchErr)
			}
		}
	}

	now := metav1.Now()
	status.CompletedAt = &now
	if status.StartedAt != nil {
		// Phase 1 approximation: observedDowntime = CompletedAt -
		// StartedAt. The accurate measure (Preparing entry to
		// GuestRunning) requires per-phase timestamps the status
		// schema doesn't yet carry. Validating completes in <1s for
		// Phase 1 so the difference is negligible. Phase 3 can
		// refine if more precision is needed.
		downtime := metav1.Duration{Duration: now.Sub(status.StartedAt.Time)}
		status.ObservedDowntime = &downtime
	}
	setPhase(status, migrationv1alpha1.SwiftMigrationPhaseCompleted)
	setReadyCondition(status, metav1.ConditionTrue, ReasonCompleted,
		fmt.Sprintf("migration to %q complete; guest running with IP %s", status.DestinationNode, guest.Status.Network.PrimaryIP))
	setPhaseDetail(status, fmt.Sprintf("guest running on %q with IP %s", status.DestinationNode, guest.Status.Network.PrimaryIP))
	if r.Recorder != nil {
		r.Recorder.Event(mig, corev1.EventTypeNormal, ReasonCompleted,
			fmt.Sprintf("migration completed: SwiftGuest %q running on %q with IP %s",
				guest.Name, status.DestinationNode, guest.Status.Network.PrimaryIP))
	}
	return true, 0, "", nil
}
