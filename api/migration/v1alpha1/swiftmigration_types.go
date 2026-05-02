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
const (
	FailureReasonCancelled         = "Cancelled"
	FailureReasonPodTerminated     = "PodTerminated"
	FailureReasonSourcePodReplaced = "SourcePodReplaced"
	FailureReasonTimeout           = "Timeout"
	FailureReasonOther             = "Other"
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
	PhaseDetailLiveIssuingRecv      = "issuing receive on destination"
	PhaseDetailLiveDestReceiving    = "destination receiving"
	PhaseDetailLiveIssuingSend      = "issuing send on source"
	PhaseDetailLiveTransferring     = "transferring guest state"
	PhaseDetailLiveSrcCompleted     = "src migration complete; preparing cutover"
	PhaseDetailLiveCutoverPodRef    = "cutover: updating canonical pod"
	PhaseDetailLiveCutoverDeleteSrc = "cutover: deleting source pod"
	PhaseDetailLiveAwaitingHealth   = "waiting for guest health on destination"
	PhaseDetailLiveDestHealthy      = "destination guest healthy"
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
	// DowntimeTarget is the Cloud Hypervisor downtime_ms target. Ignored in
	// Phase 1 (offline migration's downtime is bounded by storage detach +
	// VM boot, not by CH's downtime budget). Carried in the spec for forward
	// compatibility with live mode.
	// +optional
	DowntimeTarget *metav1.Duration `json:"downtimeTarget,omitempty"`
	// ParallelConnections is the number of TCP connections CH uses for the
	// migration stream in live mode. Ignored in Phase 1.
	// +optional
	ParallelConnections int32 `json:"parallelConnections,omitempty"`
	// Timeout bounds the entire migration operation. On expiry, behavior
	// is controlled by TimeoutStrategy. Phase 1 default: 30 minutes.
	// +optional
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
	// ObservedDowntime is the wall-clock duration the guest was unresponsive,
	// measured from Preparing entry to GuestRunning=True on the destination.
	// In offline mode this includes the entire stop+attach+boot sequence.
	// +optional
	ObservedDowntime *metav1.Duration `json:"observedDowntime,omitempty"`
	// FailureMessage is a structured human-readable failure description.
	// Set on Failed (and Cancelled) phase. Free-form companion to
	// FailureReason.
	FailureMessage string `json:"failureMessage,omitempty"`
	// FailureReason classifies terminal Failed transitions for live mode.
	// One of:
	//   - Cancelled: operator cancel via spec.cancelRequested or
	//     SwiftMigration deletion (also populated on Cancelled phase
	//     for symmetry).
	//   - PodTerminated: dst pod terminated mid-migration (drain,
	//     graceful delete, OOM, etc).
	//   - SourcePodReplaced: src pod was K8s-terminated and recreated
	//     mid-migration (UID change detected per F4.2).
	//   - Timeout: spec.timeout exceeded.
	//   - Other: catch-all for migration-internal errors (CH error,
	//     CPU incompatibility, W1 violation, etc) with detail in
	//     FailureMessage.
	// Phase 1 offline mode does not populate this field — its failure
	// modes are simpler and FailureMessage alone is sufficient. Phase 3a
	// live mode uses this enum to give the operator a stable taxonomy
	// for `kubectl get swiftmigration` output and dashboard alerting.
	// See docs/design/live-migration-phase-3a.md §4.7 for the
	// failure-mode-to-reason mapping.
	// +kubebuilder:validation:Enum=Cancelled;PodTerminated;SourcePodReplaced;Timeout;Other
	// +optional
	FailureReason string `json:"failureReason,omitempty"`
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
	// ObservedPauseWindow is the swiftletd-on-src-reported vCPU-paused
	// duration during stop-and-copy, parsed from the src pod's
	// migration-status-detail annotation when send completes. Distinct
	// from ObservedDowntime (which spans cutover + apiserver latency,
	// dominated by storage and pod lifecycle costs in offline mode and
	// by network + cutover costs in live mode). The pause window is
	// the operator-relevant metric for "how long was the guest
	// frozen" — typically 0.5-5s for live mode per F6/F7 of the Phase
	// 2 spike.
	// +optional
	ObservedPauseWindow *metav1.Duration `json:"observedPauseWindow,omitempty"`
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
// +kubebuilder:printcolumn:name="Downtime",type=string,JSONPath=`.status.observedDowntime`
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
