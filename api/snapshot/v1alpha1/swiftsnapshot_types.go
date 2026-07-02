package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SnapshotBackendType selects how the snapshot is captured and stored.
// csi-volume-snapshot (Tier A, disk-only), local (Tier B, node-hostPath
// memory+state), and s3 (Tier C, object-storage export) are shipped; oci
// packages the snapshot as an OCI artifact and pushes it to a registry via
// ORAS (see docs/design/oras-vm-disk-artifacts.md). The spec is structured so
// adding a backend does not require a breaking change.
// +kubebuilder:validation:Enum=csi-volume-snapshot;local;s3;oci
type SnapshotBackendType string

const (
	SnapshotBackendCSIVolumeSnapshot SnapshotBackendType = "csi-volume-snapshot"
	SnapshotBackendLocal             SnapshotBackendType = "local"
	SnapshotBackendS3                SnapshotBackendType = "s3"
	SnapshotBackendOCI               SnapshotBackendType = "oci"
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
	// SwiftSnapshotConditionRetentionBlocked is set True when spec.ttl has
	// elapsed but the snapshot is still referenced (a cloneFromSnapshot
	// SwiftGuest or an in-flight SwiftRestore), so TTL-driven deletion is
	// deferred until the references clear.
	SwiftSnapshotConditionRetentionBlocked = "RetentionBlocked"
)

// SnapshotDeletionPolicy controls whether deleting a SwiftSnapshot also purges
// its backend artifacts.
// +kubebuilder:validation:Enum=Delete;Retain
type SnapshotDeletionPolicy string

