package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SwiftMigrationMode selects the migration strategy. Phase 1 ships offline
// only; live is reserved for Phase 3 and the validation webhook rejects it
// with a clear error. auto is forward-compatible — Phase 1 always resolves
// auto to offline (recorded in status.mode), and a Phase 3+ controller will
// resolve auto to live where the guest's capabilities allow.
// +kubebuilder:validation:Enum=auto;live;offline
type SwiftMigrationMode string

const (
	SwiftMigrationModeAuto    SwiftMigrationMode = "auto"
	SwiftMigrationModeLive    SwiftMigrationMode = "live"
	SwiftMigrationModeOffline SwiftMigrationMode = "offline"
)

// PhaseLiveMigrationNotShipped is the rejection reason used by the Phase 1
// validation webhook when an operator submits mode: live. Centralised here
// so Phase 3 reviewers find the gate by searching for this constant.
const PhaseLiveMigrationNotShipped = "LiveMigrationNotShipped"

// SwiftMigrationPhase is the lifecycle phase of a SwiftMigration. Phase 1
// implements all of these (the offline path uses every phase except a future
// PreCopy that lives between Preparing and StopAndCopy in live mode).
// Existing phase consumers must treat unknown phases as opaque to keep
// forward compatibility with Phase 3's PreCopy addition.
// +kubebuilder:validation:Enum=Pending;Validating;Preparing;StopAndCopy;Resuming;Completed;Failed;Cancelled
type SwiftMigrationPhase string

const (
	SwiftMigrationPhasePending     SwiftMigrationPhase = "Pending"
	SwiftMigrationPhaseValidating  SwiftMigrationPhase = "Validating"
	SwiftMigrationPhasePreparing   SwiftMigrationPhase = "Preparing"
	SwiftMigrationPhaseStopAndCopy SwiftMigrationPhase = "StopAndCopy"
	SwiftMigrationPhaseResuming    SwiftMigrationPhase = "Resuming"
	SwiftMigrationPhaseCompleted   SwiftMigrationPhase = "Completed"
	SwiftMigrationPhaseFailed      SwiftMigrationPhase = "Failed"
	SwiftMigrationPhaseCancelled   SwiftMigrationPhase = "Cancelled"
)

// SwiftMigrationTimeoutStrategy controls behavior when a migration exceeds
// spec.timeout. Phase 1 supports cancel only; ignore is reserved for live
// mode (Phase 3) where it controls the pre-copy convergence trade-off.
// +kubebuilder:validation:Enum=cancel;ignore
type SwiftMigrationTimeoutStrategy string

const (
	SwiftMigrationTimeoutStrategyCancel SwiftMigrationTimeoutStrategy = "cancel"
	SwiftMigrationTimeoutStrategyIgnore SwiftMigrationTimeoutStrategy = "ignore"
)

// Standard condition types exposed by SwiftMigration.
const (
	// ConditionReady is True when the migration has reached Completed.
	// GitOps tooling (Flux, Argo) checks this.
	SwiftMigrationConditionReady = "Ready"
	// ConditionCompatible is True when the Validating phase has determined
	// the migration is feasible. Set False with reason on validation failure.
	SwiftMigrationConditionCompatible = "Compatible"
	// ConditionIPWillChange is True when the operator opted into spec.allowIPChange
	// AND the guest is on default node-local networking AND source != target.
	// Set as a warning so operators can spot it in `kubectl describe`.
	SwiftMigrationConditionIPWillChange = "IPWillChange"
	// ConditionPodRefSwapped is True after the live-mode cutover step 1
	// (SwiftGuest.status.podRef.name patched to dst pod name) succeeds.
	// The lastTransitionTime field captures WHEN the swap landed,
	// which is operationally useful when debugging a CancelIgnored
	// outcome (operator cancel arriving after this point cannot
	// reverse the cutover). The isPostCutover helper checks for
	// PodRefSwapped=True to gate cancel honoring per
	// docs/design/live-migration-phase-3a.md §5.3. Live mode only;
	// offline mode does not set this condition.
	SwiftMigrationConditionPodRefSwapped = "PodRefSwapped"
	// ConditionCancelIgnored is True when the operator set
	// spec.cancelRequested but the migration was already past the
	// cutover boundary (PodRefSwapped=True). The migration completes
	// normally; this condition preserves the operator's cancel intent
	// in the audit trail without misrepresenting cluster state as
	// Cancelled (the migration succeeded, the dst pod is canonical,
	// the src is gone).
	SwiftMigrationConditionCancelIgnored = "CancelIgnored"
)

