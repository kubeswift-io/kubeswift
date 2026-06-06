package swiftsnapshotschedule

import (
	"context"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/projectbeskar/kubeswift/api/snapshot/v1alpha1"
	"github.com/projectbeskar/kubeswift/internal/controller/swiftsnapshot"
	"github.com/projectbeskar/kubeswift/internal/metrics"
)

// pruneKeepN deletes the schedule's oldest snapshots beyond
// spec.retention.keepLast. Only Ready, not-already-deleting snapshots count
// toward the budget and are eligible for pruning (a non-terminal capture is
// never deleted; a Failed one is left for inspection — OQ1). A snapshot still
// referenced by a cloneFromSnapshot guest / in-flight restore is skipped and
// pruned on a later reconcile (the shared ReferenceBlocker gate — OQ4). The
// delete is a normal SwiftSnapshot delete, so its deletionPolicy purge runs.
func (r *SwiftSnapshotScheduleReconciler) pruneKeepN(
	ctx context.Context,
	sched *snapshotv1alpha1.SwiftSnapshotSchedule,
	children []snapshotv1alpha1.SwiftSnapshot,
) error {
	if sched.Spec.Retention == nil || sched.Spec.Retention.KeepLast == nil {
		return nil
	}
	keep := int(*sched.Spec.Retention.KeepLast)

	ready := make([]snapshotv1alpha1.SwiftSnapshot, 0, len(children))
	for i := range children {
		c := children[i]
		if c.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseReady && c.DeletionTimestamp == nil {
			ready = append(ready, c)
		}
	}
	if len(ready) <= keep {
		return nil
	}

	// Newest first, so ready[keep:] are the oldest beyond the budget.
	sort.Slice(ready, func(i, j int) bool { return snapTime(ready[i]).After(snapTime(ready[j])) })

	logger := log.FromContext(ctx)
	for i := keep; i < len(ready); i++ {
		old := ready[i]
		blocker, err := swiftsnapshot.ReferenceBlocker(ctx, r.Client, &old)
		if err != nil {
			return err
		}
		if blocker != "" {
			logger.Info("keep-N: deferring prune of referenced snapshot", "snapshot", old.Name, "reason", blocker)
			continue
		}
		if err := r.Delete(ctx, &old); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		logger.Info("keep-N: pruned snapshot", "snapshot", old.Name, "keepLast", keep)
		metrics.SnapshotSchedulePrunedTotal.Inc()
	}
	return nil
}

// snapTime is a snapshot's effective age key: capturedAt if Ready, else the
// creation timestamp.
func snapTime(s snapshotv1alpha1.SwiftSnapshot) time.Time {
	if s.Status.CapturedAt != nil {
		return s.Status.CapturedAt.Time
	}
	return s.CreationTimestamp.Time
}