const (
	// SnapshotDeletionPolicyDelete purges the backend artifacts (local hostPath
	// / s3 objects) when the SwiftSnapshot is deleted. The default.
	SnapshotDeletionPolicyDelete SnapshotDeletionPolicy = "Delete"
	// SnapshotDeletionPolicyRetain leaves the backend artifacts in place when
	// the SwiftSnapshot is deleted (the finalizer is dropped without a purge),
	// for out-of-band archival.
	SnapshotDeletionPolicyRetain SnapshotDeletionPolicy = "Retain"
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

// S3Backend configures the s3 (Tier C, object-storage export) backend.
type S3Backend struct {
	// Bucket is the S3 bucket. Required.
	Bucket string `json:"bucket,omitempty"`
	// Region is the S3 region (required for AWS; optional for some
	// S3-compatible stores).
	Region string `json:"region,omitempty"`
	// Prefix is an optional key prefix; objects land at
	// <prefix>/<namespace>/<snapshot>/.
	Prefix string `json:"prefix,omitempty"`
	// Endpoint is an S3-compatible endpoint host[:port] (MinIO, Ceph RGW).
	// Empty targets AWS S3.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// ForcePathStyle uses path-style addressing (bucket in the path, not the
	// host) — typically required by MinIO / Ceph RGW.
	// +optional
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`
	// Insecure allows a plaintext (http) endpoint instead of TLS. UNSAFE —
	// credentials and snapshot bytes traverse the network unencrypted. Use only
	// for an in-cluster MinIO / test store on a trusted network. Production S3
	// (AWS, TLS-fronted MinIO/RGW) must leave this false.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// CredentialsSecretRef references a Secret (same namespace) with keys
	// accessKeyId, secretAccessKey, and optional sessionToken.
	CredentialsSecretRef *SecretObjectReference `json:"credentialsSecretRef,omitempty"`
}

// OCIBackend configures the oci (OCI registry / ORAS) backend — the snapshot
// artifacts are packaged as an OCI artifact and pushed to a registry via ORAS.
// The registry is a declared external dependency (Harbor / Zot / distribution /
// a cloud registry), never embedded — KubeSwift is a registry client. Compared
// to the s3 backend this adds content-addressed layer dedup (strongest for a
// shared golden base), cosign/SBOM provenance as OCI referrers, and a single
// artifact store for the edge. See docs/design/oras-vm-disk-artifacts.md.
type OCIBackend struct {
	// Repository is the target OCI repository WITHOUT a tag, e.g.
	// "ghcr.io/kubeswift-io/vm-snapshots" or "zot.registry.svc:5000/snapshots".
	// Required. The snapshot artifact is pushed to Repository:Tag.
	Repository string `json:"repository,omitempty"`
	// Tag is the artifact tag. Empty defaults to "<namespace>-<snapshot>", a
	// stable per-snapshot tag; restore reads the artifact from Repository:Tag
	// but pins it by digest (status.oci.manifestDigest).
	// +optional
	Tag string `json:"tag,omitempty"`
	// Insecure allows a plaintext (http) registry instead of TLS. UNSAFE —
	// credentials and snapshot bytes traverse the network unencrypted. Use only
	// for an in-cluster Zot / test registry on a trusted network. Production
	// registries (ghcr.io, TLS-fronted Harbor / Zot) must leave this false.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// CredentialsSecretRef references a Secret (same namespace) holding registry
	// credentials — a kubernetes.io/dockerconfigjson Secret (key
	// .dockerconfigjson), the same pull-secret shape SwiftKernel uses. Empty
	// means anonymous access (an unauthenticated / in-cluster registry).
	// +optional
	CredentialsSecretRef *SecretObjectReference `json:"credentialsSecretRef,omitempty"`

	// SigningKeySecretRef, when set, makes the push cosign-sign the artifact as
	// an OCI referrer (discoverable via `oras discover` / `cosign verify`),
	// extending the supply-chain spine to the disk artifact. The referenced
	// Secret (same namespace) must hold a cosign keypair: keys `cosign.key`
	// (the encrypted private key) and `cosign.password`. Key-based (not keyless)
	// — an in-cluster capture has no CI OIDC identity, and a key-based signature
	// verifies offline for sovereign/air-gapped edge. Signing is strict: if it is
	// requested and fails, the snapshot Fails (no unsigned artifact is left
	// behind as if signed). NOTE: cosign verification of a referrer-mode
	// signature requires a TLS registry; a plaintext (insecure) registry can
	// carry the referrer but cosign verify against it is unsupported by cosign.
	// +optional
	SigningKeySecretRef *SecretObjectReference `json:"signingKeySecretRef,omitempty"`
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
	// OCI configures the oci (OCI registry / ORAS) backend.
	// +optional
	OCI *OCIBackend `json:"oci,omitempty"`
}

// SwiftSnapshotSpec defines the desired state of a SwiftSnapshot.
type SwiftSnapshotSpec struct {
	// GuestRef points at the SwiftGuest in the same namespace to snapshot.
	GuestRef SwiftSnapshotGuestRef `json:"guestRef"`
	// Backend selects how and where the snapshot is captured.
	Backend SwiftSnapshotBackend `json:"backend"`
	// IncludeMemory requests a memory snapshot in addition to disks. NOTE: the
	// captured set is backend-determined, not controlled by this flag —
	// local/s3 use a Cloud Hypervisor vm.snapshot, which ALWAYS includes memory,
	// while csi-volume-snapshot is disk-only by definition. So includeMemory:false
	// is a no-op on every backend (the webhook surfaces a warning for local/s3);
	// for a truly disk-only snapshot, use the csi-volume-snapshot backend. The
	// field remains for forward compatibility and manifest metadata.
	// +kubebuilder:default=true
	IncludeMemory bool `json:"includeMemory,omitempty"`
	// IncludeDisk requests the guest's disk be captured to the registry alongside
	// memory, producing a FULL-STATE (cold / suspended-state migration) snapshot
	// the import can resume in another cluster. Only valid with backend.type: oci
	// + includeMemory: true (the webhook enforces this). When set, the capture is
	// coherent via capture-then-terminate: the guest is paused, memory
	// snapshotted, NOT resumed, then terminated so the disk is frozen at the
	// snapshot instant before it is chunked to the registry — so this IMPLIES the
	// guest is migrated away, not a live backup. See
	// docs/design/oras-cold-migration.md (P4).
	// +optional
	IncludeDisk bool `json:"includeDisk,omitempty"`
	// ResumeAfterSnapshot controls whether the source SwiftGuest is resumed
	// after the snapshot completes (default true). false leaves the VM
	// stopped/paused for operator inspection. Ignored when the source guest
	// was already stopped at snapshot time (csi-volume-snapshot backend
	// stops the VM gracefully and restarts it iff this is true).
	// +kubebuilder:default=true
	ResumeAfterSnapshot bool `json:"resumeAfterSnapshot,omitempty"`

	// DeletionPolicy controls whether deleting this SwiftSnapshot also purges
	// its backend artifacts. Delete (default) purges the local hostPath / s3
	// objects; Retain leaves them in place (the cleanup finalizer is dropped
	// without a purge) for out-of-band archival. Ignored for
	// csi-volume-snapshot — the VolumeSnapshotClass deletionPolicy governs the
	// underlying VolumeSnapshot.
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy SnapshotDeletionPolicy `json:"deletionPolicy,omitempty"`

	// TTL, when set, makes the controller delete this SwiftSnapshot once
	// status.capturedAt + ttl has elapsed; the normal deletion path then runs,
	// honoring deletionPolicy. The controller refuses to delete a snapshot
	// still referenced by a cloneFromSnapshot SwiftGuest or an in-flight
	// SwiftRestore — it sets a RetentionBlocked condition and retries until the
	// references clear. An operator-initiated `kubectl delete` is never blocked.
	// Unset means keep until deleted by hand.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`
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

	// The fields below are the launcher-sufficient surface for a
	// source-independent (fully cross-cluster) full-state clone: captured
	// while the source is live so a clone can resume when the source
	// SwiftGuest / SwiftImage / SwiftSeedProfile are gone. Populated only for
	// full-state (includeDisk oci) captures; a memory-only snapshot leaves
	// them empty and its clone still needs the live source spec. See
	// docs/design/oras-cold-migration-source-independent.md.

	// Storage is the resolved storage settings for the clone's root PVC
	// (materialized from the oci disk artifact).
	// +optional
	Storage *CapturedStorage `json:"storage,omitempty"`
	// RootDiskSize is the resolved root disk size (e.g. "40Gi").
	// +optional
	RootDiskSize string `json:"rootDiskSize,omitempty"`
	// Network reports whether the source had tap+bridge networking.
	// +optional
	Network bool `json:"network,omitempty"`
	// InterfaceNames are the source's NIC names, used to seed deterministic
	// per-clone MAC rewrites when the source guest is gone.
	// +optional
	InterfaceNames []string `json:"interfaceNames,omitempty"`
	// GuestAgent reports whether the source opted into the in-guest vsock
	// identity agent.
	// +optional
	GuestAgent bool `json:"guestAgent,omitempty"`
	// OSType is the source OS family ("linux" or "windows").
	// +optional
	OSType string `json:"osType,omitempty"`
	// HasSeed reports whether the source had a SwiftSeedProfile — the clone
	// launcher must then synthesize a placeholder seed.iso so CH --restore can
	// open the seed disk recorded in config.json (a resume never reads it).
	// +optional
	HasSeed bool `json:"hasSeed,omitempty"`
	// HasDataDisks reports whether the source had secondary data disks.
	// Source-independent import rejects such snapshots when the data-disk
	// artifacts are absent (pre-v1.1 captures); v1.1 full-state captures
	// carry them (DataDisks below + status.oci.dataDisks).
	// +optional
	HasDataDisks bool `json:"hasDataDisks,omitempty"`
	// DataDisks is the launcher-sufficient shape of the source's secondary VM
	// data disks (v1.1 full-state capture: blank + attachAsDisk disks; a source
	// with image-backed data disks is rejected for includeDisk). Import
	// materializes each from its oci artifact into a guest-owned PVC and
	// attaches it under the same disk name, so CH device paths — which derive
	// from the disk name — match the captured config.json.
	// +optional
	DataDisks []CapturedDataDisk `json:"dataDisks,omitempty"`
}

// CapturedDataDisk is one secondary VM data disk frozen at capture.
type CapturedDataDisk struct {
	// Name is the disk's stable identifier (drives the CH device path).
	Name string `json:"name"`
	// Size is the backing PVC's requested size (e.g. "20Gi") — the
	// materialized clone PVC is created at this size.
	Size string `json:"size,omitempty"`
	// Block is true for a raw block disk (volumeDevices), false for a
	// filesystem-backed image.raw disk.
	// +optional
	Block bool `json:"block,omitempty"`
	// PVCName is the SOURCE-cluster PVC backing this disk — capture-side
	// only (the chunk Job mounts it); meaningless to the import side.
	// +optional
	PVCName string `json:"pvcName,omitempty"`
}

// CapturedStorage is the resolved storage shape frozen at capture for a
// source-independent clone's root PVC.
type CapturedStorage struct {
	// +optional
	AccessMode string `json:"accessMode,omitempty"`
	// +optional
	VolumeMode string `json:"volumeMode,omitempty"`
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
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

	// NodeName is the Kubernetes node where the snapshot was taken.
	// Only set for the local backend (Tier B) — local snapshots live in
	// hostPath storage on a single node, and SwiftRestore uses this to
	// pin the restore-receive launcher pod to the same node.
	NodeName string `json:"nodeName,omitempty"`

	// ObservedPauseWindowMs is the wall-clock duration the source VM
	// was paused during capture, measured by swiftletd at the action
	// handler. Set on a successful local-backend capture; absent for
	// csi-volume-snapshot (which does not pause the VM).
	ObservedPauseWindowMs int64 `json:"observedPauseWindowMs,omitempty"`

	// SnapshotDirVersion is the on-disk format version of the snapshot
	// directory, set at capture time. Currently always "v1". The
	// SwiftRestore controller's hypervisor-version check (architect
	// risk #3) compares this alongside major.minor of the CH version
	// before allowing a restore to proceed.
	SnapshotDirVersion string `json:"snapshotDirVersion,omitempty"`

	// S3 records the object-storage location of an s3-backend export
	// (Phase 3 / Tier C). Set when the Uploading phase completes.
	// +optional
	S3 *S3SnapshotStatus `json:"s3,omitempty"`

	// OCI records the registry location of an oci-backend export. Set when the
	// Uploading (push) phase completes.
	// +optional
	OCI *OCISnapshotStatus `json:"oci,omitempty"`
}

// S3SnapshotStatus records where an s3-backend snapshot was exported and how to
// verify it. The manifest at Location+"/manifest.json" is the source of truth
// for restore; ManifestDigest pins the exact manifest the restore must match.
type S3SnapshotStatus struct {
	// Location is the s3 URI of the snapshot prefix (s3://bucket/key/).
	Location string `json:"location,omitempty"`
	// ManifestDigest is the sha256 of the uploaded manifest.json.
	ManifestDigest string `json:"manifestDigest,omitempty"`
	// UploadedBytes is the snapshot's total artifact footprint in S3 (read from
	// the upload Job's byte report). This is the snapshot's S3 size, not the
	// per-Job wire traffic — a resumed upload that skipped already-present
	// objects still reports the full footprint. Wire traffic is the
	// kubeswift_snapshot_upload_bytes_total metric.
	UploadedBytes int64 `json:"uploadedBytes,omitempty"`
	// UploadedAt is when the Uploading phase completed.
	UploadedAt *metav1.Time `json:"uploadedAt,omitempty"`
}

// OCISnapshotStatus records where an oci-backend snapshot was pushed and how to
// verify it. Reference pins the human-readable tag; ManifestDigest pins the
// exact content-addressed artifact the restore must pull (Repository@digest),
// so a retag cannot silently swap the bytes underneath a restore.
type OCISnapshotStatus struct {
	// Reference is the pushed artifact reference (repository:tag).
	Reference string `json:"reference,omitempty"`
	// ManifestDigest is the sha256 digest of the pushed OCI manifest. Restore
	// pulls Repository@ManifestDigest so the exact artifact is pinned.
	ManifestDigest string `json:"manifestDigest,omitempty"`
	// PushedBytes is the snapshot's total artifact footprint in the registry
	// (from the push Job's byte report) — the full footprint, not the per-Job
	// wire traffic (a resumed push that deduped already-present layers still
	// reports the full footprint). Wire traffic is a metric.
	PushedBytes int64 `json:"pushedBytes,omitempty"`
	// PushedAt is when the Uploading (push) phase completed.
	PushedAt *metav1.Time `json:"pushedAt,omitempty"`
	// Signed is true when the push cosign-signed the artifact as an OCI referrer
	// (spec.backend.oci.signingKeySecretRef was set and signing succeeded).
	// +optional
	Signed bool `json:"signed,omitempty"`
	// Disk is the chunked disk artifact pushed alongside memory for a full-state
	// (cold-migration) snapshot — populated only when spec.includeDisk is true.
	// The import materializes the disk from this ref, then CH --restore's the
	// memory (the fields above) against it. See docs/design/oras-cold-migration.md.
	// +optional
	Disk *OCIDiskArtifact `json:"disk,omitempty"`
	// DataDisks records the chunked artifacts of the source's secondary VM
	// data disks (v1.1 full-state capture), one per captured disk, matched to
	// status.guestSpec.dataDisks by Name. The import materializes each into a
	// guest-owned PVC and attaches it under the same disk name.
	// +optional
	DataDisks []OCIDataDiskArtifact `json:"dataDisks,omitempty"`
}

// OCIDataDiskArtifact is one secondary data disk's chunked artifact.
type OCIDataDiskArtifact struct {
	// Name matches the captured disk's name (status.guestSpec.dataDisks).
	Name string `json:"name"`
	// Reference is the pushed artifact reference (repository:tag).
	Reference string `json:"reference,omitempty"`
	// ManifestDigest pins the artifact for the import.
	ManifestDigest string `json:"manifestDigest,omitempty"`
	// PushedBytes is the artifact's footprint in the registry.
	PushedBytes int64 `json:"pushedBytes,omitempty"`
}

// OCIDiskArtifact records the chunked disk artifact of a full-state oci snapshot
// (P4). It is the P3 golden-image chunk format applied to the guest's frozen root
// disk; the import pulls Repository@ManifestDigest and reassembles it.
type OCIDiskArtifact struct {
	// Reference is the pushed disk artifact reference (repository:tag).
	Reference string `json:"reference,omitempty"`
	// ManifestDigest pins the disk artifact (Repository@digest) for the import.
	ManifestDigest string `json:"manifestDigest,omitempty"`
	// PushedBytes is the disk artifact's footprint in the registry.
	PushedBytes int64 `json:"pushedBytes,omitempty"`
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
