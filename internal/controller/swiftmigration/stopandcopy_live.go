package swiftmigration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// stopAndCopyLivePollInterval is the requeue cadence while waiting for
// swiftletd-side acknowledgments (recv accepted, src complete). The
// labeled informer (§5.1) wakes the controller faster on actual
// annotation events; the periodic requeue is the safety net for
// missed events.
const stopAndCopyLivePollInterval = 2 * time.Second

// migrationListenPort is the TCP port both src and dst CH use for
// the migration channel. Phase 2 manual demo uses 6789
// (docs/design/live-migration-phase-2.md §2 example flow); Phase 3a
// PR 1 keeps the same fixed port. mTLS sidecar (Phase 3b) may move
// the port to localhost-proxy semantics — the design note in Phase 2
// §spike "Decision 2" calls this out, so callers should NOT bake-in
// assumptions about whether the port is direct CH or via a proxy.
const migrationListenPort = 6789

// migrationActionTimeoutSeconds is the per-action HTTP timeout the
// controller passes to swiftletd via migration-action-args. Phase 2
// PR-B's DEFAULT_ACTION_TIMEOUT_SECS handles this when the args
// field is absent; we set it explicitly for predictability and to
// give operators a knob via spec.timeout (Phase 3b will derive a
// per-VM-memory-size value from spec.timeout).
//
// 600s covers typical workloads (Phase 2 spike Q2: LOW dirty-rate
// ~19s, HIGH dirty-rate ~37s for 1 GiB guest; large guests scale
// roughly linearly with memory). The migration's overall timeout
// is governed by spec.timeout (default 5min per F3.5; webhook
// minimum 60s for mode=live).
const migrationActionTimeoutSeconds = 600

