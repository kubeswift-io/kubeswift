package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftRestorePhase is the lifecycle phase of a SwiftRestore.
// +kubebuilder:validation:Enum=Pending;Downloading;Restoring;Resuming;Ready;Failed
type SwiftRestorePhase string

const (
	SwiftRestorePhasePending     SwiftRestorePhase = "Pending"
	SwiftRestorePhaseDownloading SwiftRestorePhase = "Downloading"
	SwiftRestorePhaseRestoring   SwiftRestorePhase = "Restoring"
	SwiftRestorePhaseResuming    SwiftRestorePhase = "Resuming"
	SwiftRestorePhaseReady       SwiftRestorePhase = "Ready"
	SwiftRestorePhaseFailed      SwiftRestorePhase = "Failed"
)

const (
	SwiftRestoreConditionReady = "Ready"
)

// SwiftRestoreSnapshotRef points at the SwiftSnapshot to restore from.
// The snapshot must live in the same namespace as the SwiftRestore. The
// validation webhook rejects cross-namespace references — see Phase 0
// spike finding §6a (silent empty-PVC failure on k0s 1.34).
type SwiftRestoreSnapshotRef struct {
	Name string `json:"name"`
}

// SwiftRestoreTarget specifies where the restored VM lands.
type SwiftRestoreTarget struct {
	// Name of the resulting SwiftGuest. May match an existing SwiftGuest
	// only if OverwriteExisting is true.
	Name string `json:"name"`
	// OverwriteExisting must be true to restore over a SwiftGuest that
	// already exists at the target Name. The existing guest is gracefully
	// stopped before the restore proceeds.
	OverwriteExisting bool `json:"overwriteExisting,omitempty"`
}

// IdentityRegenerationItem names a guest-identity attribute to regenerate
// when the snapshot is cloned into a new VM.
// +kubebuilder:validation:Enum=hostname;machineId;sshHostKeys;macAddresses
type IdentityRegenerationItem string

const (
	RegenHostname     IdentityRegenerationItem = "hostname"
	RegenMachineID    IdentityRegenerationItem = "machineId"
	RegenSSHHostKeys  IdentityRegenerationItem = "sshHostKeys"
	RegenMACAddresses IdentityRegenerationItem = "macAddresses"
)

// IdentityRegeneration controls which identity attributes are reset on
// restore (used for cloning workflows). Implementation lands in Phase 2;
// in Phase 1 the field is recorded but the controller does not act on it.
type IdentityRegeneration struct {
	// Regenerate names the attributes the controller should reset on the
	// restored guest.
	Regenerate []IdentityRegenerationItem `json:"regenerate,omitempty"`
}

// SwiftRestoreSpec defines the desired state of a SwiftRestore.
type SwiftRestoreSpec struct {
	// SnapshotRef selects the SwiftSnapshot to restore. Must be in the same
	// namespace as the SwiftRestore.
	SnapshotRef SwiftRestoreSnapshotRef `json:"snapshotRef"`
	// TargetGuest names the resulting SwiftGuest.
	TargetGuest SwiftRestoreTarget `json:"targetGuest"`
	// ResumeAfterRestore controls whether the restored VM is resumed
	// automatically (default true). false leaves the VM in the post-restore
	// Paused state for operator inspection.
	// +kubebuilder:default=true
	ResumeAfterRestore bool `json:"resumeAfterRestore,omitempty"`
	// Identity controls per-clone identity regeneration. Phase 2 territory
	// (memory snapshots only); accepted in Phase 1 for forward compatibility.
	// +optional
	Identity *IdentityRegeneration `json:"identity,omitempty"`
	// TargetNode pins the node the restore lands on. It is only consulted for
	// the s3 (Tier C) backend, where the artifacts live in object storage and
	// the download Job + restore-receive launcher must be co-located on the
	// chosen node. When empty for an s3 restore, the controller falls back to
	// the in-place target guest's current node; a clone/cross-node s3 restore
	// (target guest does not yet exist) requires this field. Ignored for the
	// csi-volume-snapshot and local backends (local is pinned to the capture
	// node).
	// +optional
	TargetNode string `json:"targetNode,omitempty"`
}

// SwiftRestoreGuestRef is the SwiftGuest the SwiftRestore produced (or
// updated, when OverwriteExisting was true).
type SwiftRestoreGuestRef struct {
	Name string `json:"name"`
}

// SwiftRestoreStatus is the observed state of a SwiftRestore.
type SwiftRestoreStatus struct {
	Phase       SwiftRestorePhase     `json:"phase,omitempty"`
	Conditions  []metav1.Condition    `json:"conditions,omitempty"`
	GuestRef    *SwiftRestoreGuestRef `json:"guestRef,omitempty"`
	StartedAt   *metav1.Time          `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time          `json:"completedAt,omitempty"`

	// DownloadedBytes is the snapshot's total artifact footprint materialized on
	// the target node for an s3 (Tier C) restore — read from the download Job's
	// byte report. Absent for local/csi restores (no download step).
	// +optional
	DownloadedBytes int64 `json:"downloadedBytes,omitempty"`
}

// SwiftRestore restores a SwiftSnapshot into a new (or existing) SwiftGuest.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftrestores,scope=Namespaced,shortName=srst
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Snapshot",type=string,JSONPath=`.spec.snapshotRef.name`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetGuest.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftRestoreSpec   `json:"spec,omitempty"`
	Status SwiftRestoreStatus `json:"status,omitempty"`
}

// SwiftRestoreList contains a list of SwiftRestore.
// +kubebuilder:object:root=true
type SwiftRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftRestore `json:"items"`
}
