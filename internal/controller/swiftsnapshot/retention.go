// TTL-driven retention for SwiftSnapshot (Phase 5).
//
// When spec.ttl is set, the controller deletes the snapshot once
// status.capturedAt + ttl has elapsed — by issuing a normal Delete on itself,
// so the existing deletion path (and its deletionPolicy purge) runs unchanged.
// It refuses to delete a snapshot still referenced by a cloneFromSnapshot
// SwiftGuest or an in-flight SwiftRestore (a retention-scoped, lightweight form
// of the snapshot-lifetime guard); operator-initiated deletion is never blocked.

package swiftsnapshot

import (
	"context"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
)

const (
	// retentionMaxRequeue caps the not-yet-expired requeue so a long TTL still
	// re-checks periodically — bounds staleness after a controller restart or
	// clock skew to <= 1h past expiry (design OQ2).
	retentionMaxRequeue = time.Hour
	// retentionBlockedRequeue is how often a TTL-expired-but-referenced snapshot
	// re-checks whether its references have cleared.
	retentionBlockedRequeue = time.Minute
)

// handleRetention runs in the Ready branch when spec.ttl is set. Returns the
// RequeueAfter the caller should use (0 = no TTL action pending). On expiry it
// deletes the snapshot (unless still referenced, in which case it sets the
// RetentionBlocked condition and retries).
func (r *SwiftSnapshotReconciler) handleRetention(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (time.Duration, error) {
	if snap.Spec.TTL == nil || snap.Status.CapturedAt == nil {
		return 0, nil // no TTL, or no capture anchor yet
	}
	expiry := snap.Status.CapturedAt.Add(snap.Spec.TTL.Duration)
	now := time.Now()
	if now.Before(expiry) {
		remaining := expiry.Sub(now)
		if remaining > retentionMaxRequeue {
			remaining = retentionMaxRequeue
		}
		return remaining, nil
	}

	// Expired — but never delete a snapshot something still depends on.
	blocker, err := r.retentionBlocker(ctx, snap)
	if err != nil {
		return 0, err
	}
	if blocker != "" {
		if serr := r.setRetentionBlocked(ctx, snap, blocker); serr != nil {
			return 0, serr
		}
		return retentionBlockedRequeue, nil
	}

	// Expired + unreferenced — delete self. The deletion path honors
	// deletionPolicy (purge vs retain). IgnoreNotFound: a concurrent delete is fine.
	log.FromContext(ctx).Info("SwiftSnapshot TTL expired; deleting", "snapshot", snap.Name, "ttl", snap.Spec.TTL.Duration)
	if derr := r.Delete(ctx, snap); derr != nil {
		return 0, client.IgnoreNotFound(derr)
	}
	return 0, nil
}

// retentionBlocker returns a non-empty human reason when the snapshot is still
// referenced (and so must NOT be TTL-deleted): a cloneFromSnapshot SwiftGuest,
// or a non-terminal SwiftRestore, in the same namespace.
func (r *SwiftSnapshotReconciler) retentionBlocker(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot) (string, error) {
	return ReferenceBlocker(ctx, r.Client, snap)
}

// ReferenceBlocker returns a non-empty human reason when a snapshot is still
// referenced (and so must NOT be auto-deleted by TTL or keep-N retention): a
// cloneFromSnapshot SwiftGuest, or a non-terminal SwiftRestore, in the same
// namespace. Exported so the SwiftSnapshotSchedule keep-N controller reuses the
// exact same gate (Phase 6, OQ4). An operator-initiated `kubectl delete` is
// never gated by this — it only governs controller-driven retention deletes.
func ReferenceBlocker(ctx context.Context, c client.Reader, snap *snapshotv1alpha1.SwiftSnapshot) (string, error) {
	var guests swiftv1alpha1.SwiftGuestList
	if err := c.List(ctx, &guests, client.InNamespace(snap.Namespace)); err != nil {
		return "", err
	}
	for i := range guests.Items {
		g := &guests.Items[i]
		if g.Spec.CloneFromSnapshot != nil && g.Spec.CloneFromSnapshot.SnapshotRef.Name == snap.Name {
			return "referenced by SwiftGuest " + g.Name + " (cloneFromSnapshot)", nil
		}
	}
	var restores snapshotv1alpha1.SwiftRestoreList
	if err := c.List(ctx, &restores, client.InNamespace(snap.Namespace)); err != nil {
		return "", err
	}
	for i := range restores.Items {
		rst := &restores.Items[i]
		if rst.Spec.SnapshotRef.Name == snap.Name && !restorePhaseTerminal(rst.Status.Phase) {
			return "referenced by in-flight SwiftRestore " + rst.Name, nil
		}
	}
	return "", nil
}

// restorePhaseTerminal reports whether a SwiftRestore phase is terminal (it no
// longer reads the snapshot). Ready/Failed are terminal; an empty phase is
// treated as in-flight (just created).
func restorePhaseTerminal(p snapshotv1alpha1.SwiftRestorePhase) bool {
	return p == snapshotv1alpha1.SwiftRestorePhaseReady || p == snapshotv1alpha1.SwiftRestorePhaseFailed
}

// setRetentionBlocked sets the RetentionBlocked condition (idempotent — only
// writes status when the condition actually changed).
func (r *SwiftSnapshotReconciler) setRetentionBlocked(ctx context.Context, snap *snapshotv1alpha1.SwiftSnapshot, reason string) error {
	changed := apimeta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type:               snapshotv1alpha1.SwiftSnapshotConditionRetentionBlocked,
		Status:             metav1.ConditionTrue,
		Reason:             "Referenced",
		Message:            "TTL expired but deletion is deferred: " + reason,
		ObservedGeneration: snap.Generation,
	})
	if !changed {
		return nil
	}
	log.FromContext(ctx).Info("SwiftSnapshot TTL expired but still referenced; deferring deletion", "snapshot", snap.Name, "reason", reason)
	return r.Status().Update(ctx, snap)
}
