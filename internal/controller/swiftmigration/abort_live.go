package swiftmigration

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/kubeswift-io/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/kubeswift-io/kubeswift/api/swift/v1alpha1"
	"github.com/kubeswift-io/kubeswift/internal/metrics"
)

// Abandoning a live migration before its commit point (#893).
//
// Every path that gives up on a live migration -- spec.cancelRequested, an
// expired spec.timeout, a failure in any pre-cutover phase, deletion of the
// SwiftMigration -- ends by deleting the destination launcher. Once the
// source has started sending, that is safe only when it is known how the
// send ended: a transfer that completes after its destination was deleted
// leaves no guest (the source exits on success). resolveSourceOwnership is
// the one place that finds out. Its answer:
//
//	abortNoSend          this migration never sent on the source, or there
//	                     is no destination left to protect
//	abortSourceRetained  the source reported the send ended without
//	                     completing (cancelled or failed): the source runs
//	                     the guest
//	abortDestinationDead legacy path: the receiving Cloud Hypervisor was
//	                     killed before it ran the guest, so the send fails
//	                     and the source resumes
//	abortCommitted       the source reported complete or the destination
//	                     running: the destination is the guest
//	abortPending         not known yet: delete nothing, look again
//	abortUnresolved      both mechanisms exhausted without an answer; the
//	                     caller does the long-standing fallback cleanup
//
// How it finds out depends on the source launcher:
//
//   - migration-cancel (Cloud Hypervisor >= v54): cancel-send on the source
//     (vm.cancel-migration) and read the send's own report (cancel_source.go).
//     Undeliverable, or no report in sourceCancelTimeout: the next step.
//   - otherwise (v53 and older launchers), or as that fallback: the
//     destination cancel, which kills the receiving Cloud Hypervisor unless
//     its VM already runs (cancel_live.go). No acknowledgement within
//     cancelAckTimeout: abortUnresolved.
//
// Every input is a pod annotation, so a restarted controller -- or a pass
// from a stale cache -- resumes where the last one stopped. Callers finish
// the abort their own way (Cancelled, Failed, finalizer removed) and only
// after an outcome other than abortPending; on abortCommitted they never
// abort at all.
type abortOutcome int

const (
	abortPending abortOutcome = iota
	abortNoSend
	abortSourceRetained
	abortDestinationDead
	abortCommitted
	abortUnresolved
)

func (o abortOutcome) String() string {
	switch o {
	case abortNoSend:
		return "not-started"
	case abortSourceRetained, abortDestinationDead:
		return "cancelled"
	case abortCommitted:
		return "committed"
	case abortUnresolved:
		return "unresolved"
	default:
		return "pending"
	}
}

// abortTrigger is why a migration is being abandoned (a metric label and
// part of what operators read in Events).
type abortTrigger string

const (
	abortExplicit abortTrigger = "explicit" // spec.cancelRequested
	abortDeadline abortTrigger = "deadline" // spec.timeout
	abortDeletion abortTrigger = "deletion" // the SwiftMigration deleted
	abortFailure  abortTrigger = "failure"  // any other pre-cutover failure
)

// abortResolution is resolveSourceOwnership's answer.
type abortResolution struct {
	outcome abortOutcome
	// path is the mechanism that settled it: source, destination, or none.
	path string
	// detail says what settled it, for messages.
	detail string
	// requeue applies while outcome is abortPending.
	requeue ctrl.Result
}

const cancelPathNone = "none"

// settled reports whether destination cleanup may proceed.
func (a abortResolution) settled() bool {
	return a.outcome != abortPending && a.outcome != abortCommitted
}

