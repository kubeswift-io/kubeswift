package swiftmigration

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	migrationv1alpha1 "github.com/projectbeskar/kubeswift/api/migration/v1alpha1"
	swiftv1alpha1 "github.com/projectbeskar/kubeswift/api/swift/v1alpha1"
)

// stopAndCopyLivePollInterval is the requeue cadence while waiting for
// swiftletd-side acknowledgments (recv accepted, src complete). The
// labeled informer (§5.1) wakes the controller faster on actual
// annotation events; the periodic requeue is the safety net for
// missed events.
const stopAndCopyLivePollInterval = 2 * time.Second

// migrationListenPort is the cross-pod TCP port for the migration
// channel. In the plaintext path (mTLS off) it is the port CH itself
// binds/dials. In the Phase 3c mTLS path it is the port the stunnel
// SERVER sidecar binds on the destination pod (0.0.0.0:6789); CH then
// receives on the localhost plaintext port behind it.
const migrationListenPort = 6789

// migrationLocalPlaintextPort is the loopback port the destination CH
// receiver listens on when mTLS is enabled (Phase 3c). The dst stunnel
// server accepts cross-pod TLS on migrationListenPort and forwards
// decrypted bytes to 127.0.0.1:migrationLocalPlaintextPort, where CH's
// vm.receive-migration listens. Localhost-only — no plaintext leaves
// the pod. The SOURCE-side counterpart (CH sends to 127.0.0.1:6790 via
// the src stunnel client) lands in PR 3b.
const migrationLocalPlaintextPort = 6790

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
// is governed by spec.timeout (default 30m; webhook
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
	// GuestRAMMiB is the guest's RAM in MiB. swiftletd-source needs it to
	// compute the kubeswift.io/migration-progress-estimate annotation
	// (design §5.4); its emitter skips emission entirely when this is
	// absent. Maps to MigrationSendArgs.guest_ram_mib in
	// rust/swiftletd/src/action.rs. PR 2 shipped without populating this,
	// so the progress estimate never appeared in the controller-driven
	// path — wired here. Best-effort: a nil value (memory unresolvable)
	// just disables the estimate; it never fails the migration.
	GuestRAMMiB *uint32 `json:"guest_ram_mib,omitempty"`
	// DowntimeMs is the Cloud Hypervisor `downtime_ms` target for
	// vm.send-migration (CH >= v52): CH iterates pre-copy until the
	// estimated final stop-and-copy fits under this vCPU-pause budget,
	// then commits (classical convergence, superseding v51.1's hardcoded
	// 5-iteration cap). Derived from SwiftMigration.spec.downtimeTarget;
	// nil (operator did not set downtimeTarget) omits the field so CH
	// uses its native default. Maps to MigrationSendArgs.downtime_ms in
	// rust/swiftletd/src/action.rs -> swift_ch_client::send_migration.
	DowntimeMs *int64 `json:"downtime_ms,omitempty"`
	// Connections is the Cloud Hypervisor `connections` count for
	// vm.send-migration (CH >= v52): parallel TCP connections for the
	// memory stream. Derived from SwiftMigration.spec.parallelConnections;
	// nil (unset, or 1) omits the field so CH uses a single connection.
	// Maps to MigrationSendArgs.connections in
	// rust/swiftletd/src/action.rs -> swift_ch_client::send_migration.
	Connections *int32 `json:"connections,omitempty"`
}

// downtimeMs converts SwiftMigration.spec.downtimeTarget to a CH
// `downtime_ms` value (milliseconds). Returns nil when unset or
// non-positive so the send-args omit it and CH keeps its native
// behaviour (operator opt-in; defaulting is deferred pending the
// PR 1 convergence spike). Live-mode only.
func downtimeMs(mig *migrationv1alpha1.SwiftMigration) *int64 {
	if mig.Spec.DowntimeTarget == nil {
		return nil
	}
	ms := mig.Spec.DowntimeTarget.Milliseconds()
	if ms <= 0 {
		return nil
	}
	return &ms
}

// parallelConnections returns the CH `connections` count from
// SwiftMigration.spec.parallelConnections. Returns nil for 0 or 1 (a
// single connection is CH's default and needs no field) so the send-args
// omit it. Values >= 2 are passed through (the webhook caps at
// MaxParallelConnections). Live-mode only.
func parallelConnections(mig *migrationv1alpha1.SwiftMigration) *int32 {
	if mig.Spec.ParallelConnections < 2 {
		return nil
	}
	n := mig.Spec.ParallelConnections
	return &n
}

