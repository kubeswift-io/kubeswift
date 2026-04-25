package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotBackendType selects how the snapshot is captured and stored.
// Phase 1 ships csi-volume-snapshot only; local and s3 are reserved for
// later phases. The spec is structured so adding the other backends does
// not require a breaking change.
// +kubebuilder:validation:Enum=csi-volume-snapshot;local;s3
type SnapshotBackendType string

const (
	SnapshotBackendCSIVolumeSnapshot SnapshotBackendType = "csi-volume-snapshot"
	SnapshotBackendLocal             SnapshotBackendType = "local"
	SnapshotBackendS3                SnapshotBackendType = "s3"
)

// SwiftSnapshotPhase is the lifecycle phase of a SwiftSnapshot. The set of
// phases is backend-dependent: csi-volume-snapshot uses
// Pending -> Capturing -> Ready (and Failed). The local and s3 backends
// (added in later phases) introduce additional phases (e.g. Uploading);
// existing phase consumers must treat unknown phases as opaque.
// +kubebuilder:validation:Enum=Pending;Capturing;Uploading;Ready;Failed
type SwiftSnapshotPhase string

const (
	SwiftSnapshotPhasePending   SwiftSnapshotPhase = "Pending"
	SwiftSnapshotPhaseCapturing SwiftSnapshotPhase = "Capturing"
	SwiftSnapshotPhaseUploading SwiftSnapshotPhase = "Uploading"
	SwiftSnapshotPhaseReady     SwiftSnapshotPhase = "Ready"
	SwiftSnapshotPhaseFailed    SwiftSnapshotPhase = "Failed"
)

// Standard condition types exposed by SwiftSnapshot. Ready is required from
// day one (per the GitOps design's recommendation).
const (
	SwiftSnapshotConditionReady = "Ready"
)

// SwiftSnapshotGuestRef references the SwiftGuest to snapshot.
type SwiftSnapshotGuestRef struct {
	Name string `json:"name"`
}

// CSIVolumeSnapshotBackend configures the csi-volume-snapshot backend.
type CSIVolumeSnapshotBackend struct {
	// VolumeSnapshotClassName is the snapshot.storage.k8s.io VolumeSnapshotClass
	// to use. If empty, the default VolumeSnapshotClass is used.
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`
}

// LocalBackend is reserved for Phase 2 (memory + disk capture to hostPath).
type LocalBackend struct {
	// HostPath is the directory on the node where the snapshot is written.
	HostPath string `json:"hostPath,omitempty"`
}

// S3Backend is reserved for Phase 3 (object-storage export).
type S3Backend struct {
	Bucket               string                 `json:"bucket,omitempty"`
	Region               string                 `json:"region,omitempty"`
	Prefix               string                 `json:"prefix,omitempty"`
	CredentialsSecretRef *SecretObjectReference `json:"credentialsSecretRef,omitempty"`
}

// SecretObjectReference references a Secret in the same namespace.
type SecretObjectReference struct {
	Name string `json:"name"`
}

// SwiftSnapshotBackend selects the backend and carries its configuration.
type SwiftSnapshotBackend struct {
	Type              SnapshotBackendType       `json:"type"`
	CSIVolumeSnapshot *CSIVolumeSnapshotBackend `json:"csiVolumeSnapshot,omitempty"`
	// Local is reserved for Phase 2.
	// +optional
	Local *LocalBackend `json:"local,omitempty"`
	// S3 is reserved for Phase 3.
	// +optional
	S3 *S3Backend `json:"s3,omitempty"`
}

// SwiftSnapshotSpec defines the desired state of a SwiftSnapshot.
type SwiftSnapshotSpec struct {
	// GuestRef points at the SwiftGuest in the same namespace to snapshot.
	GuestRef SwiftSnapshotGuestRef `json:"guestRef"`
	// Backend selects how and where the snapshot is captured.
	Backend SwiftSnapshotBackend `json:"backend"`
	// IncludeMemory requests a memory snapshot in addition to disks. Ignored
	// for the csi-volume-snapshot backend (which is disk-only by definition).
	// +kubebuilder:default=true
	IncludeMemory bool `json:"includeMemory,omitempty"`
	// ResumeAfterSnapshot controls whether the source SwiftGuest is resumed
	// after the snapshot completes (default true). false leaves the VM
	// stopped/paused for operator inspection. Ignored when the source guest
	// was already stopped at snapshot time (csi-volume-snapshot backend
	// stops the VM gracefully and restarts it iff this is true).
	// +kubebuilder:default=true
	ResumeAfterSnapshot bool `json:"resumeAfterSnapshot,omitempty"`
}

// SnapshotDiskRef records one captured disk (root or data) by role + handle.
type SnapshotDiskRef struct {
	// Role is "root" or "data".
	Role string `json:"role"`
	// DiskName is the SwiftGuest's logical disk name (used for "data" role
	// to disambiguate among multiple data disks). Empty for the root disk.
	DiskName string `json:"diskName,omitempty"`
	// SizeBytes is the size at snapshot time.
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// Handle is the backend-specific identifier — for csi-volume-snapshot
	// this is "<namespace>/<volumesnapshot-name>". For local/s3 it is the
	// filesystem path / object key.
	Handle string `json:"handle,omitempty"`
}

// MemorySnapshotRef records the memory portion of a snapshot. Nil when
// IncludeMemory is false or the backend is csi-volume-snapshot.
type MemorySnapshotRef struct {
	SizeBytes int64  `json:"sizeBytes,omitempty"`
	Handle    string `json:"handle,omitempty"`
}

// CapturedGuestSpec preserves the SwiftGuest's relevant spec fields at
// snapshot time so SwiftRestore can validate compatibility on restore.
type CapturedGuestSpec struct {
	CPU       string `json:"cpu,omitempty"`
	MemoryMi  int64  `json:"memoryMi,omitempty"`
	ImageName string `json:"imageName,omitempty"`
}

// SwiftSnapshotStatus is the observed state of a SwiftSnapshot.
type SwiftSnapshotStatus struct {
	Phase             SwiftSnapshotPhase `json:"phase,omitempty"`
	Conditions        []metav1.Condition `json:"conditions,omitempty"`
	CapturedAt        *metav1.Time       `json:"capturedAt,omitempty"`
	Hypervisor        string             `json:"hypervisor,omitempty"`
	HypervisorVersion string             `json:"hypervisorVersion,omitempty"`
	GuestSpec         *CapturedGuestSpec `json:"guestSpec,omitempty"`
	Disks             []SnapshotDiskRef  `json:"disks,omitempty"`
	MemorySnapshot    *MemorySnapshotRef `json:"memorySnapshot,omitempty"`
	TotalSizeBytes    int64              `json:"totalSizeBytes,omitempty"`
}

// SwiftSnapshot is a point-in-time capture of a SwiftGuest's disk (and
// optionally memory) state, for backup/restore and templating workflows.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftsnapshots,scope=Namespaced,shortName=ssnap
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Guest",type=string,JSONPath=`.spec.guestRef.name`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend.type`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.totalSizeBytes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftSnapshotSpec   `json:"spec,omitempty"`
	Status SwiftSnapshotStatus `json:"status,omitempty"`
}

// SwiftSnapshotList contains a list of SwiftSnapshot.
// +kubebuilder:object:root=true
type SwiftSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftSnapshot `json:"items"`
}
