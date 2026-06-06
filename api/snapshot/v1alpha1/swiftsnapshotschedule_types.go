package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotConcurrencyPolicy controls what happens when a schedule tick fires
// while a previous scheduled snapshot is still in flight (non-terminal).
// +kubebuilder:validation:Enum=Forbid;Allow
type SnapshotConcurrencyPolicy string

const (
	// ConcurrencyForbid skips a tick while a prior scheduled snapshot of this
	// schedule is still capturing/uploading (the default — captures are heavy).
	ConcurrencyForbid SnapshotConcurrencyPolicy = "Forbid"
	// ConcurrencyAllow lets scheduled snapshots overlap.
	ConcurrencyAllow SnapshotConcurrencyPolicy = "Allow"
)

// SnapshotScheduleRetention controls count-based pruning of a schedule's
// snapshots. It complements the per-snapshot age-based spec.ttl.
type SnapshotScheduleRetention struct {
	// KeepLast keeps the most recent N Ready snapshots created by this schedule;
	// older Ready ones are deleted (honoring each snapshot's deletionPolicy, and
	// never deleting one still referenced by a cloneFromSnapshot SwiftGuest or an
	// in-flight SwiftRestore). Non-terminal and Failed snapshots do not count
	// toward the budget and are not pruned by keepLast. Unset = keep all (rely on
	// per-snapshot ttl or manual deletion).
	// +kubebuilder:validation:Minimum=1
	// +optional
	KeepLast *int32 `json:"keepLast,omitempty"`
}

// SnapshotTemplateMeta is the metadata merged onto each created SwiftSnapshot.
type SnapshotTemplateMeta struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// SnapshotTemplate is the SwiftSnapshot the schedule instantiates each tick.
type SnapshotTemplate struct {
	// +optional
	Metadata SnapshotTemplateMeta `json:"metadata,omitempty"`
	// Spec is the SwiftSnapshotSpec to instantiate (same shape as a hand-written
	// SwiftSnapshot — guestRef, backend, includeMemory, deletionPolicy, ttl).
	Spec SwiftSnapshotSpec `json:"spec"`
}

// SwiftSnapshotScheduleSpec defines the desired state of a SwiftSnapshotSchedule.
type SwiftSnapshotScheduleSpec struct {
	// Schedule is a standard 5-field cron expression, evaluated in UTC.
	// Example: "0 2 * * *" (daily at 02:00 UTC).
	Schedule string `json:"schedule"`

	// Suspend pauses the schedule (no new snapshots) without deleting it or its
	// existing snapshots.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// ConcurrencyPolicy controls overlapping runs. Forbid (default) skips a tick
	// while a prior scheduled snapshot is still in flight; Allow lets them overlap.
	// +kubebuilder:default=Forbid
	// +optional
	ConcurrencyPolicy SnapshotConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// StartingDeadlineSeconds bounds how late a tick may fire: a tick missed by
	// more than this (e.g. during a controller outage) is skipped rather than
	// run, avoiding a catch-up stampede. Unset = no deadline.
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// Retention controls count-based pruning (keepLast) of this schedule's
	// snapshots. Composes with each snapshot's age-based spec.ttl.
	// +optional
	Retention *SnapshotScheduleRetention `json:"retention,omitempty"`

	// Template is the SwiftSnapshot to create each time the schedule fires.
	Template SnapshotTemplate `json:"template"`
}

// SwiftSnapshotScheduleStatus is the observed state of a SwiftSnapshotSchedule.
type SwiftSnapshotScheduleStatus struct {
	// LastScheduleTime is when the controller last fired a tick.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// LastSuccessfulTime is when a scheduled snapshot last reached Ready.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`
	// Active lists the names of in-flight (non-terminal) scheduled snapshots.
	// +optional
	Active []string `json:"active,omitempty"`
	// Conditions exposes Ready.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SwiftSnapshotSchedule creates SwiftSnapshots of a SwiftGuest on a cron
// schedule, with optional count-based (keepLast) retention.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftsnapshotschedules,scope=Namespaced,shortName=sss
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Guest",type=string,JSONPath=`.spec.template.spec.guestRef.name`
// +kubebuilder:printcolumn:name="Last-Schedule",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftSnapshotSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftSnapshotScheduleSpec   `json:"spec,omitempty"`
	Status SwiftSnapshotScheduleStatus `json:"status,omitempty"`
}

// SwiftSnapshotScheduleList contains a list of SwiftSnapshotSchedule.
// +kubebuilder:object:root=true
type SwiftSnapshotScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftSnapshotSchedule `json:"items"`
}

// ScheduleLabel is set on every SwiftSnapshot a schedule creates, valued with
// the schedule's name. It is the keep-N grouping key and a `kubectl get` filter.
const ScheduleLabel = "snapshot.kubeswift.io/schedule"

// SwiftSnapshotScheduleConditionReady is the schedule's Ready condition type.
const SwiftSnapshotScheduleConditionReady = "Ready"