// Standard condition reasons for live-mode conditions.
const (
	// ReasonCutoverStep1Complete is the Reason value for the
	// PodRefSwapped condition at the moment cutover step 1 succeeds.
	ReasonCutoverStep1Complete = "CutoverStep1Complete"
	// ReasonPastCutover is the Reason value for the CancelIgnored
	// condition: the cancel arrived too late to reverse the migration.
	ReasonPastCutover = "PastCutover"
)

// FailureReason values for status.failureReason. Centralised so
// controller code, tests, and operator dashboards reference the same
// string. The CRD's kubebuilder enum validation pins these at admission
// time; mismatched values would be rejected.
//
// Phase 3a (offline + live state machine) shipped the first five codes.
// Phase 3b PR 2 extends the taxonomy with seven additional codes that
// classify live-mode failures more precisely; see the per-code docstrings
// for fire conditions and the swiftletd-side / controller-side origin.
// FailureReasonCode is the typed code stored in status.failureReason.
// Typing it (vs a bare string) makes typos compile-fail. The string VALUES are
// unchanged, so this is wire/CRD-compatible. (TFU #21.)
type FailureReasonCode string

const (
	// FailureReasonCancelled — set when the operator cancels the
	// migration via spec.cancelRequested in a pre-cutover phase (the W21
	// CancelIgnored gate suppresses this for post-cutover cancels). Also
	// set when swiftletd reports cancellation propagating from the dst
	// pod's cancel handshake.
	FailureReasonCancelled FailureReasonCode = "Cancelled"
	// FailureReasonPodTerminated — set when the dst pod terminates
	// mid-migration (drain, graceful delete, OOM, etc.) or when the
	// PreparingLive budget expires without the dst pod reaching Running.
	FailureReasonPodTerminated FailureReasonCode = "PodTerminated"
	// FailureReasonSourcePodReplaced — set when the src pod's UID
	// changes mid-migration (K8s-terminated and recreated). Detected via
	// status.sourcePodUID lock-in per F4.2.
	FailureReasonSourcePodReplaced FailureReasonCode = "SourcePodReplaced"
	// FailureReasonTimeout — set when spec.timeout (default 30m,
	// minimum 60s for live mode) expires.
	FailureReasonTimeout FailureReasonCode = "Timeout"
	// FailureReasonOther — catch-all for migration-internal errors
	// that don't fit any specific code. Detail in FailureMessage.
	FailureReasonOther FailureReasonCode = "Other"

	// Phase 3b PR 2 additions — see Phase 3b design doc §4.7.

	// FailureReasonEligibilityMismatch — set by Validating-phase
	// eligibility checks when the SwiftGuest's resolved spec does not
	// support live migration (non-RWX+Block storage, VFIO devices, etc.).
	// Defensive twin of the webhook's admission-time gate; fires for the
	// rare case where eligibility changed after admission (e.g., guest's
	// storage spec mutated between SwiftMigration creation and reconcile).
	FailureReasonEligibilityMismatch FailureReasonCode = "EligibilityMismatch"
	// FailureReasonDstScheduleFailed — set when the dst pod cannot be
	// scheduled onto the target node (insufficient capacity, taints,
	// affinity rules, etc.) within the PreparingLive budget.
	FailureReasonDstScheduleFailed FailureReasonCode = "DstScheduleFailed"
	// FailureReasonDstNeverReady — set when the dst pod is created but
	// does not reach Ready within the PreparingLive budget (default
	// 60s). Distinguishes a stuck-but-alive dst pod from one that
	// terminated mid-migration (PodTerminated).
	//
	// SEMANTIC REFINEMENT FROM PHASE 3A: this code supersedes
	// PodTerminated for the budget-timeout site in preparing_live.go.
	// Phase 3a reported PodTerminated for the "pod alive but stuck"
	// case as well as the "pod genuinely terminated" case; Phase 3b PR 2
	// splits them. Operators with dashboards filtering on PodTerminated
	// will no longer match the budget-timeout scenario — they should
	// match DstNeverReady OR PodTerminated.
	//
	// Future work (tracked follow-up): DstScheduleFailed will split out
	// the "could not be scheduled onto target node" case from
	// DstNeverReady. Until then, scheduling-failure scenarios fall into
	// DstNeverReady (the budget-timeout site catches both).
	FailureReasonDstNeverReady FailureReasonCode = "DstNeverReady"
	// FailureReasonReceiveDisconnect — set when CH's migration RPC
	// reports a peer/network disconnect (transport_error or
	// connection_refused category from sanitize_ch_error). The name
	// reflects the event — disconnect during the migration RPC — not
	// the side that observed it. Both src-side (vm.send-migration
	// observes peer drop) and dst-side (vm.receive-migration observes
	// peer drop) detail strings classify to this code. Operator-
	// visible naming asymmetry intentional: the underlying failure is
	// the same network event regardless of which end reported it.
	FailureReasonReceiveDisconnect FailureReasonCode = "ReceiveDisconnect"
	// FailureReasonRpcError — set when CH's vm.send-migration or
	// vm.receive-migration HTTP RPC returns an error that doesn't map to
	// a more specific code (CPU incompatibility, version skew at the
	// wire level, protocol error). Classified from swiftletd's
	// failure-reason-code annotation; detail in FailureMessage.
	FailureReasonRpcError FailureReasonCode = "RpcError"
	// FailureReasonImageTagMismatch — set when the Validating phase's
	// defensive image-tag-match check (LBA-1 trip-wire) detects that the
	// destination pod would not inherit the source pod's launcher
	// image. This should NEVER fire in practice because newDstPod uses
	// srcPod.DeepCopy() which clones the source image atomically; if
	// this code surfaces, a refactor has regressed the clone-src
	// guarantee. See docs/design/live-migration-phase-3b.md LBA-1.
	FailureReasonImageTagMismatch FailureReasonCode = "ImageTagMismatch"
	// FailureReasonDstPodConflict — set when the controller observes a
	// dst pod with the deterministic dst-pod name already present at
	// PreparingLive entry but its shape does not match what newDstPod
	// would produce (wrong nodeName, wrong receiver-mode env, wrong
	// owner ref, etc.). Distinguishes a clean leader-handover idempotent
	// re-entry from a name collision with foreign state.
	FailureReasonDstPodConflict FailureReasonCode = "DstPodConflict"
	// FailureReasonMigrationIdentityNotReady — set by the Validating-live
	// precondition (Phase 3c, Option B) when live-migration mTLS is
	// enabled but a participating node's per-node identity Secret is not
	// yet provisioned in the system namespace (cert-manager precondition
	// not ready). Per design §4.4 the per-node cert is a long-lived
	// precondition, not a per-migration artifact — failing fast here keeps
	// a missing/expired node identity from ever reaching the cutover
	// window. Untyped string (TFU #21) for consistency with the rest of
	// this enum.
	FailureReasonMigrationIdentityNotReady FailureReasonCode = "MigrationIdentityNotReady"
	// FailureReasonSourceSidecarNotReady — set by Validating-live (Phase
	// 3c, Option B) when live-migration mTLS is enabled but the source pod
	// lacks a client-role migration-stunnel sidecar. Happens when the
	// source pod predates mTLS enablement (the sidecar is only injected on
	// newly created launcher pods) or is a post-cutover destination pod
	// from a prior migration (server-role sidecar; chain migrations under
	// mTLS need a pod recycle). Operators recycle the guest's pod and
	// retry. Untyped string (TFU #21).
	FailureReasonSourceSidecarNotReady FailureReasonCode = "SourceSidecarNotReady"
)