// migrationReceiveArgs is the JSON shape swiftletd-on-dst's
// MigrationReceiveArgs deserializes (rust/swiftletd/src/action.rs).
// Field names match the Rust serde derive; mismatches surface as
// rejected actions.
type migrationReceiveArgs struct {
	ListenURL      string `json:"listen_url"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	GuestIP        string `json:"guest_ip,omitempty"`
}

// migrationSendArgs is the JSON shape swiftletd-on-src's
// MigrationSendArgs deserializes.
type migrationSendArgs struct {
	TargetURL      string `json:"target_url"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// handleStopAndCopyLive implements the live-mode StopAndCopy phase
// per design §2.3 (sub-states) and §3.5 (cutover ordering invariant).
//
// **B3.1 SCOPE**: 6-substate state machine through src-completed.
// The cutover sub-state (§3.5 3-step sequence) is intentionally
// stubbed: when substateSrcCompleted is observed, the handler sets
// phaseDetail to PhaseDetailLiveSrcCompleted and returns
// phaseRequeue. **B3.2 lands the cutover sequence**.
//
// **State-machine pattern**: each reconcile reads observable cluster
// state (src/dst pod annotations + SwiftMigration counters) via
// deriveSubstate to determine the current sub-state, then drives the
// next action. No in-memory state survives reconcile. Per §2.4
// reconcile-loop interruption recovery, leader-handover at any
// sub-state boundary is handled by re-derivation on the new leader's
// next reconcile.
//
// **Sub-state actions**:
//
//   - substatePreRecv: write migration-action=receive on dst pod with
//     $RECV_ID and the JSON args (listen_url, guest_ip from src
//     annotation). Increment status.RecvAttempts to 1. Set
//     phaseDetail=IssuingRecv. Requeue.
//   - substateRecvPending: phaseDetail=DestReceiving. Requeue.
//   - substatePreSend: write migration-action=send on src pod with
//     $SEND_ID and JSON args (target_url=tcp:<dst-pod-ip>:6789).
//     Increment status.SendAttempts to 1. Set phaseDetail=IssuingSend.
//     Requeue.
//   - substateSendPending: phaseDetail=Transferring. Requeue.
//   - substateSrcCompleted: W1 gate per F1.2 satisfied. Set
//     phaseDetail=SrcCompleted. **B3.1 parks here**; B3.2 will
//     replace this branch with the cutover sequence.
//   - substateDstFailed: dst reported failed → Failed phase with
//     FailureReason from §4 mapping (Other for now; B3.3 refines).
//   - substateSrcFailed: src reported failed → Failed phase with
//     FailureReason from §4 mapping (Other for now; B3.3 refines).
//
// **Failure handling note for B3.1**: dst/src failure detection
// uses FailureReasonOther for the general case. B3.3 adds the
// detail-string parsing that maps to FailureReasonPodTerminated
// (D2 watchdog detail) and refines the failureReason taxonomy per
// §4.7.
//
// **F4.2 source-pod-replacement detection (UID check)**: gated by
// shouldCheckSourcePodUID per §4.2 detection scope. Returns true
// during pre-cutover sub-states (preRecv through src-completed
// because PodRefSwapped is False); B3.2's cutover transitions will
// flip the gate by stamping PodRefSwapped/cutoverStep1At.
//
// **spec.timeout enforcement (F4.3)**: every reconcile compares
// elapsed time since status.StartedAt against spec.timeout. Default
// 5min per F3.5; webhook minimum 60s for mode=live (B1).
//
// **Defensive guard**: assert isLiveMode at entry per architect Q1.
func (r *SwiftMigrationReconciler) handleStopAndCopyLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig) {
		return phaseFailure(
			"internal: handleStopAndCopyLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}

	// spec.timeout enforcement (F4.3). Total-migration cap from
	// status.StartedAt; default 5min per F3.5.
	if mig.Spec.Timeout != nil && mig.Spec.Timeout.Duration > 0 && status.StartedAt != nil {
		if time.Since(status.StartedAt.Time) > mig.Spec.Timeout.Duration {
			return phaseFailure(
				fmt.Sprintf("spec.timeout=%s exceeded since StartedAt; migration did not complete in time", mig.Spec.Timeout.Duration),
				migrationv1alpha1.FailureReasonTimeout)
		}
	}

	// Resolve source guest.
	var guest swiftv1alpha1.SwiftGuest
	if err := r.Get(ctx, client.ObjectKey{Name: mig.Spec.GuestRef.Name, Namespace: mig.Namespace}, &guest); err != nil {
		if apierrors.IsNotFound(err) {
			return phaseFailure(
				fmt.Sprintf("source SwiftGuest %q deleted during StopAndCopy", mig.Spec.GuestRef.Name),
				migrationv1alpha1.FailureReasonOther)
		}
		return phaseTransient(fmt.Errorf("get source guest: %w", err))
	}

	// Resolve src pod (the canonical pod pre-cutover; B2.3's helper
	// returns guest.Name when status.podRef is nil/empty, which is
	// the correct value here since B2.4's cutover hasn't run yet).
	srcPodName := canonicalPodNameForGuest(&guest)
	var srcPod corev1.Pod
	srcPodPresent := true
	if err := r.Get(ctx, client.ObjectKey{Name: srcPodName, Namespace: guest.Namespace}, &srcPod); err != nil {
		if apierrors.IsNotFound(err) {
			srcPodPresent = false
		} else {
			return phaseTransient(fmt.Errorf("get source pod: %w", err))
		}
	}

	// F4.2 source-pod-replacement detection (UID check). Gated by
	// shouldCheckSourcePodUID. Pre-cutover phases ALWAYS check;
	// during cutover sub-states the gate flips off (B3.2 territory).
	if shouldCheckSourcePodUID(mig) && status.SourcePodUID != "" {
		if !srcPodPresent {
			return phaseFailure(
				fmt.Sprintf("source pod for SwiftGuest %q no longer exists during StopAndCopy", guest.Name),
				migrationv1alpha1.FailureReasonSourcePodReplaced)
		}
		if srcPod.UID != status.SourcePodUID {
			return phaseFailure(
				fmt.Sprintf("source pod for SwiftGuest %q was replaced (UID changed from %q to %q)", guest.Name, status.SourcePodUID, srcPod.UID),
				migrationv1alpha1.FailureReasonSourcePodReplaced)
		}
	}

	// Resolve dst pod by deterministic name (B2.2's helper). Pre-
	// cutover, status.podRef still points at src so canonicalPodName
	// won't return the dst name; we derive it directly.
	dstName, derr := dstPodName(mig, guest.Name)
	if derr != nil {
		return phaseFailure(derr.Error(), migrationv1alpha1.FailureReasonOther)
	}
	var dstPod corev1.Pod
	dstPodPresent := true
	if err := r.Get(ctx, client.ObjectKey{Name: dstName, Namespace: mig.Namespace}, &dstPod); err != nil {
		if apierrors.IsNotFound(err) {
			dstPodPresent = false
		} else {
			return phaseTransient(fmt.Errorf("get destination pod: %w", err))
		}
	}

	// Dst pod must exist throughout StopAndCopy (Preparing-live
	// created it and waited for Ready; if it disappears mid-
	// StopAndCopy, the migration cannot proceed and the failure
	// reason is PodTerminated per §4.7).
	if !dstPodPresent {
		return phaseFailure(
			fmt.Sprintf("destination pod %q disappeared during StopAndCopy", dstName),
			migrationv1alpha1.FailureReasonPodTerminated)
	}

	// Pass nil to deriveSubstate when src pod is absent (helper
	// short-circuits the src-side checks; observed substate falls
	// through to the dst-side checks).
	var srcArg, dstArg *corev1.Pod
	dstArg = &dstPod
	if srcPodPresent {
		srcArg = &srcPod
	}

	// Cutover-in-progress short-circuit: if SwiftGuest.status.podRef.name
	// already equals the dst pod name, cutover step 1 has succeeded.
	// Subsequent reconciles dispatch directly to the cutover handler
	// (which derives the within-cutover step from cluster state).
	// Without this short-circuit, deriveSubstate falls through to
	// substatePreRecv when src pod is gone (post-step-2), which would
	// mis-route to writing a fresh receive-action.
	//
	// Equivalent: we're in cutover whenever PodRefSwapped is True per
	// B1's isPostCutover() helper; here we read directly from
	// guest.status.podRef so the check works whether or not the
	// caller has already loaded the SwiftMigration's Conditions.
	if guest.Status.PodRef != nil && guest.Status.PodRef.Name == dstName {
		return r.executeCutover(ctx, mig, status, &guest, srcArg, dstName)
	}

	sub := deriveSubstate(mig, srcArg, dstArg)

	switch sub {
	case substatePreRecv:
		// Patch src pod with migration-name label per architect F-3:
		// makes src observable via the same labeled-pod watch the
		// manager registers for dst. Idempotent — skips patch if the
		// label is already present (leader-handover safe). Without
		// this, controller observability of src migration-status
		// transitions falls back to the 30s SyncPeriod (defense-in-
		// depth), adding up to 30s latency to state advances.
		if srcPodPresent && srcPod.Labels[LabelMigrationName] != mig.Name {
			labelPatch := client.MergeFrom(srcPod.DeepCopy())
			if srcPod.Labels == nil {
				srcPod.Labels = map[string]string{}
			}
			srcPod.Labels[LabelMigrationName] = mig.Name
			if err := r.Patch(ctx, &srcPod, labelPatch); err != nil {
				return phaseTransient(fmt.Errorf("label src pod for informer observability: %w", err))
			}
		}

		// Write the receive-action on the dst pod. Increment
		// RecvAttempts to 1 (in-memory; persisted by dispatchResult).
		// Per F1.8 the counter starts at 0 and is incremented to 1
		// at issue-time; PR 1 never retries, so 1 is the terminal
		// value.
		guestIP := ""
		if srcArg != nil {
			guestIP = srcArg.Annotations[AnnotationGuestIP]
		}
		args := migrationReceiveArgs{
			ListenURL:      fmt.Sprintf("tcp:0.0.0.0:%d", migrationListenPort),
			TimeoutSeconds: migrationActionTimeoutSeconds,
			GuestIP:        guestIP,
		}
		argsJSON, err := json.Marshal(args)
		if err != nil {
			return phaseFailure(fmt.Sprintf("marshal receive args: %v", err), migrationv1alpha1.FailureReasonOther)
		}
		if err := r.writeMigrationAction(ctx, &dstPod, migrationActionVerbReceive, recvActionID(mig), string(argsJSON)); err != nil {
			return phaseTransient(fmt.Errorf("write receive-action on dst pod: %w", err))
		}
		status.RecvAttempts = 1
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveIssuingRecv)
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy,
			"issuing receive action on destination pod")
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "ReceiveIssued",
				"wrote receive-action on dst pod %q (id=%s)", dstName, recvActionID(mig))
		}
		return phaseRequeue(stopAndCopyLivePollInterval)

	case substateRecvPending:
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveIssuingRecv)
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy,
			"awaiting destination receive acknowledgment")
		return phaseRequeue(stopAndCopyLivePollInterval)

	case substatePreSend:
		// dst accepted receive (migration-status=running with
		// matching $RECV_ID). Update phaseDetail then write the
		// send-action on the src pod.
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveDestReceiving)

		if !srcPodPresent {
			return phaseFailure(
				fmt.Sprintf("source pod %q disappeared between pre-recv and pre-send", srcPodName),
				migrationv1alpha1.FailureReasonSourcePodReplaced)
		}
		if dstPod.Status.PodIP == "" {
			// Pod is running but kubelet hasn't populated PodIP yet
			// (rare; would have blocked Preparing-live's Ready check).
			// Requeue briefly.
			return phaseRequeue(stopAndCopyLivePollInterval)
		}
		args := migrationSendArgs{
			TargetURL:      fmt.Sprintf("tcp:%s:%d", dstPod.Status.PodIP, migrationListenPort),
			TimeoutSeconds: migrationActionTimeoutSeconds,
		}
		argsJSON, err := json.Marshal(args)
		if err != nil {
			return phaseFailure(fmt.Sprintf("marshal send args: %v", err), migrationv1alpha1.FailureReasonOther)
		}
		if err := r.writeMigrationAction(ctx, &srcPod, migrationActionVerbSend, sendActionID(mig), string(argsJSON)); err != nil {
			return phaseTransient(fmt.Errorf("write send-action on src pod: %w", err))
		}
		status.SendAttempts = 1
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveIssuingSend)
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy,
			"issuing send action on source pod")
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "SendIssued",
				"wrote send-action on src pod %q (id=%s)", srcPodName, sendActionID(mig))
		}
		return phaseRequeue(stopAndCopyLivePollInterval)

	case substateSendPending:
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveTransferring)
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy,
			"transferring guest state from source to destination")
		return phaseRequeue(stopAndCopyLivePollInterval)

	case substateSrcCompleted:
		// W1 gate per F1.2 satisfied: src wrote migration-status=
		// complete with matching $SEND_ID. swiftletd-on-src's
		// vm.send-migration internally probed the dst CH for
		// vm_info=Running before writing complete, so observing
		// src=complete implies dst CH is in Running state with the
		// migrated guest.
		//
		// **B3.2 CUTOVER**: dispatch to the 3-step cutover sequence
		// per §2.3 + §3.5 cutover ordering invariant. Each reconcile
		// in the cutover handler executes ONLY the pending step;
		// next reconcile reads cluster state and proceeds. Forward-
		// only retry-in-place; no rollback.
		return r.executeCutover(ctx, mig, status, &guest, srcArg, dstName)

	case substateDstFailed:
		// dst reported migration-status=failed with matching $RECV_ID.
		// B3.3: classify via classifyFailureFromDetail (D2 watchdog
		// detail "destination listener exited abnormally: ..." maps to
		// PodTerminated; W1 violation maps to Other; "cancelled" maps
		// to Cancelled; default Other).
		detail := dstPod.Annotations[AnnotationMigrationStatusDtl]
		return phaseFailure(
			fmt.Sprintf("destination reported migration failure: %s", normalizeStatusDetail(detail)),
			classifyFailureFromDetail(detail))

	case substateSrcFailed:
		detail := ""
		if srcArg != nil {
			detail = srcArg.Annotations[AnnotationMigrationStatusDtl]
		}
		return phaseFailure(
			fmt.Sprintf("source reported migration failure: %s", normalizeStatusDetail(detail)),
			classifyFailureFromDetail(detail))

	default:
		// deriveSubstate is exhaustive; this branch is unreachable
		// unless a future sub-state is added without updating the
		// dispatch. Default-to-explicit per PR #26: fail visibly.
		return phaseFailure(
			fmt.Sprintf("internal: unhandled StopAndCopy sub-state %q", sub),
			migrationv1alpha1.FailureReasonOther)
	}
}