// resolveSourceOwnership finds out, before anything is deleted, whether an
// abandoned live migration's source still owns the guest. See the file
// comment.
func (r *SwiftMigrationReconciler) resolveSourceOwnership(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	trigger abortTrigger,
) (abortResolution, error) {
	logger := log.FromContext(ctx).WithValues("migration", mig.Name, "trigger", trigger)
	committed, err := r.liveCommitted(ctx, mig)
	if err != nil {
		return abortResolution{}, fmt.Errorf("determine commit point: %w", err)
	}
	if committed {
		return abortResolution{outcome: abortCommitted, path: cancelPathNone,
			detail: "the migration had already committed"}, nil
	}

	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			// The guest is gone; its launchers go with it.
			return abortResolution{outcome: abortNoSend, path: cancelPathNone,
				detail: "the source SwiftGuest is gone"}, nil
		}
		return abortResolution{}, fmt.Errorf("get source guest: %w", err)
	}
	dstName, err := dstPodName(mig, guest.Name)
	if err != nil {
		return abortResolution{outcome: abortNoSend, path: cancelPathNone,
			detail: fmt.Sprintf("no destination pod name: %v", err)}, nil
	}
	var dst corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: dstName, Namespace: mig.Namespace}, &dst); err != nil {
		if apierrors.IsNotFound(err) {
			// Nothing to protect at the destination; a send to it fails and
			// the source resumes.
			return abortResolution{outcome: abortNoSend, path: cancelPathNone,
				detail: "the destination pod does not exist"}, nil
		}
		return abortResolution{}, fmt.Errorf("get destination pod: %w", err)
	}

	var src *corev1.Pod
	var srcPod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodLookupName(mig, &guest), Namespace: mig.Namespace}, &srcPod); err == nil {
		src = &srcPod
	} else if !apierrors.IsNotFound(err) {
		return abortResolution{}, fmt.Errorf("get source pod: %w", err)
	}

	// The source already reported this send failed: it runs the guest (Cloud
	// Hypervisor resumes the source on a failed send since v53).
	if src != nil && srcReportedSendEnded(mig, src) {
		return abortResolution{outcome: abortSourceRetained, path: cancelPathSource,
			detail: strings.TrimPrefix(normalizeStatusDetail(src.Annotations[AnnotationMigrationStatusDtl]), "cancelled: ")}, nil
	}

	// The destination cancel, once started, finishes the job: it was the
	// fallback, or the source could not cancel.
	if !destinationCancelIssued(mig, &dst) {
		if src != nil && !sendMayHaveStarted(mig, src) {
			// The send was never written to the source: nothing to cancel.
			// (A send written but not yet taken up is replaced by the
			// cancel-send or cleared below, so this holds only before any.)
			return abortResolution{outcome: abortNoSend, path: cancelPathNone,
				detail: "the source never started sending"}, nil
		}
		if src != nil && sourceCanCancel(mig, src) {
			res, fallback, err := r.resolveOnSource(ctx, mig, src, &dst, trigger)
			if err != nil || !fallback {
				return res, err
			}
		}
	}
	logger.V(1).Info("resolving through the destination cancel")
	return r.resolveOnDestination(ctx, mig, &dst, trigger)
}

// srcReportedSendEnded reports whether the source reported this
// migration's current send ended without completing.
func srcReportedSendEnded(mig *migrationv1alpha1.SwiftMigration, src *corev1.Pod) bool {
	st := src.Annotations[AnnotationMigrationStatus]
	return src.Annotations[AnnotationMigrationStatusID] == sendActionID(mig) &&
		(st == MigrationStatusFailed || st == MigrationStatusRejected)
}

// sendMayHaveStarted reports whether this migration's send has reached the
// source launcher: its action, a status for it, or a cancel of it.
func sendMayHaveStarted(mig *migrationv1alpha1.SwiftMigration, src *corev1.Pod) bool {
	send := mig.Name + ":send:"
	return strings.HasPrefix(src.Annotations[AnnotationMigrationActionID], send) ||
		strings.HasPrefix(src.Annotations[AnnotationMigrationStatusID], send) ||
		sourceCancelIssued(mig, src) ||
		src.Annotations[AnnotationMigrationCancelResultID] == cancelSendID(mig)
}

// sourceCanCancel reports whether the abort goes through the source: its
// launcher can cancel (or already was asked) and has not handed the abort
// to the destination cancel.
func sourceCanCancel(mig *migrationv1alpha1.SwiftMigration, src *corev1.Pod) bool {
	cid := cancelSendID(mig)
	if src.Annotations[AnnotationMigrationCancelResultID] == cid &&
		src.Annotations[AnnotationMigrationCancelResult] == cancelResultFailed &&
		!sourceCancelIssued(mig, src) {
		return false // fell back
	}
	if sourceCancelIssued(mig, src) || src.Annotations[AnnotationMigrationCancelResultID] == cid {
		return true // already asked: finish on this path
	}
	return launcherHasFeature(src, LauncherFeatureMigrationCancel) && src.DeletionTimestamp == nil
}