// Phase 3a phaseDetail vocabulary additions (live mode only). These
// strings are stable per docs/design/live-migration-phase-3a.md §6.4
// stability discipline: additions are non-breaking; renames go through
// one-minor-release deprecation cycles with both forms emitted in
// parallel; semantic changes require a NEW value rather than
// repurposing an existing one. Operators may parse these via shell
// scripts; dashboards may match against them; reconcile-loop recovery
// reads them.
const (
	PhaseDetailLiveIssuingRecv       = "issuing receive on destination"
	PhaseDetailLiveDestReceiving     = "destination receiving"
	PhaseDetailLiveIssuingSend       = "issuing send on source"
	PhaseDetailLiveTransferring      = "transferring guest state"
	PhaseDetailLiveSrcCompleted      = "src migration complete; preparing cutover"
	PhaseDetailLiveCutoverPodRef     = "cutover: updating canonical pod"
	PhaseDetailLiveCutoverDeleteSrc  = "cutover: deleting source pod"
	PhaseDetailLiveCutoverCompleting = "cutover: completing"
	PhaseDetailLiveAwaitingHealth    = "waiting for guest health on destination"
	PhaseDetailLiveDestHealthy       = "destination guest healthy"
)

// SwiftMigrationGuestRef references the SwiftGuest to migrate.
type SwiftMigrationGuestRef struct {
	// Name of the SwiftGuest in the same namespace as this SwiftMigration.
	// Cross-namespace migration is not supported in Phase 1.
	Name string `json:"name"`
}

