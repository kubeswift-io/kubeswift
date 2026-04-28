package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// CompletedAt is when the migration reached a terminal state.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// ObservedDowntime is the wall-clock duration the guest was unresponsive,
	// measured from Preparing entry to GuestRunning=True on the destination.
	// In offline mode this includes the entire stop+attach+boot sequence.
	// +optional
	ObservedDowntime *metav1.Duration `json:"observedDowntime,omitempty"`
	// FailureMessage is a structured human-readable failure description.
	// Set on Failed phase only.
	FailureMessage string `json:"failureMessage,omitempty"`
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

