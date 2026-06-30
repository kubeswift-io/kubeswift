// Package swiftsnapshotschedule reconciles SwiftSnapshotSchedule resources:
// it creates a SwiftSnapshot each time the cron schedule fires (Phase 6).
// keep-N retention pruning lands in a follow-up (PR 4); this controller does
// cron evaluation + snapshot creation + concurrency + status only.
package swiftsnapshotschedule

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	snapshotv1alpha1 "github.com/kubeswift-io/kubeswift/api/snapshot/v1alpha1"
)

const (
	// maxRequeue caps the wait to the next tick so a far-future schedule still
	// re-checks periodically (mirrors the ttl re-check cap).
	maxRequeue = time.Hour
	// suspendedRequeue is the cheap re-check cadence for a suspended schedule.
	suspendedRequeue = time.Hour
	// missedTickCap bounds catch-up after a long outage: the controller fires at
	// most ONE snapshot (the most recent missed tick), never a backlog.
	missedTickCap = 100
)

// SwiftSnapshotScheduleReconciler reconciles SwiftSnapshotSchedule resources.
type SwiftSnapshotScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

func (r *SwiftSnapshotScheduleReconciler) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *SwiftSnapshotScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var sched snapshotv1alpha1.SwiftSnapshotSchedule
	if err := r.Get(ctx, req.NamespacedName, &sched); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if sched.Spec.Suspend {
		return ctrl.Result{RequeueAfter: suspendedRequeue}, nil
	}

	cronSched, err := cron.ParseStandard(sched.Spec.Schedule)
	if err != nil {
		// Invalid cron (the webhook rejects this once it lands). Don't hot-loop —
		// the schedule is re-reconciled when the spec is fixed.
		logger.Error(err, "invalid spec.schedule; not scheduling", "schedule", sched.Spec.Schedule)
		return ctrl.Result{}, nil
	}

	now := r.clock()
	status := sched.Status.DeepCopy()

	// Refresh observed status (active + lastSuccessfulTime) from owned snapshots.
	children, err := r.ownedSnapshots(ctx, &sched)
	if err != nil {
		return ctrl.Result{}, err
	}
	updateObservedStatus(status, children)

	// The most recent due tick after the last fire (or the schedule's creation).
	earliest := sched.CreationTimestamp.Time
	if status.LastScheduleTime != nil {
		earliest = status.LastScheduleTime.Time
	}
	dueTick, due := mostRecentDue(cronSched, earliest, now)
	if due {
		switch {
		case tooLate(&sched, dueTick, now):
			logger.Info("skipping tick past startingDeadline", "tick", dueTick)
			setLastSchedule(status, dueTick)
		case sched.Spec.ConcurrencyPolicy == snapshotv1alpha1.ConcurrencyForbid && hasInFlight(children):
			logger.Info("skipping tick: a prior scheduled snapshot is still in flight (Forbid)", "tick", dueTick)
			setLastSchedule(status, dueTick)
		default:
			if cerr := r.createScheduledSnapshot(ctx, &sched, dueTick); cerr != nil {
				return ctrl.Result{}, cerr
			}
			logger.Info("created scheduled snapshot", "tick", dueTick)
			setLastSchedule(status, dueTick)
		}
	}

	if err := r.persistStatus(ctx, &sched, status); err != nil {
		return ctrl.Result{}, err
	}

	// keep-N retention: prune the oldest Ready snapshots beyond keepLast. Runs
	// every reconcile (the Owns(SwiftSnapshot) watch fires this as children
	// reach Ready), not just on a tick. Uses the pre-create child list — a
	// just-created snapshot is still Capturing, so never a prune candidate.
	if err := r.pruneKeepN(ctx, &sched, children); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueToNext(cronSched, now)}, nil
}

// mostRecentDue returns the latest scheduled time in (earliest, now], and
// whether one exists. It fires at most once per reconcile (the most recent
// missed tick), coalescing a backlog after an outage rather than stampeding.
func mostRecentDue(sched cron.Schedule, earliest, now time.Time) (time.Time, bool) {
	var due time.Time
	found := false
	t := sched.Next(earliest)
	for i := 0; !t.After(now); i++ {
		due, found = t, true
		if i >= missedTickCap {
			break
		}
		t = sched.Next(t)
	}
	return due, found
}

// tooLate reports whether a due tick is older than startingDeadlineSeconds.
func tooLate(sched *snapshotv1alpha1.SwiftSnapshotSchedule, tick, now time.Time) bool {
	d := sched.Spec.StartingDeadlineSeconds
	return d != nil && now.Sub(tick) > time.Duration(*d)*time.Second
}

