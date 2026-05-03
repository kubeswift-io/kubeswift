// Package swiftmigration reconciles SwiftMigration resources for Phase 1
// of live migration: offline migration via direct PVC reuse.
//
// State machine:
//
//	Pending → Validating → Preparing → StopAndCopy → Resuming → Completed
//	                            │
//	                            └── (or Failed | Cancelled at any point)
//
// Approach A (in-place SwiftGuest patch): the source SwiftGuest's CR
// identity is unchanged across the migration (same UID throughout).
// Only spec.runPolicy and spec.nodeName are patched. The PVC ownerRef
// stays valid so the SwiftGuest controller's EnsureRootDiskClone path
// runs unchanged. Per the Phase 1 spike findings.
//
// Risk 3 mitigation: re-entrant reconciles must not corrupt phase
// transitions. The controller writes a "kubeswift.io/migration-in-
// progress: <name>" annotation on the SwiftGuest at first Preparing
// entry and treats it as the source-of-truth idempotency marker on
// subsequent re-entries. Same shape as the swiftrestore PR-#18 fix.
//
// Phase implementations land in subsequent commits:
//
//   - commit 6: Validating (capacity check, defense-in-depth re-run of
//     webhook rules, structured Compatible condition)
//   - commit 7: Preparing (annotation marker, runPolicy=Stopped patch,
//     Delete(pod), dual-poll for pod-gone + PVC-detached)
//   - commit 8: StopAndCopy (single client.MergeFrom patch of
//     runPolicy=Running + nodeName=target, atomicity-critical per
//     architect Q1)
//   - commit 9: Resuming + Completed (poll for GuestRunning condition,
//     compute observedDowntime, clear annotation, set Ready=True)
//   - commit 10: Failure handling + cancellation (drive-forward post-
//     cutover per architect Risk 2)
//
// This commit ships the skeleton: Reconcile dispatch, persist helper,
// terminal-state shortcut, and SetupWithManager. Each phase handler
// is a stub that returns "not implemented" until its commit lands.
package swiftmigration

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// Standard reasons used for Conditions and Events. Centralised so
// operator-facing tooling and tests can pin specific values.
const (
	ReasonValidating         = "Validating"
	ReasonPreparing          = "Preparing"
	ReasonStopAndCopy        = "StopAndCopy"
	ReasonResuming           = "Resuming"
	ReasonCompleted          = "Completed"
	ReasonValidationFailed   = "ValidationFailed"
	ReasonMigrationFailed    = "MigrationFailed"
	ReasonCancelled          = "Cancelled"
	ReasonNotImplemented     = "NotImplemented"
	ReasonGuestNotFound      = "GuestNotFound"
	ReasonTargetNodeNotFound = "TargetNodeNotFound"
	ReasonIPWillChange       = "IPWillChange"
)

// SwiftMigrationReconciler reconciles SwiftMigration resources.
type SwiftMigrationReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// phaseResult is the return type of every per-phase handler. It
// replaces the earlier (advanced, requeue, errMsg, err) quadruple to
// give a named place for the Phase 3a `failureReason` enum. The
// failureMsg/failureReason pair maps to a Failed terminal phase via
// dispatchResult; advanced/requeue/err preserve Phase 1's semantics.
//
// Construction conventions (see helpers below):
//   - phaseAdvance()        — phase advanced, immediate requeue
//   - phaseRequeue(d)       — still in current phase, requeue after d
//   - phaseFailure(msg, r)  — user-actionable failure → Failed phase
//     with status.failureReason = r (live mode);
//     leave r empty for offline-mode failures.
//   - phaseTransient(err)   — transient/retry-worthy error
//
// Phase 1 offline-mode handlers leave FailureReason empty when calling
// phaseFailure — the Phase 1 status schema doesn't carry the enum, and
// the FailureMsg alone is sufficient for offline's simpler failure
// taxonomy. Phase 3a live-mode handlers always pair msg + reason.
type phaseResult struct {
	// Advanced indicates the phase transition completed; dispatchResult
	// requeues immediately so the next handler runs without poll
	// latency.
	Advanced bool
	// Requeue is the interval to requeue after when still in the
	// current phase (Advanced=false). Zero means no requeue (caller
	// will fall through to controller-runtime's default cadence).
	Requeue time.Duration
	// Err is a transient/retry-worthy error returned from the handler.
	// dispatchResult propagates it to controller-runtime so the resource
	// is requeued with backoff. Distinct from FailureMsg, which
	// represents a user-actionable migration failure.
	Err error
	// FailureMsg is set when the handler observed a user-actionable
	// failure (e.g., source guest missing, target node not Ready).
	// dispatchResult maps this to the Failed terminal phase with
	// status.failureMessage = FailureMsg.
	FailureMsg string
	// FailureReason is the §6 enum value paired with FailureMsg in
	// live mode (one of: Cancelled, PodTerminated, SourcePodReplaced,
	// Timeout, Other). Empty for offline-mode failures and for
	// non-failure paths. See api/migration/v1alpha1's
	// FailureReason* constants.
	FailureReason string
}