// writeMigrationAction sets migration-action, migration-action-id,
// and migration-action-args annotations on a pod via single
// client.MergeFrom patch. The three keys travel together — splitting
// would let swiftletd observe an action without its idempotency ID
// or required args, which Phase 2 PR-B's decide() would reject.
//
// Idempotency: re-writing the same action+id is a no-op patch
// (apiserver-side). Re-writing with a different id is treated as a
// new dispatch by swiftletd's per-id idempotency layer.
func (r *SwiftMigrationReconciler) writeMigrationAction(
	ctx context.Context,
	pod *corev1.Pod,
	action, id, argsJSON string,
) error {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationMigrationAction] = action
	pod.Annotations[AnnotationMigrationActionID] = id
	pod.Annotations[AnnotationMigrationActionArgs] = argsJSON
	return r.Patch(ctx, pod, patch)
}

// AnnotationMigrationActionArgs carries the JSON-encoded action
// arguments (listen_url + guest_ip for receive; target_url for send).
// Mirrors rust/swiftletd/src/action.rs::MIGRATION_ACTION_ARGS_KEY.
const AnnotationMigrationActionArgs = "kubeswift.io/migration-action-args"

// normalizeStatusDetail returns a single-line, length-bounded variant
// of the swiftletd-written status-detail annotation for inclusion in
// SwiftMigration.status.failureMessage. Long or multi-line details
// blow up `kubectl describe` output and event logs.
func normalizeStatusDetail(s string) string {
	if s == "" {
		return "(no detail)"
	}
	// Collapse newlines and tabs to spaces.
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	const max = 256
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