// SwiftMigrationTarget identifies the destination node. Exactly one of
// nodeName or nodeSelector must be set; the validation webhook rejects
// both-set or neither-set.
//
// Phase 1 ships nodeName only. nodeSelector is reserved for Phase 4 (drain
// integration) where the controller picks an arbitrary healthy node and
// the operator may want to constrain the candidate set (e.g. by zone).
type SwiftMigrationTarget struct {
	// NodeName pins the migration to a specific node by name.
	// +optional
	NodeName string `json:"nodeName,omitempty"`
	// NodeSelector constrains the candidate node set. Reserved for Phase 4.
	// The Phase 1 webhook rejects nodeSelector with a clear "not yet shipped"
	// message — the validation surface lands now so Phase 4 doesn't need a
	// breaking API change.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// SwiftMigrationSpec defines the desired state of a SwiftMigration.
type SwiftMigrationSpec struct {
	// GuestRef references the SwiftGuest to migrate (same namespace).
	GuestRef SwiftMigrationGuestRef `json:"guestRef"`
	// Target identifies the destination node.
	Target SwiftMigrationTarget `json:"target"`
	// Mode selects the migration strategy. Phase 1 supports offline;
	// the webhook rejects live; auto resolves to offline.
	// +kubebuilder:default=auto
	Mode SwiftMigrationMode `json:"mode,omitempty"`
	// DowntimeTarget is the Cloud Hypervisor `downtime_ms` target for
	// mode=live on CH >= v52: CH iterates pre-copy until the estimated final
	// stop-and-copy (the vCPU-paused window) fits under this budget, then
	// commits — classical dirty-rate convergence, superseding v51.1's
	// hardcoded 5-iteration cap. Unset means CH keeps its native behaviour
	// (the controller omits downtime_ms from the send). The webhook bounds it
	// to [10ms, 10s] for mode=live. Ignored for mode=offline (offline
	// downtime is bounded by storage detach + VM boot, not a CH budget).
	// +optional
	DowntimeTarget *metav1.Duration `json:"downtimeTarget,omitempty"`
	// ParallelConnections is the number of parallel TCP connections CH uses
	// for the live-migration memory stream (CH >= v52) — higher throughput
	// on fast interconnects. 0 or 1 uses a single connection (the default);
	// values >= 2 are passed through as CH's `connections`. The webhook caps
	// it (MaxParallelConnections). Below the NIC line rate parallel streams
	// add little (the single stream already saturates), so this matters on
	// 10GbE+ interconnects. Ignored for mode=offline (no memory stream).
	// +optional
	ParallelConnections int32 `json:"parallelConnections,omitempty"`
	// Timeout bounds the entire migration operation (StartedAt -> terminal).
	// On expiry, behaviour is controlled by TimeoutStrategy (default cancel).
	// It is a runaway BACKSTOP, not a tight SLA: the default 30m sits well
	// above swiftletd's own per-action timeout (600s, sized for a ~200 GiB
	// VM), so it never pre-empts a legitimately slow transfer — it only
	// rescues a genuinely wedged migration. The webhook enforces a 60s
	// minimum for mode=live. Operators wanting a tighter bound set it
	// explicitly (e.g. swiftctl migrate --timeout).
	// +optional
	// +kubebuilder:default="30m0s"
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// TimeoutStrategy controls behavior on timeout. Phase 1 supports cancel
	// only (the default); ignore is reserved for live mode.
	// +kubebuilder:default=cancel
	TimeoutStrategy SwiftMigrationTimeoutStrategy `json:"timeoutStrategy,omitempty"`
	// Reason is a free-form informational string. Drain-initiated migrations
	// (Phase 4) populate this with "node-drain"; operator-initiated migrations
	// may use it for audit trail purposes.
	// +optional
	Reason string `json:"reason,omitempty"`
	// TTL, when set, makes the controller delete this SwiftMigration once it has
	// been in a terminal phase (Completed/Failed/Cancelled) for at least ttl —
	// keeping `kubectl get swiftmigration` from accumulating finished records.
	// A SwiftMigration carries no backend artifacts, so deletion just removes
	// the CR (no purge). Unset = keep until deleted by hand. Drain-initiated
	// migrations get a default ttl set by the drain controller.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
	// AllowIPChange opts the operator into a migration that will produce a
	// fresh guest IP on the destination side. Required for guests on the
	// default node-local-bridge network (KubeSwift's default) when source
	// node != target node. Without this flag the validation webhook rejects
	// the migration with a clear error pointing operators at multus or
	// OVN-K layer-2 networking. When the flag is set and triggers, the
	// controller writes IPWillChange=True for visibility.
	// +optional
	AllowIPChange bool `json:"allowIPChange,omitempty"`
	// CancelRequested triggers controller-side cancellation of an in-flight
	// migration without deleting the SwiftMigration CR. Honored only in
	// pre-cutover phases (Pending, Validating, Preparing, StopAndCopy
	// before cutover step 1 succeeds). When set during or after cutover,
	// the controller surfaces a CancelIgnored condition with reason
	// PastCutover and the migration completes normally — the cutover
	// cannot be reversed once status.podRef.name has swapped to the dst
	// pod and the src pod is deleted. See
	// docs/design/live-migration-phase-3a.md §5.3 for the rationale.
	//
	// Operators using mode=live should prefer spec.cancelRequested over
	// `kubectl delete pod <dst>` for cancellation: force-delete of the
	// dst pod produces a slow failure path bounded by spec.timeout
	// (W12 finding from PR #41 — swift-ch-client's synchronous send
	// hangs on TCP retransmit when the dst is killed without
	// swiftletd's mediation).
	//
	// Operationally distinct from DeletionTimestamp != nil: deleting the
	// SwiftMigration CR runs handleCancellation (Phase 1 finalizer
	// path) which removes the CR after cleanup; cancelRequested keeps
	// the CR for audit and surfaces the result via phase=Cancelled.
	// +optional
	CancelRequested bool `json:"cancelRequested,omitempty"`
}

// SwiftMigrationPodRef captures the pod identity at a phase boundary.
type SwiftMigrationPodRef struct {
	Name string `json:"name,omitempty"`
}

// SwiftMigrationStatus is the observed state of a SwiftMigration.
type SwiftMigrationStatus struct {
	// Phase is the current lifecycle phase.
	Phase SwiftMigrationPhase `json:"phase,omitempty"`
	// Conditions list. Ready and Compatible are populated; IPWillChange
	// surfaces only when the operator opted into AllowIPChange and the
	// guest is on default networking.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Mode is the resolved migration mode. In Phase 1 this is always offline
	// regardless of spec.mode. Operators can confirm what was actually picked.
	Mode SwiftMigrationMode `json:"mode,omitempty"`
	// SourceNode is the node the SwiftGuest was running on at migration start.
	SourceNode string `json:"sourceNode,omitempty"`
	// DestinationNode is the resolved target node (after webhook validation).
	DestinationNode string `json:"destinationNode,omitempty"`
	// SourcePodRef is the launcher pod that was running on the source.
	// +optional
	SourcePodRef *SwiftMigrationPodRef `json:"sourcePodRef,omitempty"`
	// DestinationPodRef is the launcher pod created on the destination.
	// In Phase 1 (direct PVC reuse) source and destination share the same
	// pod name (guest.Name) — the field exists for symmetry and Phase 3
	// where live mode may run two launcher pods concurrently.
	// +optional
	DestinationPodRef *SwiftMigrationPodRef `json:"destinationPodRef,omitempty"`
	// PhaseDetail is a short human-readable description of the current
	// sub-state. Updated only on meaningful sub-state transitions, not on
	// every reconcile. Examples: "waiting for PVC detach (Longhorn:
	// detaching)", "waiting for GuestRunning condition".
	PhaseDetail string `json:"phaseDetail,omitempty"`
	// StartedAt is when the controller first observed the SwiftMigration.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// PreparingStartedAt is when the SwiftMigration first transitioned
	// to the Preparing phase. Live-mode Preparing-live uses this as the
	// anchor for the 60-second destination-pod-Ready budget; if the
	// dst pod hasn't reached Ready by PreparingStartedAt + 60s, the
	// migration transitions to Failed with FailureReason=PodTerminated.
	// Persisted in status (not in-memory) so leader-handover preserves
	// the budget anchor and the new leader doesn't restart the 60s
	// window. Phase 1 offline mode does not populate this field.
	// +optional
	PreparingStartedAt *metav1.Time `json:"preparingStartedAt,omitempty"`
	// CutoverStep1At is when cutover step 1 (SwiftGuest.status.podRef.name
	// patch) succeeded. Stamped by StopAndCopy-live's cutover handler
	// alongside the SwiftGuest patch. Operator-visible audit data:
	// `kubectl get swiftmigration -o wide` may surface this for
	// drilling into "when did the canonical pod actually swap?"
	// during incident triage.
	//
	// Phase 3a's PodRefSwapped condition is DERIVED from cluster
	// state on every reconcile (per architect Q3.3(c)) — this
	// timestamp is independent of that derivation and serves only
	// audit purposes. Resuming-live (B2.3) may reference this for
	// observedDowntime computation refinement; B2.3 currently uses
	// status.ResumingStartedAt as the anchor (which is at-most one
	// apiserver round-trip after CutoverStep1At).
	//
	// Phase 1 offline mode does not populate this field.
	// +optional
	CutoverStep1At *metav1.Time `json:"cutoverStep1At,omitempty"`
	// CutoverStep2DispatchedAt is when cutover step 2 (src pod Delete
	// dispatch) was issued by the controller. This is the cutover
	// commit point on the source side — vCPU pause begins inside CH
	// at this moment, propagating to the dst migration receiver.
	//
	// W27a (PR #54 follow-up, Tracked Follow-up #7): this is the
	// authoritative anchor for observedDowntime. The prior anchor
	// (status.ResumingStartedAt) was stamped one apiserver round-trip
	// later AND consumed in the same reconcile invocation, producing
	// sub-millisecond observedDowntime values across all 17 PR #46 +
	// E12 walkthrough runs. Anchoring on CutoverStep2DispatchedAt
	// instead measures the actual operator-visible cutover-to-resume
	// window: cutover-step-2-dispatch → GuestRunning=True observation.
	//
	// Stamped idempotently by cutoverStep2: only set if currently nil,
	// preserving the original timestamp across leader-handover and
	// cutover-mid-flight reconcile-recovery (§2.4) reentry.
	//
	// Phase 1 offline mode does not populate this field.
	// +optional
	CutoverStep2DispatchedAt *metav1.Time `json:"cutoverStep2DispatchedAt,omitempty"`
	// TerminalAt is when the SwiftMigration first reached a terminal phase
	// (Completed/Failed/Cancelled). Stamped once on the non-terminal→terminal
	// transition; the anchor for spec.ttl-driven deletion.
	// +optional
	TerminalAt *metav1.Time `json:"terminalAt,omitempty"`
	// ResumingStartedAt is when the SwiftMigration first transitioned
	// to the Resuming phase (cutover step 3 — the controller's
	// observable boundary between StopAndCopy completion and Resuming
	// entry). Live-mode Resuming-live uses this as the anchor for
	// status.observedDowntime: ObservedDowntime = GuestRunning=True
	// observation time - ResumingStartedAt. Persisted in status (not
	// in-memory) so leader-handover preserves the anchor and the new
	// leader's downtime computation reflects the original cutover
	// boundary, not the leader-handover boundary. Phase 1 offline does
	// not populate this field.
	// +optional
	ResumingStartedAt *metav1.Time `json:"resumingStartedAt,omitempty"`
	// CompletedAt is when the migration reached a terminal state.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// ObservedDowntime is the wall-clock duration the guest was
	// unresponsive on cluster, measured against different anchors per
	// mode:
	//
	//   - Offline mode (Phase 1): from Preparing entry to GuestRunning
	//     =True observation on destination. Includes the entire
	//     stop+detach+attach+cold-boot sequence — typically tens of
	//     seconds dominated by storage detach and VM boot.
	//
	//   - Live mode (Phase 3a, post-W27a fix): from
	//     CutoverStep2DispatchedAt (src pod Delete dispatch — vCPU
	//     pause begins inside CH on src) to GuestRunning=True
	//     observation on destination (vCPU pause ends on dst).
	//     Typically low single-digit seconds for default node-local
	//     networking. Pre-W27a this measured two adjacent
	//     metav1.Now() calls in the same reconcile, producing
	//     sub-millisecond nonsense; see Tracked Follow-up #7 close-out
	//     in kubeswift_context.md.
	//
	// **Live-mode caveat**: this is the cutover-orchestration window,
	// NOT the "guest stopped-the-world" window from inside the guest.
	// The actual vCPU-paused sub-phase inside Cloud Hypervisor's
	// vm.send-migration RPC is internal to CH and not separately
	// surfaced today (W28 candidate per Tracked Follow-up #7
	// close-out — capture per-phase timing from CH internals).
	// +optional
	ObservedDowntime *metav1.Duration `json:"observedDowntime,omitempty"`
	// FailureMessage is a structured human-readable failure description.
	// Set on Failed (and Cancelled) phase. Free-form companion to
	// FailureReason.
	FailureMessage string `json:"failureMessage,omitempty"`
	// FailureReason classifies terminal Failed transitions for live mode.
	// Phase 3a shipped the first five codes; Phase 3b PR 2 extends the
	// taxonomy with seven additional codes that classify live-mode
	// failures more precisely. See the per-constant docstrings at the
	// top of this file for fire conditions; cross-reference
	// docs/design/live-migration-phase-3b.md §4.7 for the
	// failure-mode-to-reason mapping.
	//
	// Phase 3a codes (carried through unchanged):
	//   - Cancelled, PodTerminated, SourcePodReplaced, Timeout, Other
	//
	// Phase 3b PR 2 codes (live-mode classification):
	//   - EligibilityMismatch — Validating defense-in-depth for
	//     post-admission eligibility drift.
	//   - DstScheduleFailed — dst pod could not be scheduled within
	//     the PreparingLive budget.
	//   - DstNeverReady — dst pod ran but never reached receive-ready;
	//     distinguishes hung swiftletd from a terminated pod.
	//   - ReceiveDisconnect — dst-side RPC reported peer/network
	//     disconnect during transfer.
	//   - RpcError — CH HTTP RPC error not otherwise classified.
	//   - ImageTagMismatch — LBA-1 trip-wire (defensive; should never
	//     fire). See docs/design/live-migration-phase-3b.md LBA-1.
	//   - DstPodConflict — dst pod name collision with foreign state.
	//
	// Phase 1 offline mode does not populate this field — its failure
	// modes are simpler and FailureMessage alone is sufficient. Live
	// mode uses this enum to give the operator a stable taxonomy
	// for `kubectl get swiftmigration` output and dashboard alerting.
	// +kubebuilder:validation:Enum=Cancelled;PodTerminated;SourcePodReplaced;Timeout;Other;EligibilityMismatch;DstScheduleFailed;DstNeverReady;ReceiveDisconnect;RpcError;ImageTagMismatch;DstPodConflict;MigrationIdentityNotReady;SourceSidecarNotReady
	// +optional
	FailureReason FailureReasonCode `json:"failureReason,omitempty"`
	// SourcePodUID is the source launcher pod's UID at Validating-phase
	// entry. Used by F4.2's UID-change failure detection: at every
	// reconcile in pre-cutover phases, the controller compares the
	// observed src pod UID against this stored value; mismatch indicates
	// the src pod was K8s-terminated and recreated mid-migration, which
	// transitions the SwiftMigration to Failed with
	// FailureReason=SourcePodReplaced. Suppressed post-cutover because
	// the cutover step 2 intentionally deletes the src pod.
	// +optional
	SourcePodUID types.UID `json:"sourcePodUID,omitempty"`
	// RecvAttempts counts vm.receive-migration dispatches issued on the
	// destination launcher pod. Used to derive $RECV_ID per F1.8 as
	// `<swiftmigration.Name>:recv:<RecvAttempts>`. Phase 3a never
	// retries within a single SwiftMigration — failures cascade to the
	// Failed terminal state, the operator creates a new SwiftMigration
	// to retry — so this counter is 1 on success and never increments
	// in PR 1. The field exists as forward-compat for future retry
	// policies (Phase 5+) and for leader-handover state reconstruction
	// (the same RECV_ID survives across reconcile invocations).
	// +optional
	RecvAttempts int32 `json:"recvAttempts,omitempty"`
	// SendAttempts counts vm.send-migration dispatches issued on the
	// source launcher pod. Used to derive $SEND_ID per F1.8. Same
	// semantics as RecvAttempts.
	// +optional
	SendAttempts int32 `json:"sendAttempts,omitempty"`
	// ObservedTransferDuration is the swiftletd-reported elapsed time
	// of the vm.send-migration RPC on the source pod: pre-copy
	// iterations + final stop-and-copy + finalize. Most of this window
	// is NOT vCPU-paused; the guest stays responsive throughout
	// pre-copy. For the operator-visible cluster downtime window
	// (cutoverStep2DispatchedAt → GuestRunning observation on the
	// destination), see ObservedDowntime.
	//
	// Read from the kubeswift.io/migration-pause-window-ms annotation
	// that swiftletd writes alongside migration-status=complete
	// (rust/swiftletd/src/action.rs::write_migration_status). Stamped
	// by stopandcopy_live's substateSrcCompleted handler per W27b.
	// (The deprecated ObservedPauseWindow alias was removed in the
	// CH-v52 observability work; this is the canonical field.)
	//
	// Empirical baseline from Phase 3b spike Q2 on Calico VXLAN pod
	// networking: ~38s for a 4Gi RAM guest with no memory-dirtying
	// workload; scales monotonically with dirty rate (~45s LOW, ~68s
	// MED, ~87s HIGH for stress-ng rand-set workloads). Operator
	// sizing formula for live-migratable guests:
	//   (guest_RAM × 1.05) / pod_network_bandwidth
	// (the 1.05 factor accounts for the ~5% CH orchestration overhead
	// measured in spike Q4.)
	//
	// Not populated for status.mode=offline migrations; offline
	// migration shuts down the source guest and restarts it on the
	// destination, with no memory transfer RPC involved.
	// +optional
	ObservedTransferDuration *metav1.Duration `json:"observedTransferDuration,omitempty"`
	// AppliedDowntimeMs is the Cloud Hypervisor `downtime_ms` ceiling the
	// controller actually sent on vm.send-migration (from
	// spec.downtimeTarget; CH >= v52). It is a BOUND, not a measurement:
	// CH converges pre-copy so the final vCPU stop-and-copy fits under it,
	// so the real "guest frozen" window is <= this value — but CH v52 does
	// not report the achieved downtime (vm.send-migration returns 204 with
	// no body, and CH logs no migration telemetry at its default level), so
	// the achieved window is not directly measurable here. The only ground
	// truth is an external observer (e.g. an L2 sibling pinging the guest;
	// Tracked Follow-up #1). Nil when spec.downtimeTarget was unset (CH used
	// its native behaviour) or for mode=offline.
	// +optional
	AppliedDowntimeMs *int64 `json:"appliedDowntimeMs,omitempty"`

	// TransferProgress is the live-migration pre-copy progress estimate, an
	// integer percentage 0-100, surfaced from the swiftletd-on-source
	// kubeswift.io/migration-progress-estimate annotation during the
	// StopAndCopy transferring substate (Phase 5). It is a BANDWIDTH HEURISTIC
	// (Phase 3b design §5.4, calibrated on Calico-VXLAN), not a byte-exact
	// counter — accuracy degrades on CNIs far from that baseline. It climbs
	// during pre-copy and is pinned to 100 once the source reports transfer
	// complete. Not populated for status.mode=offline migrations (no memory
	// transfer). Operators reading it should treat it as approximate.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	TransferProgress *int32 `json:"transferProgress,omitempty"`
	// TargetIP is the destination guest's primary IP, propagated from
	// the source pod's pre-migration kubeswift.io/guest-ip annotation
	// via swiftletd at receive-complete time (PR #41 D3 propagation).
	// The controller reads the dst pod's annotation in the Resuming
	// phase and reflects the value here for operator visibility in
	// `kubectl get swiftmigration -o wide`. Live mode only; offline
	// mode populates SwiftGuest.status.network.primaryIP via the
	// existing first-boot lease poller path.
	// +optional
	TargetIP string `json:"targetIP,omitempty"`
}

// SwiftMigration represents an operator-initiated request to move a
// SwiftGuest from one Kubernetes node to another. Phase 1 ships offline
// migration only — the source guest is fully stopped, its root-disk PVC
// is reattached on the destination node, and the launcher pod is
// recreated with spec.nodeName pinning. Live migration (sub-second
// downtime via memory pre-copy) is reserved for Phase 3.
//
// Same-namespace constraint: the referenced SwiftGuest, its PVCs, and
// the SwiftMigration all live in the same namespace.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftmigrations,scope=Namespaced,shortName=smig
// +kubebuilder:printcolumn:name="Guest",type=string,JSONPath=`.spec.guestRef.name`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.status.sourceNode`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.status.destinationNode`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.status.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Progress",type=integer,JSONPath=`.status.transferProgress`
// +kubebuilder:printcolumn:name="Downtime",type=string,JSONPath=`.status.observedDowntime`
// +kubebuilder:printcolumn:name="Transfer",type=string,JSONPath=`.status.observedTransferDuration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftMigrationSpec   `json:"spec,omitempty"`
	Status SwiftMigrationStatus `json:"status,omitempty"`
}

// SwiftMigrationList contains a list of SwiftMigration.
// +kubebuilder:object:root=true
type SwiftMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftMigration `json:"items"`
}

// Annotation constants used by the SwiftMigration controller to coordinate
// with the SwiftGuest controller. Architect risk #3 (cache staleness on
// reconcile re-entry) is addressed by writing this annotation on the source
// SwiftGuest at first StopAndCopy entry, then treating the annotation as
// the source-of-truth idempotency marker on subsequent re-entries.
const (
	// AnnotationMigrationInProgress on a SwiftGuest carries the name of
	// the SwiftMigration currently migrating it. Set at first Preparing
	// entry, cleared on Completed/Failed/Cancelled. The SwiftGuest
	// controller does not consume this annotation; it is purely a
	// SwiftMigration-controller idempotency marker.
	AnnotationMigrationInProgress = "kubeswift.io/migration-in-progress"
)
