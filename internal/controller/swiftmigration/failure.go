package swiftmigration

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// FinalizerName is added to SwiftMigration resources at first reconcile
// and removed after cleanup completes. Required so a `kubectl delete
// swiftmigration` mid-flight gives the controller a chance to clear
// the in-progress annotation on the source SwiftGuest and (when
// pre-cutover) restore runPolicy=Running so the source guest resumes.
const FinalizerName = "migration.kubeswift.io/cleanup"

// ensureFinalizer adds FinalizerName to the SwiftMigration if absent.
// Called from the main Reconcile path on non-terminal phases; idempotent.
func (r *SwiftMigrationReconciler) ensureFinalizer(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) error {
	if hasFinalizer(mig) {
		return nil
	}
	patch := client.MergeFrom(mig.DeepCopy())
	mig.Finalizers = append(mig.Finalizers, FinalizerName)
	return r.Patch(ctx, mig, patch)
}

// removeFinalizer removes FinalizerName from the SwiftMigration. Called
// after cleanup completes (cancellation path) or on terminal-phase
// transition (Completed/Failed). Idempotent.
func (r *SwiftMigrationReconciler) removeFinalizer(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) error {
	idx := -1
	for i, f := range mig.Finalizers {
		if f == FinalizerName {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	patch := client.MergeFrom(mig.DeepCopy())
	mig.Finalizers = append(mig.Finalizers[:idx], mig.Finalizers[idx+1:]...)
	return r.Patch(ctx, mig, patch)
}

// handleCancellation processes deletion of an in-flight SwiftMigration.
// Implements the architect's Risk 2 split: pre-cutover failures
// restore the source guest; post-cutover drives forward (the cutover
// is committed once Delete(pod) ran in Preparing).
//
// Pre-cutover (Validating, Preparing-before-Delete): annotation may
// or may not be set; runPolicy may or may not be Stopped. Restore
// runPolicy=Running and clear the annotation if it matches our name.
// Source guest resumes naturally via the SwiftGuest controller's
// reconcile.
//
// Post-cutover (StopAndCopy onwards): the source pod has been deleted
// and the SwiftGuest has spec.nodeName=target. We do NOT roll back
// the spec.nodeName patch — the guest is in flight to the destination
// and the runPolicy=Running patch (commit 8) means the SwiftGuest
// controller will create the destination pod. Cancellation in this
// phase only clears the annotation; the migration "completes" as
// far as the SwiftGuest is concerned (just on the destination).
//
// The cleanup runs once. The finalizer is removed after, allowing
// the SwiftMigration deletion to proceed.
func (r *SwiftMigrationReconciler) handleCancellation(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
) (ctrl.Result, error) {
	// Idempotent: if our finalizer is already gone, nothing to do.
	if !hasFinalizer(mig) {
		return ctrl.Result{}, nil
	}

	// Decide pre-cutover vs post-cutover by examining the
	// SwiftMigration's phase. Preparing and earlier are pre-cutover;
	// StopAndCopy and later are post-cutover.
	postCutover := false
	switch mig.Status.Phase {
	case migrationv1alpha1.SwiftMigrationPhaseStopAndCopy,
		migrationv1alpha1.SwiftMigrationPhaseResuming,
		migrationv1alpha1.SwiftMigrationPhaseCompleted:
		postCutover = true
	}

	if err := r.cleanupSourceGuest(ctx, mig, !postCutover); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup source guest on cancellation: %w", err)
	}

	if r.Recorder != nil {
		reason := ReasonCancelled
		msg := "migration cancelled; source guest cleanup complete"
		if postCutover {
			msg = "migration cancelled post-cutover; destination guest continues running"
		}
		r.Recorder.Event(mig, corev1.EventTypeNormal, reason, msg)
	}

	if err := r.removeFinalizer(ctx, mig); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// cleanupSourceGuest clears the in-progress annotation on the source
// SwiftGuest. When restoreRunPolicy=true (pre-cutover failure), also
// patches runPolicy=Running so the SwiftGuest controller resurrects
// the source pod on its previous node.
//
// Idempotent: if the annotation is absent or names a different
// migration, this is a no-op (we don't touch state we didn't write).
func (r *SwiftMigrationReconciler) cleanupSourceGuest(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	restoreRunPolicy bool,
) error {
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			// Source guest gone — nothing to clean up.
			return nil
		}
		return err
	}

	// Only touch the SwiftGuest if our annotation is on it. The
	// conflict-detection guard in Preparing means a different
	// migration's marker should never appear here, but defense in
	// depth: if it does, leave it alone.
	if guest.Annotations[migrationv1alpha1.AnnotationMigrationInProgress] != mig.Name {
		return nil
	}

	patch := client.MergeFrom(guest.DeepCopy())
	delete(guest.Annotations, migrationv1alpha1.AnnotationMigrationInProgress)
	if restoreRunPolicy && guest.Spec.RunPolicy == swiftv1alpha1.RunPolicyStopped {
		guest.Spec.RunPolicy = swiftv1alpha1.RunPolicyRunning
	}
	if err := r.Patch(ctx, &guest, patch); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// onTerminalPhase performs cleanup when the SwiftMigration transitions
// to Failed (the dispatchResult path) so the source guest doesn't get
// left annotated and stopped, AND so a pre-cutover-created dst pod
// (live mode) doesn't leak (W17).
//
// Mirrors handleCancellation's pre/post-cutover split. The Completed
// phase already clears the annotation in Resuming; this helper covers
// the Failed case across all phases.
//
// Pre/post-cutover detection is mode-aware:
//   - offline: SwiftGuest.spec.nodeName patched at cutover, so check
//     guest.Spec.NodeName == status.DestinationNode (existing
//     heuristic, unchanged).
//   - live (Phase 3a): cutover patches guest.STATUS.podRef.name and
//     writes the PodRefSwapped condition (W21). Use isPostCutover()
//     which reads the condition — same gate as honorCancel and
//     shouldCheckSourcePodUID for consistency.
//
// W17 (PR #46 Scenario 3): on pre-cutover Failed in LIVE mode, also
// delete the destination pod the controller created in Preparing-live.
// Without this, the dst pod leaks (resource consumption + UX confusion).
// Best-effort retry-in-place: a transient Delete failure surfaces as
// the function's error return, which triggers controller-runtime
// reconcile retry (controller.go:299 propagates the error).
//
// Idempotent: cleanupSourceGuest skips when the annotation isn't ours;
// dst pod Delete returns NotFound on second attempt (success).
func (r *SwiftMigrationReconciler) onTerminalPhase(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) error {
	if status.Phase != migrationv1alpha1.SwiftMigrationPhaseFailed {
		return nil
	}

	postCutover := false
	if status.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		// Live mode: PodRefSwapped condition is the authoritative
		// post-cutover signal (post-W21 it's always written at
		// cutoverStep1).
		postCutover = isPostCutover(mig)
	} else {
		// Offline mode: check guest.Spec.NodeName patch.
		var guest swiftv1alpha1.SwiftGuest
		if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if guest.Spec.NodeName == status.DestinationNode && status.DestinationNode != "" {
			postCutover = true
		}
	}

	if err := r.cleanupSourceGuest(ctx, mig, !postCutover); err != nil {
		return err
	}

	// W17: pre-cutover Failed in live mode → delete the dst pod that
	// Preparing-live created. Post-cutover Failed leaves the dst pod
	// in place because it IS the canonical guest at that point
	// (the migration crossed the cutover commit point).
	if !postCutover && status.Mode == migrationv1alpha1.SwiftMigrationModeLive {
		if err := r.cleanupDstPod(ctx, mig, status); err != nil {
			return err
		}
	}

	return nil
}

// cleanupDstPod deletes the destination pod created during Preparing-
// live. Best-effort: NotFound is success (idempotent re-fire); other
// errors surface and trigger reconcile retry.
//
// status.destinationPodRef.name is the load-bearing input — it's set
// in Preparing-live B2.2 when the controller successfully Creates the
// dst pod. If the field is empty (Validating-phase failure, or
// Preparing failed before Create), there's nothing to clean up.
func (r *SwiftMigrationReconciler) cleanupDstPod(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) error {
	if status.DestinationPodRef == nil || status.DestinationPodRef.Name == "" {
		return nil
	}
	pod := &corev1.Pod{}
	pod.Name = status.DestinationPodRef.Name
	pod.Namespace = mig.Namespace
	if err := r.Delete(ctx, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete destination pod %q: %w", pod.Name, err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(mig, corev1.EventTypeNormal, "DestinationPodCleanedUp",
			"deleted destination pod %q after pre-cutover Failed", pod.Name)
	}
	return nil
}