// destinationCancelIssued reports whether this migration's destination
// cancel is on the destination pod.
func destinationCancelIssued(mig *migrationv1alpha1.SwiftMigration, dst *corev1.Pod) bool {
	return dst.Annotations[AnnotationMigrationAction] == MigrationActionCancel &&
		dst.Annotations[AnnotationMigrationActionID] == cancelID(mig)
}

// resolveOnDestination is the destination cancel as an ownership
// resolution: it SIGKILLs the receiving Cloud Hypervisor unless its VM
// already runs (swiftletd refuses then, and the destination's `running`
// commits the migration). The source's send then fails and its Cloud
// Hypervisor resumes the guest. Used for launchers that cannot cancel on
// the source (Cloud Hypervisor v53 and older), and as the source path's
// fallback.
func (r *SwiftMigrationReconciler) resolveOnDestination(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	dst *corev1.Pod,
	trigger abortTrigger,
) (abortResolution, error) {
	// Take this migration's send off the source first, so a launcher still
	// busy with an earlier send cannot take it up later, after the
	// destination is gone (clearSourceSend).
	if err := r.clearSourceSend(ctx, mig); err != nil {
		return abortResolution{}, fmt.Errorf("clear send action on source pod: %w", err)
	}
	cid := cancelID(mig)
	if !destinationCancelIssued(mig, dst) {
		patch := client.MergeFrom(dst.DeepCopy())
		if dst.Annotations == nil {
			dst.Annotations = map[string]string{}
		}
		dst.Annotations[AnnotationMigrationAction] = MigrationActionCancel
		dst.Annotations[AnnotationMigrationActionID] = cid
		dst.Annotations[AnnotationMigrationCancelIssuedAt] = time.Now().UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, dst, patch); err != nil {
			return abortResolution{}, fmt.Errorf("write cancel annotation on dst pod %q: %w", dst.Name, err)
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "CancelIssued",
				"%s: wrote cancel annotation on destination pod %q (id=%s); awaiting swiftletd ack",
				triggerText(trigger), dst.Name, cid)
		}
		return abortResolution{outcome: abortPending, path: cancelPathDestination, detail: phaseDetailCancelIssuing,
			requeue: ctrl.Result{RequeueAfter: cancelPollInterval}}, nil
	}

	ackObserved := dst.Annotations[AnnotationMigrationStatus] == MigrationStatusFailed &&
		dst.Annotations[AnnotationMigrationStatusID] == cid &&
		strings.Contains(strings.ToLower(dst.Annotations[AnnotationMigrationStatusDtl]), MigrationStatusFailedCancelDt)
	if ackObserved {
		return abortResolution{outcome: abortDestinationDead, path: cancelPathDestination,
			detail: "the receiving Cloud Hypervisor was stopped before it ran the guest"}, nil
	}
	// The budget runs from when the cancel was issued, recorded with it
	// (since v0.15.0; one written without it gets a fresh budget).
	issuedAt, perr := time.Parse(time.RFC3339, dst.Annotations[AnnotationMigrationCancelIssuedAt])
	if perr != nil {
		patch := client.MergeFrom(dst.DeepCopy())
		dst.Annotations[AnnotationMigrationCancelIssuedAt] = time.Now().UTC().Format(time.RFC3339)
		if err := r.Patch(ctx, dst, patch); err != nil {
			return abortResolution{}, fmt.Errorf("stamp cancel time on dst pod %q: %w", dst.Name, err)
		}
		return abortResolution{outcome: abortPending, path: cancelPathDestination, detail: phaseDetailCancelWaiting,
			requeue: ctrl.Result{RequeueAfter: cancelPollInterval}}, nil
	}
	if time.Since(issuedAt) > cancelAckTimeout {
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeWarning, "CancelAckTimeout",
				"swiftletd cancel ack not observed within %s; the outcome of the transfer is not known", cancelAckTimeout)
		}
		return abortResolution{outcome: abortUnresolved, path: cancelPathDestination,
			detail: fmt.Sprintf("swiftletd cancel ack timed out (%s budget)", cancelAckTimeout)}, nil
	}
	return abortResolution{outcome: abortPending, path: cancelPathDestination, detail: phaseDetailCancelWaiting,
		requeue: ctrl.Result{RequeueAfter: cancelPollInterval}}, nil
}

