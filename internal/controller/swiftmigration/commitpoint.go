package swiftmigration

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

// The commit point of a LIVE migration.
//
// The point of no return is NOT cutover step 1 (the PodRefSwapped condition).
// It is earlier: the moment the source launcher writes
// migration-status=complete with this migration's $SEND_ID. swiftletd-on-src
// writes that only after its vm.send-migration probed the destination CH for
// vm_info=Running AND its own Cloud Hypervisor has exited, so from that instant
// the DESTINATION holds the only running copy of the VM
// (rust/swiftletd/src/action.rs). Cutover step 1 then stamps PodRefSwapped a
// few reconciles later, leaving a window in which the source is already gone but
// isPostCutover() is still false.
//
// In that window a cancel, a spec.timeout expiry, or a CR deletion that deletes
// the destination pod destroys the only running copy of the guest. So every
// such handler must treat "source reported complete" as committed, exactly like
// post-cutover: drive the migration FORWARD to completion, never tear the
// destination down.

// srcReportedComplete reports whether the source launcher pod carries
// migration-status=complete with this migration's $SEND_ID — the commit point.
// A nil src pod is not complete.
func srcReportedComplete(mig *migrationv1alpha1.SwiftMigration, src *corev1.Pod) bool {
	return podStatusMatches(src, migrationStatusComplete, sendActionID(mig))
}

// liveCommitted reports whether a live migration has passed the commit point:
// cutover has stamped PodRefSwapped, or the source has reported complete. It is
// for handlers (cancel, deletion) that run before the StopAndCopy handler has
// loaded the pods and so must look the source pod up themselves.
//
// Determining this requires reading the source pod. A transient read error is
// returned to the caller rather than guessed: treating an unknown state as
// "not committed" could delete a destination that in fact holds the only
// running copy, so a caller that would tear the destination down must requeue
// on error instead.
func (r *SwiftMigrationReconciler) liveCommitted(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) (bool, error) {
	if isPostCutover(mig) {
		return true, nil
	}
	if mig.Status.Mode != migrationv1alpha1.SwiftMigrationModeLive {
		return false, nil
	}
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, &guest), Namespace: guest.Namespace}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return srcReportedComplete(mig, &srcPod), nil
}

// deletionCommitted reports whether a migration being deleted has passed its
// commit point, mode-aware. For live it is liveCommitted (source reported
// complete, or PodRefSwapped). For offline it is the guest.spec.nodeName patch
// that cutover applies (mirrors onTerminalPhase's offline check). Before the
// commit point a deletion is an abort; after it, the destination holds the only
// running copy and must be preserved.
func (r *SwiftMigrationReconciler) deletionCommitted(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) (bool, error) {
	if mig.Status.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		return r.liveCommitted(ctx, mig)
	}
	// Offline: cutover patches guest.spec.nodeName to the destination node.
	if mig.Status.DestinationNode == "" {
		return false, nil
	}
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return guest.Spec.NodeName == mig.Status.DestinationNode, nil
}

// timeoutExceeded reports whether spec.timeout has run out since StartedAt and
// the timeout strategy says to act on it. timeoutStrategy: ignore turns the
// backstop off (it used to be accepted and never read).
func timeoutExceeded(mig *migrationv1alpha1.SwiftMigration, status *migrationv1alpha1.SwiftMigrationStatus) bool {
	if mig.Spec.TimeoutStrategy == migrationv1alpha1.SwiftMigrationTimeoutStrategyIgnore {
		return false
	}
	if mig.Spec.Timeout == nil || mig.Spec.Timeout.Duration <= 0 || status.StartedAt == nil {
		return false
	}
	return time.Since(status.StartedAt.Time) > mig.Spec.Timeout.Duration
}

// timeoutFailure is the terminal result for an expired spec.timeout.
func timeoutFailure(mig *migrationv1alpha1.SwiftMigration) *phaseResult {
	return phaseFailure(
		fmt.Sprintf("spec.timeout=%s exceeded since StartedAt; migration did not complete in time", mig.Spec.Timeout.Duration),
		migrationv1alpha1.FailureReasonTimeout)
}