// guestRAMMiB returns the guest's RAM in MiB from its SwiftGuestClass,
// for the migration-progress-estimate heuristic. Returns nil (and never
// errors) when the class can't be read or memory is zero — the progress
// estimate is best-effort and must not gate the migration. SwiftGuest
// has no per-guest memory override, so the class value matches
// resolved.GetMemoryMiB() (resolved/merge.go) exactly. SwiftGuestClass
// is cluster-scoped (no namespace on the key).
func guestRAMMiB(ctx context.Context, r *SwiftMigrationReconciler, guest *swiftv1alpha1.SwiftGuest) *uint32 {
	var class swiftv1alpha1.SwiftGuestClass
	if err := r.Get(ctx, client.ObjectKey{Name: guest.Spec.GuestClassRef.Name}, &class); err != nil {
		return nil
	}
	miB := class.Spec.Memory.Value() / (1024 * 1024)
	if miB <= 0 {
		return nil
	}
	v := uint32(miB)
	return &v
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
// 30m; webhook minimum 60s for mode=live (B1).
//
// **Defensive guard**: assert isLiveMode at entry per architect Q1.
func (r *SwiftMigrationReconciler) handleStopAndCopyLive(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
) *phaseResult {
	if !isLiveMode(mig, status) {
		return phaseFailure(
			"internal: handleStopAndCopyLive invoked without live mode",
			migrationv1alpha1.FailureReasonOther,
		)
	}

	// spec.timeout enforcement (F4.3). Total-migration cap from
	// status.StartedAt; default 30m.
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

	// Resolve src pod by status.SourcePodRef.Name (locked in at
	// Validating-live).
	//
	// W15 + W26 background. Two prior single-mode fixes were tried and
	// both broke the other case:
	//
	//   - W15 fix (literal guest.Name): closes the cutoverStep1 race
	//     window where canonicalPodName resolves to dst-name before
	//     the SwiftMigration status persists. But fails on chain
	//     migrations: after a prior migration's cutover, the original
	//     src pod is named with the prior migration's dst-suffix, NOT
	//     guest.Name; literal guest.Name lookup hits NotFound and
	//     false-fires SourcePodReplaced.
	//   - canonicalPodName: works for chains (resolves to the prior
	//     dst-suffix = current src). But re-opens the W15 race AND
	//     in cutover.go::executeCutover post-step1 would resolve to
	//     THIS migration's dst pod and cutoverStep2 would delete the
	//     migrated guest.
	//
	// W26 fix: lock in the src pod name at Validating time
	// (status.SourcePodRef.Name), use it consistently here and in
	// cutover.go::executeCutover. Race-immune (locked at validation,
	// not derived from cluster state) AND chain-safe (the recorded
	// name is the actual src pod, regardless of how many prior
	// migrations the guest has been through).
	var srcPod corev1.Pod
	srcPodPresent := true
	srcName := srcPodLookupName(mig, &guest)
	if err := r.Get(ctx, client.ObjectKey{Name: srcName, Namespace: guest.Namespace}, &srcPod); err != nil {
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
		// Patch src pod with the migration-name label (architect F-3,
		// informer observability) AND the Phase 2 plaintext-ack
		// annotation (W13: swiftletd's decide() rejects send actions
		// on pods missing the ack with status=rejected; without this
		// patch the migration would stall at substateSendPending until
		// spec.timeout). Both writes are idempotent at the apiserver
		// layer (same-value MergeFrom is a no-op patch); skip-when-
		// present is a small optimisation that also makes leader-
		// handover behaviour observable in tests. dst pod gets the
		// ack at construction time in B2.2's mergeAnnotationsForDst;
		// src pod is the existing SwiftGuest launcher pod which
		// predates the migration, so the controller must add it.
		needLabelPatch := srcPodPresent && srcPod.Labels[LabelMigrationName] != mig.Name
		// Phase 3c cleanup: only stamp the plaintext-ack annotation on the
		// plaintext path. Under mTLS swiftletd bypasses the ack gate
		// (secured mode), so the patch is unnecessary and the annotation
		// would be misleading on a TLS-secured pod. Safe from version skew
		// (mTLS implies the Phase 3c swiftletd). The key is retained for the
		// plaintext path's THREAT-MODEL acknowledgement gate.
		needAckPatch := !r.MigrationMTLSEnabled && srcPodPresent &&
			srcPod.Annotations[AnnotationMigrationPhase2Ack] != AnnotationMigrationPhase2AckValue
		if needLabelPatch || needAckPatch {
			patch := client.MergeFrom(srcPod.DeepCopy())
			if srcPod.Labels == nil {
				srcPod.Labels = map[string]string{}
			}
			if srcPod.Annotations == nil {
				srcPod.Annotations = map[string]string{}
			}
			if needLabelPatch {
				srcPod.Labels[LabelMigrationName] = mig.Name
			}
			if needAckPatch {
				srcPod.Annotations[AnnotationMigrationPhase2Ack] = AnnotationMigrationPhase2AckValue
			}
			if err := r.Patch(ctx, &srcPod, patch); err != nil {
				return phaseTransient(fmt.Errorf("patch src pod (label + phase2 ack) at StopAndCopy entry: %w", err))
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
		// Phase 3c: when mTLS is enabled the dst CH receives on the
		// loopback plaintext port behind the stunnel server sidecar
		// (which terminates cross-pod TLS on migrationListenPort and
		// forwards here). swiftletd is handed a localhost URL and stays
		// TLS-unaware (design §5 opacity contract). With mTLS off, CH
		// binds the cross-pod port directly exactly as in Phase 3a/3b.
		listenURL := fmt.Sprintf("tcp:0.0.0.0:%d", migrationListenPort)
		if r.MigrationMTLSEnabled {
			listenURL = fmt.Sprintf("tcp:127.0.0.1:%d", migrationLocalPlaintextPort)
		}
		args := migrationReceiveArgs{
			ListenURL:      listenURL,
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
				fmt.Sprintf("source pod %q disappeared between pre-recv and pre-send", guest.Name),
				migrationv1alpha1.FailureReasonSourcePodReplaced)
		}
		if dstPod.Status.PodIP == "" {
			// Pod is running but kubelet hasn't populated PodIP yet
			// (rare; would have blocked Preparing-live's Ready check).
			// Requeue briefly.
			return phaseRequeue(stopAndCopyLivePollInterval)
		}
		// Phase 3c: when mTLS is enabled the src CH sends to the LOCAL
		// stunnel client (127.0.0.1:6790), which tunnels over TLS to the
		// dst sidecar; swiftletd stays TLS-unaware (design §5). With mTLS
		// off, CH dials the dst pod IP directly exactly as in Phase 3a/3b.
		targetURL := fmt.Sprintf("tcp:%s:%d", dstPod.Status.PodIP, migrationListenPort)
		if r.MigrationMTLSEnabled {
			targetURL = fmt.Sprintf("tcp:127.0.0.1:%d", migrationLocalPlaintextPort)
		}
		dtMs := downtimeMs(mig)
		args := migrationSendArgs{
			TargetURL:      targetURL,
			TimeoutSeconds: migrationActionTimeoutSeconds,
			GuestRAMMiB:    guestRAMMiB(ctx, r, &guest),
			DowntimeMs:     dtMs,
			Connections:    parallelConnections(mig),
		}
		// Echo the downtime_ms ceiling we actually sent into status so
		// operators can see the bound that governed this migration.
		// AppliedDowntimeMs is a BOUND, not a measurement — CH v52 does
		// not report the achieved vCPU-stop (see the field docstring).
		status.AppliedDowntimeMs = dtMs
		argsJSON, err := json.Marshal(args)
		if err != nil {
			return phaseFailure(fmt.Sprintf("marshal send args: %v", err), migrationv1alpha1.FailureReasonOther)
		}
		if err := r.writeMigrationAction(ctx, &srcPod, migrationActionVerbSend, sendActionID(mig), string(argsJSON)); err != nil {
			return phaseTransient(fmt.Errorf("write send-action on src pod: %w", err))
		}
		// Persist the SAME attempt number sendActionID just used (NOT a
		// hard-coded 1) so the mTLS retry counter is preserved across
		// reconciles: a retry bumps mig.Status.SendAttempts at
		// substateSrcFailed, and re-entering substatePreSend with the new
		// id must not reset it.
		status.SendAttempts = sendAttemptNumber(mig)
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveIssuingSend)
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy,
			"issuing send action on source pod")
		if r.Recorder != nil {
			r.Recorder.Eventf(mig, corev1.EventTypeNormal, "SendIssued",
				"wrote send-action on src pod %q (id=%s)", guest.Name, sendActionID(mig))
		}
		return phaseRequeue(stopAndCopyLivePollInterval)

	case substateSendPending:
		setPhaseDetail(status, migrationv1alpha1.PhaseDetailLiveTransferring)
		setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy,
			"transferring guest state from source to destination")
		// Phase 5: surface the swiftletd pre-copy progress estimate so
		// operators see movement on `kubectl get swiftmigration` instead of a
		// static "transferring" for the whole send window.
		stampTransferProgress(status, srcArg)
		return phaseRequeue(stopAndCopyLivePollInterval)

	case substateSrcCompleted:
		// W1 gate per F1.2 satisfied: src wrote migration-status=
		// complete with matching $SEND_ID. swiftletd-on-src's
		// vm.send-migration internally probed the dst CH for
		// vm_info=Running before writing complete, so observing
		// src=complete implies dst CH is in Running state with the
		// migrated guest.
		//
		// W27b: read swiftletd-reported pause window from launcher pod
		// annotation. swiftletd-on-src writes
		// kubeswift.io/migration-pause-window-ms alongside
		// migration-status=complete (rust/swiftletd/src/action.rs::
		// write_migration_status). Mirrors the snapshot pattern at
		// internal/controller/swiftsnapshot/local.go:249-251. Idempotent
		// (same observed value on every reconcile until cutover advances).
		stampTransferDuration(ctx, mig, status, srcArg)
		// Phase 5: the source reported transfer complete — pin progress to 100
		// (the bandwidth estimate caps below 100 since it excludes the final
		// stop-and-copy; completion is the unambiguous 100%).
		hundred := int32(100)
		status.TransferProgress = &hundred
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
		// Phase 3d — bounded retry for the mTLS migration-channel readiness
		// races. With mTLS on, target_url is the LOCAL stunnel client
		// (127.0.0.1:6790). Two "not-ready-yet" failures leave the channel
		// never established, so NO migration data flowed and re-issuing the
		// send is safe (bumping SendAttempts yields a fresh $SEND_ID that
		// re-drives substatePreSend on the next reconcile):
		//   - SOURCE sidecar not ready: CH couldn't connect to the local
		//     stunnel (it hasn't started yet) -> "connection_refused".
		//   - DESTINATION receiver not ready: the local stunnel is up but the
		//     dst CH hasn't called vm.receive-migration yet, so the dst
		//     stunnel resets the forward and CH sees a generic Status-500 ->
		//     "internal_server_error". This is the primary-on-NAD race (the
		//     NAD dst pod's network-init runs the multi-node-L2 datapath and
		//     is slower to reach receive-migration; non-NAD dst pods win the
		//     race). Found in the multi-node-L2 validation spike.
		// Bounded by maxMTLSSendRetries; spec.timeout is the hard cap. BOTH
		// are gated on dst NOT terminating, so a genuine dst-gone failure
		// (transport_error after data flowed, or W18 dst-K8s-termination which
		// shares the internal_server_error symptom but is going away) is never
		// mistaken for a not-ready-yet race.
		srcNotReady := isLocalStunnelNotReady(detail)
		dstNotReady := isDestReceiverNotReady(detail)
		if r.MigrationMTLSEnabled && (srcNotReady || dstNotReady) &&
			dstPod.DeletionTimestamp == nil && sendAttemptNumber(mig) < maxMTLSSendRetries {
			status.SendAttempts = sendAttemptNumber(mig) + 1
			detailMsg := "source mTLS sidecar not ready; retrying send"
			condMsg := "waiting for source mTLS sidecar to become ready; retrying send"
			eventMsg := "source mTLS sidecar not yet listening (attempt %d/%d); re-issuing send"
			if dstNotReady && !srcNotReady {
				detailMsg = "destination receiver not ready; retrying send"
				condMsg = "waiting for destination CH receiver to bind its listener; retrying send"
				eventMsg = "destination receiver not yet listening (attempt %d/%d); re-issuing send"
			}
			setPhaseDetail(status, detailMsg)
			setReadyCondition(status, metav1.ConditionFalse, ReasonStopAndCopy, condMsg)
			if r.Recorder != nil {
				r.Recorder.Eventf(mig, corev1.EventTypeNormal, "SendRetry",
					eventMsg, sendAttemptNumber(mig), maxMTLSSendRetries)
			}
			return phaseRequeue(stopAndCopyLivePollInterval)
		}
		// W18 (PR #46 Scenario 4): when dst pod is being K8s-
		// terminated mid-StopAndCopy, src CH's vm.send-migration
		// errors out with a generic CH error (e.g.,
		// "send_migration: internal_server_error") because the TCP
		// peer disappeared. The src-side detail string by itself
		// can't distinguish "dst terminated" from "any other src
		// failure", so classifyFailureFromDetail defaults to Other.
		// §4.7 design intent: dst-K8s-termination should map to
		// PodTerminated. Use dst pod state to disambiguate: if
		// dst has DeletionTimestamp set (graceful termination in
		// progress), override the classification.
		reason := classifyFailureFromDetail(detail)
		if dstPod.DeletionTimestamp != nil {
			reason = migrationv1alpha1.FailureReasonPodTerminated
		}
		return phaseFailure(
			fmt.Sprintf("source reported migration failure: %s", normalizeStatusDetail(detail)),
			reason)

	case substateDstRejected:
		// W14: dst's swiftletd refused the receive-action via decide().
		// Detail carries the rejection reason (e.g.,
		// phase2_plaintext_ack_missing, action-id mismatch). Surface as
		// FailureReasonOther with the detail preserved — operator-
		// readable failureMessage is the load-bearing artifact for
		// diagnosis since rejection reasons are open-ended.
		detail := dstPod.Annotations[AnnotationMigrationStatusDtl]
		return phaseFailure(
			fmt.Sprintf("destination rejected migration action: %s", normalizeStatusDetail(detail)),
			migrationv1alpha1.FailureReasonOther)

	case substateSrcRejected:
		// W14: src's swiftletd refused the send-action via decide().
		// See substateDstRejected for rationale.
		detail := ""
		if srcArg != nil {
			detail = srcArg.Annotations[AnnotationMigrationStatusDtl]
		}
		return phaseFailure(
			fmt.Sprintf("source rejected migration action: %s", normalizeStatusDetail(detail)),
			migrationv1alpha1.FailureReasonOther)

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

// stampTransferDuration reads the swiftletd-on-src reported
// vm.send-migration RPC duration from the src pod's
// kubeswift.io/migration-pause-window-ms annotation and writes it into
// status.ObservedTransferDuration. (The deprecated
// status.ObservedPauseWindow alias was removed in the CH-v52
// observability work — operator tooling reads ObservedTransferDuration.)
// swiftletd writes the annotation alongside
// migration-status=complete via write_migration_status
// (rust/swiftletd/src/action.rs:1383-1407). The value reflects the
// CH-internal pause-and-send window, NOT the controller-orchestration
// downtime (that's status.ObservedDowntime, anchored on
// CutoverStep2DispatchedAt per W27a).
//
// Wire annotation NOT renamed in Phase 3b PR 1: the
// kubeswift.io/migration-pause-window-ms key stays as-is on the
// swiftletd↔controller wire to avoid breaking the existing
// annotation contract. The CRD-field rename is independent; PR 2
// or later cleanup may align the wire annotation key.
//
// Idempotent: every reconcile in substateSrcCompleted re-reads and
// re-stamps the same value until cutover advances state. Mirrors the
// snapshot controller's pattern at
// internal/controller/swiftsnapshot/local.go:249-251.
//
// Defensive parse failure handling: malformed annotation (manual
// operator tampering, apiserver quirk, swiftletd version skew without
// the field) is logged and BOTH fields stay at their prior values
// (typically nil). Operators see a missing field, not a wrong one.
// W27b acceptance criterion preserved.
func stampTransferDuration(
	ctx context.Context,
	mig *migrationv1alpha1.SwiftMigration,
	status *migrationv1alpha1.SwiftMigrationStatus,
	srcPod *corev1.Pod,
) {
	if srcPod == nil {
		return
	}
	v := srcPod.Annotations[AnnotationMigrationPauseWindowMs]
	if v == "" {
		return
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms < 0 {
		log.FromContext(ctx).Info(
			"observedTransferDuration not stamped: src pod annotation unparseable (W27b defensive)",
			"migration", mig.Name,
			"annotation", AnnotationMigrationPauseWindowMs,
			"value", v,
			"parseError", err)
		return
	}
	d := metav1.Duration{Duration: time.Duration(ms) * time.Millisecond}
	status.ObservedTransferDuration = &d
}

// stampTransferProgress surfaces the swiftletd-on-src
// migration-progress-estimate annotation (an integer percentage) as
// status.TransferProgress (Phase 5). Best-effort: a missing or unparseable
// annotation leaves the field unchanged; the value is clamped to [0,100].
func stampTransferProgress(status *migrationv1alpha1.SwiftMigrationStatus, srcPod *corev1.Pod) {
	if srcPod == nil {
		return
	}
	v := srcPod.Annotations[AnnotationMigrationProgressEstimate]
	if v == "" {
		return
	}
	pct, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	p := int32(pct)
	status.TransferProgress = &p
}

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