// deleteAbandonedDestination deletes the destination of a migration whose
// ownership resolveSourceOwnership has settled. Only an abortUnresolved
// outcome deletes without a grace period: the receiving Cloud Hypervisor
// did not acknowledge, and a graceful shutdown would let it finish receiving
// (the long-standing fallback, see cancelAckTimeout).
func (r *SwiftMigrationReconciler) deleteAbandonedDestination(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	res abortResolution,
) error {
	if !res.settled() {
		return fmt.Errorf("internal: destination cleanup before ownership was resolved (%s)", res.outcome)
	}
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		return client.IgnoreNotFound(err)
	}
	name, err := dstPodName(mig, guest.Name)
	if err != nil {
		return nil
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mig.Namespace}}
	var opts []client.DeleteOption
	if res.outcome == abortUnresolved {
		opts = append(opts, &client.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0),
			PropagationPolicy:  ptr.To(metav1.DeletePropagationBackground),
		})
	}
	if err := r.Delete(ctx, pod, opts...); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete destination pod %q: %w", name, err)
	}
	return nil
}

// recordAbort counts a finished abort (once, by the caller that finished it).
func recordAbort(trigger abortTrigger, res abortResolution) {
	path := res.path
	if path == "" {
		path = cancelPathNone
	}
	metrics.MigrationCancelTotal.WithLabelValues(string(trigger), path, res.outcome.String()).Inc()
}

func triggerText(t abortTrigger) string {
	switch t {
	case abortDeadline:
		return "spec.timeout expired"
	case abortDeletion:
		return "SwiftMigration deleted"
	case abortFailure:
		return "migration failed"
	default:
		return "cancel requested"
	}
}

// resolveBeforeFailing is dispatchResult's gate in front of a live
// pre-cutover failure: the destination is deleted by the failure's cleanup
// (onTerminalPhase), so the source's send is resolved first. handled=true
// means the failure must not be applied now: ownership is still being
// resolved, or the migration committed and is completing instead.
func (r *SwiftMigrationReconciler) resolveBeforeFailing(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
	result *phaseResult,
) (handled bool, res ctrl.Result, resolution abortResolution, err error) {
	if !isLiveMode(mig, status) || isPostCutover(mig) {
		return false, ctrl.Result{}, abortResolution{outcome: abortNoSend, path: cancelPathNone}, nil
	}
	trigger := abortTriggerFor(result)
	resolution, err = r.resolveSourceOwnership(ctx, mig, trigger)
	if err != nil {
		return true, ctrl.Result{}, resolution, err
	}
	switch resolution.outcome {
	case abortPending:
		setPhaseDetail(status, fmt.Sprintf("%s; %s", triggerText(trigger), resolution.detail))
		if perr := r.persist(ctx, mig, status); perr != nil {
			return true, ctrl.Result{}, resolution, perr
		}
		return true, resolution.requeue, resolution, nil
	case abortCommitted:
		// The transfer won: the destination holds the guest and cutover
		// completes it. A failure here would delete it.
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "AbortIgnored",
				"%s (%s), but the migration had already committed; completing it",
				triggerText(trigger), result.FailureMsg)
		}
		log.FromContext(ctx).Info("migration abort lost to completion", "trigger", trigger, "failure", result.FailureMsg)
		return true, ctrl.Result{RequeueAfter: cancelPollInterval}, resolution, nil
	}
	if r.Recorder != nil {
		switch resolution.outcome {
		case abortSourceRetained, abortDestinationDead:
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "TransferStopped",
				"%s: %s", triggerText(trigger), resolution.detail)
		case abortUnresolved:
			r.Recorder.Eventf(mig, corev1.EventTypeWarning, "TransferOutcomeUnknown",
				"%s: %s; destination deleted without a grace period", triggerText(trigger), resolution.detail)
		}
	}
	return false, ctrl.Result{}, resolution, nil
}

// abortTriggerFor is the trigger of a failure result.
func abortTriggerFor(result *phaseResult) abortTrigger {
	if result.FailureReason == migrationv1alpha1.FailureReasonTimeout {
		return abortDeadline
	}
	return abortFailure
}