// requeueToNext returns the capped wait until the next scheduled time
// (computed from the injected now, not the wall clock).
func requeueToNext(sched cron.Schedule, now time.Time) time.Duration {
	wait := sched.Next(now).Sub(now)
	if wait < time.Second {
		wait = time.Second
	}
	if wait > maxRequeue {
		wait = maxRequeue
	}
	return wait
}

func (r *SwiftSnapshotScheduleReconciler) ownedSnapshots(ctx context.Context, sched *snapshotv1alpha1.SwiftSnapshotSchedule) ([]snapshotv1alpha1.SwiftSnapshot, error) {
	var list snapshotv1alpha1.SwiftSnapshotList
	if err := r.List(ctx, &list,
		client.InNamespace(sched.Namespace),
		client.MatchingLabels{snapshotv1alpha1.ScheduleLabel: sched.Name},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// createScheduledSnapshot creates a SwiftSnapshot from the template for a tick.
// The name is deterministic in the tick (<schedule>-<unix>), so a re-reconcile
// of the same tick is idempotent (Create returns AlreadyExists).
func (r *SwiftSnapshotScheduleReconciler) createScheduledSnapshot(ctx context.Context, sched *snapshotv1alpha1.SwiftSnapshotSchedule, tick time.Time) error {
	snap := &snapshotv1alpha1.SwiftSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("%s-%d", sched.Name, tick.Unix()),
			Namespace:   sched.Namespace,
			Labels:      mergeLabels(sched.Spec.Template.Metadata.Labels, sched.Name),
			Annotations: copyMap(sched.Spec.Template.Metadata.Annotations),
		},
		Spec: *sched.Spec.Template.Spec.DeepCopy(),
	}
	if err := ctrl.SetControllerReference(sched, snap, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, snap); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// mergeLabels copies the template labels and forces the schedule grouping label.
func mergeLabels(tmpl map[string]string, scheduleName string) map[string]string {
	out := map[string]string{}
	for k, v := range tmpl {
		out[k] = v
	}
	out[snapshotv1alpha1.ScheduleLabel] = scheduleName
	return out
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// updateObservedStatus refreshes Active (non-terminal children) and
// LastSuccessfulTime (newest Ready child's capturedAt) from the owned snapshots.
func updateObservedStatus(status *snapshotv1alpha1.SwiftSnapshotScheduleStatus, children []snapshotv1alpha1.SwiftSnapshot) {
	var active []string
	var lastSuccess *metav1.Time
	for i := range children {
		c := &children[i]
		if !snapshotTerminal(c.Status.Phase) {
			active = append(active, c.Name)
		}
		if c.Status.Phase == snapshotv1alpha1.SwiftSnapshotPhaseReady && c.Status.CapturedAt != nil {
			if lastSuccess == nil || c.Status.CapturedAt.After(lastSuccess.Time) {
				lastSuccess = c.Status.CapturedAt.DeepCopy()
			}
		}
	}
	status.Active = active
	if lastSuccess != nil {
		status.LastSuccessfulTime = lastSuccess
	}
}

func snapshotTerminal(p snapshotv1alpha1.SwiftSnapshotPhase) bool {
	return p == snapshotv1alpha1.SwiftSnapshotPhaseReady || p == snapshotv1alpha1.SwiftSnapshotPhaseFailed
}

func hasInFlight(children []snapshotv1alpha1.SwiftSnapshot) bool {
	for i := range children {
		if !snapshotTerminal(children[i].Status.Phase) {
			return true
		}
	}
	return false
}

func setLastSchedule(status *snapshotv1alpha1.SwiftSnapshotScheduleStatus, t time.Time) {
	mt := metav1.NewTime(t)
	status.LastScheduleTime = &mt
}

func (r *SwiftSnapshotScheduleReconciler) persistStatus(ctx context.Context, sched *snapshotv1alpha1.SwiftSnapshotSchedule, status *snapshotv1alpha1.SwiftSnapshotScheduleStatus) error {
	if apiequality.Semantic.DeepEqual(&sched.Status, status) {
		return nil
	}
	sched.Status = *status
	return r.Status().Update(ctx, sched)
}

// SetupWithManager registers the reconciler. Owns(SwiftSnapshot) so a child
// snapshot reaching Ready/Failed re-triggers the schedule (refreshes
// lastSuccessfulTime + active, and — once PR 4 lands — keep-N pruning).
func (r *SwiftSnapshotScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.SwiftSnapshotSchedule{}).
		Owns(&snapshotv1alpha1.SwiftSnapshot{}).
		Named("swiftsnapshotschedule").
		Complete(r)
}