// phaseAdvance returns a phaseResult that signals "phase advanced;
// requeue immediately to start the next handler."
func phaseAdvance() *phaseResult {
	return &phaseResult{Advanced: true}
}

// phaseRequeue returns a phaseResult that signals "still in current
// phase; requeue after d." Most poll loops use this with a fixed
// per-phase cadence (validating: 0, preparing: 5s, etc).
func phaseRequeue(d time.Duration) *phaseResult {
	return &phaseResult{Requeue: d}
}

// phaseFailure returns a phaseResult that maps to the Failed terminal
// phase. The msg is human-readable; reason is the §6 enum (use one of
// the FailureReason* constants in api/migration/v1alpha1; pass "" for
// offline-mode where the enum is not populated).
func phaseFailure(msg, reason string) *phaseResult {
	return &phaseResult{FailureMsg: msg, FailureReason: reason}
}

// phaseTransient returns a phaseResult carrying a transient error.
// dispatchResult propagates the error to controller-runtime which
// requeues with exponential backoff.
func phaseTransient(err error) *phaseResult {
	return &phaseResult{Err: err}
}

// Reconcile drives the SwiftMigration state machine. The shape mirrors
// the swiftsnapshot/swiftrestore controllers: load the resource,
// short-circuit on terminal phases, dispatch to per-phase handler,
// persist the status diff if anything changed.
func (r *SwiftMigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("swiftmigration", req.NamespacedName)

	var mig migrationv1alpha1.SwiftMigration
	if err := r.Get(ctx, req.NamespacedName, &mig); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Cancellation: the resource is being deleted while in flight.
	// handleCancellation runs the rollback (pre-cutover) or just
	// clears the annotation (post-cutover), then drops the
	// finalizer to allow deletion to proceed.
	if mig.DeletionTimestamp != nil {
		return r.handleCancellation(ctx, &mig)
	}

	// Terminal phases: nothing more to do. Idempotency: re-reconcile
	// of a Completed/Failed/Cancelled SwiftMigration is a no-op. The
	// SwiftMigration controller watches Pod and SwiftGuest events
	// (SetupWithManager) and a single completed migration receives
	// many spurious enqueues over its lifetime — anything past this
	// point should be skipped on stale terminal-phase resources.
	//
	// Order: check phase first, then short-circuit immediately when
	// no work is left. The finalizer-cleanup branch only runs when
	// the finalizer is still present (e.g., after a controller crash
	// between the terminal-phase patch and the finalizer-removal
	// patch in dispatchResult/handleCancellation). Skipping the
	// removeFinalizer call when the finalizer is already gone avoids
	// an unnecessary API roundtrip on every spurious enqueue.
	if isTerminalPhase(mig.Status.Phase) {
		if !hasFinalizer(&mig) {
			return ctrl.Result{}, nil
		}
		if err := r.removeFinalizer(ctx, &mig); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer on first reconcile so cancellation mid-flight
	// gets a chance to clean up the SwiftGuest annotation.
	if err := r.ensureFinalizer(ctx, &mig); err != nil {
		return ctrl.Result{}, err
	}

	// Phase 3a §5.3 cancel handling. Fires BEFORE phase dispatch
	// so that a spec.cancelRequested flag gets honored before the
	// per-phase handler runs its checks. Two paths:
	//   - Pre-cutover live cancel: drives to Cancelled (handler
	//     writes cancel annotation on dst pod, waits for D1 ack
	//     within 30s budget, deletes dst pod, finalizes).
	//   - Post-cutover live cancel: sets CancelIgnored condition
	//     with reason=PastCutover; returns false-handled so phase
	//     dispatch continues to Completed normally.
	// Offline mode is silently ignored (out of Phase 3a scope).
	if handled, res, err := r.honorCancel(ctx, &mig); handled || err != nil {
		return res, err
	}

	// First reconcile: stamp StartedAt and transition Pending →
	// Validating. The controller advances phase exactly once per
	// transition; subsequent reconciles in the same phase poll for
	// completion.
	phase := mig.Status.Phase
	if phase == "" {
		phase = migrationv1alpha1.SwiftMigrationPhasePending
	}
	status := mig.Status.DeepCopy()
	if status.StartedAt == nil {
		now := metav1.Now()
		status.StartedAt = &now
	}

	switch phase {
	case migrationv1alpha1.SwiftMigrationPhasePending:
		// Transition to Validating on first reconcile.
		setPhase(status, migrationv1alpha1.SwiftMigrationPhaseValidating)
		setReadyCondition(status, metav1.ConditionFalse, ReasonValidating, "running validation")
		setPhaseDetail(status, "validating migration request")
		return ctrl.Result{Requeue: true}, r.persist(ctx, &mig, status)

	case migrationv1alpha1.SwiftMigrationPhaseValidating:
		return r.dispatchResult(ctx, &mig, status, r.handleValidating(ctx, &mig, status))

	case migrationv1alpha1.SwiftMigrationPhasePreparing:
		return r.dispatchResult(ctx, &mig, status, r.handlePreparing(ctx, &mig, status))

	case migrationv1alpha1.SwiftMigrationPhaseStopAndCopy:
		return r.dispatchResult(ctx, &mig, status, r.handleStopAndCopy(ctx, &mig, status))

	case migrationv1alpha1.SwiftMigrationPhaseResuming:
		return r.dispatchResult(ctx, &mig, status, r.handleResuming(ctx, &mig, status))

	default:
		// Unknown phase (e.g., a resource written by a Phase 3+
		// controller and observed by a Phase 1 controller). Treat as
		// opaque: log and requeue without action.
		logger.Info("unknown phase, requeuing without action", "phase", phase)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
}

// dispatchResult is the post-handler decision tree shared by all
// non-terminal phases. The phaseResult struct's fields are interpreted
// in priority order: Err (transient retry) → FailureMsg (terminal
// Failed with FailureReason) → Advanced (immediate requeue) → Requeue
// (still in current phase, requeue after).
//
// FailureMsg vs Err discipline (from the swiftrestore precedent):
// handlers report user-actionable failures via FailureMsg, paired with
// the §6 enum FailureReason for live mode; transient/retry-worthy
// problems use Err so controller-runtime applies exponential backoff.
func (r *SwiftMigrationReconciler) dispatchResult(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
	result *phaseResult,
) (ctrl.Result, error) {
	if result.Err != nil {
		return ctrl.Result{}, result.Err
	}
	if result.FailureMsg != "" {
		setPhase(status, migrationv1alpha1.SwiftMigrationPhaseFailed)
		setReadyCondition(status, metav1.ConditionFalse, ReasonMigrationFailed, result.FailureMsg)
		status.FailureMessage = result.FailureMsg
		// Live-mode failure-reason taxonomy (§6); empty for offline-mode
		// failures, which keep the simpler Phase 1 behavior.
		if result.FailureReason != "" {
			status.FailureReason = result.FailureReason
		}
		now := metav1.Now()
		status.CompletedAt = &now
		// Cleanup must run BEFORE persist so a re-reconcile observing
		// the Failed phase doesn't immediately drop the finalizer
		// (terminal-phase shortcut) before cleanup completes.
		if cleanupErr := r.onTerminalPhase(ctx, mig, status); cleanupErr != nil {
			return ctrl.Result{}, fmt.Errorf("terminal-phase cleanup: %w", cleanupErr)
		}
		return ctrl.Result{}, r.persist(ctx, mig, status)
	}
	if result.Advanced {
		// Phase advanced; persist and requeue immediately to start the
		// next handler.
		return ctrl.Result{Requeue: true}, r.persist(ctx, mig, status)
	}
	// Still in current phase; persist any progress (phaseDetail,
	// observed conditions) and requeue after the handler-specified
	// interval.
	return ctrl.Result{RequeueAfter: result.Requeue}, r.persist(ctx, mig, status)
}

// handleCancellation is implemented in failure.go.

// --- Phase handlers ---
//
// handleValidating is in validating.go.
// handlePreparing is in preparing.go.
// handleStopAndCopy is in stopandcopy.go.
// handleResuming is in resuming.go.

// --- Helpers ---

// isTerminalPhase returns true for SwiftMigration phases where the
// outcome has been decided. Used by Reconcile's terminal-phase short-
// circuit and (textually duplicated) by the validating webhook to
// skip cluster-state validation on metadata-only patches against
// already-decided migrations. Both copies must remain in sync; the
// controller can't import the webhook (cycle) and the webhook only
// imports api types.
func isTerminalPhase(phase migrationv1alpha1.SwiftMigrationPhase) bool {
	switch phase {
	case migrationv1alpha1.SwiftMigrationPhaseCompleted,
		migrationv1alpha1.SwiftMigrationPhaseFailed,
		migrationv1alpha1.SwiftMigrationPhaseCancelled:
		return true
	}
	return false
}

// hasFinalizer returns true when FinalizerName is attached to the
// SwiftMigration. Used to skip the removeFinalizer roundtrip on the
// terminal-phase short-circuit when there's nothing to remove.
func hasFinalizer(mig *migrationv1alpha1.SwiftMigration) bool {
	for _, f := range mig.Finalizers {
		if f == FinalizerName {
			return true
		}
	}
	return false
}

// setPhase advances the SwiftMigration to phase p, leaving conditions
// alone (callers set Ready/Compatible separately).
func setPhase(status *migrationv1alpha1.SwiftMigrationStatus, p migrationv1alpha1.SwiftMigrationPhase) {
	status.Phase = p
}

// setPhaseDetail updates the short human-readable sub-state string
// shown in `kubectl get swiftmigration -o wide`. Idempotent: a no-op
// when the new value matches the current value.
func setPhaseDetail(status *migrationv1alpha1.SwiftMigrationStatus, detail string) {
	if status.PhaseDetail == detail {
		return
	}
	status.PhaseDetail = detail
}

// setReadyCondition sets (or replaces) the Ready condition.
func setReadyCondition(status *migrationv1alpha1.SwiftMigrationStatus, s metav1.ConditionStatus, reason, message string) {
	setCondition(status, migrationv1alpha1.SwiftMigrationConditionReady, s, reason, message)
}

// setCondition is a generic Conditions list updater. If a condition of
// the given type already exists, its status/reason/message are updated
// (and lastTransitionTime is bumped only when status changes); else
// a new entry is appended.
func setCondition(status *migrationv1alpha1.SwiftMigrationStatus, condType string, s metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i := range status.Conditions {
		c := &status.Conditions[i]
		if c.Type != condType {
			continue
		}
		// Update in place. Only bump LastTransitionTime when the
		// status flips — preserves the conventional Conditions
		// semantics observers rely on.
		if c.Status != s {
			c.LastTransitionTime = now
		}
		c.Status = s
		c.Reason = reason
		c.Message = message
		return
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             s,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// persist writes the status diff back to the API server. Returns nil
// when there's no change (saves an unnecessary API round-trip).
func (r *SwiftMigrationReconciler) persist(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) error {
	if equality.Semantic.DeepEqual(&mig.Status, status) {
		return nil
	}
	patch := client.MergeFrom(mig.DeepCopy())
	mig.Status = *status
	if err := r.Status().Patch(ctx, mig, patch); err != nil {
		if apierrors.IsNotFound(err) {
			// Resource deleted between Get and Patch — let the next
			// reconcile observe the deletion.
			return nil
		}
		return fmt.Errorf("patch SwiftMigration status: %w", err)
	}
	return nil
}

// SetupWithManager registers the reconciler. The watch on Pod is
// scoped to launcher pods labeled "swift.kubeswift.io/guest=<name>"
// and maps Pod events back to the SwiftMigration referencing the
// guest. This drives the Preparing phase's pod-termination wait
// (commit 7) and the Resuming phase's GuestRunning poll (commit 9)
// without periodic resync latency.
func (r *SwiftMigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&migrationv1alpha1.SwiftMigration{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.podToMigrations),
		).
		Watches(
			&swiftv1alpha1.SwiftGuest{},
			handler.EnqueueRequestsFromMapFunc(r.guestToMigrations),
		).
		Complete(r)
}

// podToMigrations maps a Pod event to SwiftMigrations referencing the
// pod's SwiftGuest. Two cases per design §5.1:
//
//   - Launcher pod whose name == SwiftGuest name: Phase 1's src-pod
//     observation path. Returns the active SwiftMigration whose
//     guestRef.name matches the pod name.
//   - Pod carrying the kubeswift.io/migration label: dst pod (set at
//     creation in B2.2) AND src pod after the StopAndCopy entry label
//     patch (architect F-3). Returns the SwiftMigration whose name
//     matches the label value, regardless of pod name.
//
// The label-based path is what enables Phase 3a's informer-driven
// observation of dst pod migration-status transitions without
// SyncPeriod latency. Both paths short-circuit terminal-phase
// SwiftMigrations.
func (r *SwiftMigrationReconciler) podToMigrations(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	// Label-based path: covers dst pods (named <guest>-mig-<uid>,
	// not matched by name-based lookup) and src pods after the
	// StopAndCopy entry label patch. See LabelMigrationName.
	if migName := pod.Labels[LabelMigrationName]; migName != "" {
		var mig migrationv1alpha1.SwiftMigration
		if err := r.Get(ctx, client.ObjectKey{Name: migName, Namespace: pod.Namespace}, &mig); err == nil {
			switch mig.Status.Phase {
			case migrationv1alpha1.SwiftMigrationPhaseCompleted,
				migrationv1alpha1.SwiftMigrationPhaseFailed,
				migrationv1alpha1.SwiftMigrationPhaseCancelled:
				// Terminal — no enqueue.
			default:
				return []ctrlreconcile.Request{{
					NamespacedName: client.ObjectKey{Name: mig.Name, Namespace: mig.Namespace},
				}}
			}
		}
		// Get failure or terminal phase: fall through to name-based
		// lookup below in case the same event matches a different
		// SwiftMigration via guestRef.
	}
	return r.findActiveMigrationsForGuest(ctx, pod.Namespace, pod.Name)
}

// guestToMigrations maps a SwiftGuest event to SwiftMigrations
// referencing it. Used to react to status.network.primaryIP becoming
// populated (commit 9 Resuming poll) and GuestRunning condition flips.
func (r *SwiftMigrationReconciler) guestToMigrations(ctx context.Context, obj client.Object) []ctrlreconcile.Request {
	guest, ok := obj.(*swiftv1alpha1.SwiftGuest)
	if !ok {
		return nil
	}
	return r.findActiveMigrationsForGuest(ctx, guest.Namespace, guest.Name)
}

// findActiveMigrationsForGuest returns at most one Request: the
// non-terminal SwiftMigration in `namespace` referencing a guest
// named `guestName`. Helper shared by podToMigrations and
// guestToMigrations.
func (r *SwiftMigrationReconciler) findActiveMigrationsForGuest(ctx context.Context, namespace, guestName string) []ctrlreconcile.Request {
	var migs migrationv1alpha1.SwiftMigrationList
	if err := r.List(ctx, &migs, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var out []ctrlreconcile.Request
	for i := range migs.Items {
		m := &migs.Items[i]
		if m.Spec.GuestRef.Name != guestName {
			continue
		}
		switch m.Status.Phase {
		case migrationv1alpha1.SwiftMigrationPhaseCompleted,
			migrationv1alpha1.SwiftMigrationPhaseFailed,
			migrationv1alpha1.SwiftMigrationPhaseCancelled:
			continue
		}
		out = append(out, ctrlreconcile.Request{
			NamespacedName: client.ObjectKey{Name: m.Name, Namespace: m.Namespace},
		})
	}
	return out
}
