package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftImagePhase is the phase of a SwiftImage.
// +kubebuilder:validation:Enum=Pending;Importing;Validating;Preparing;Snapshotting;Ready;Failed
type SwiftImagePhase string

const (
	SwiftImagePhasePending      SwiftImagePhase = "Pending"
	SwiftImagePhaseImporting    SwiftImagePhase = "Importing"
	SwiftImagePhaseValidating   SwiftImagePhase = "Validating"
	SwiftImagePhasePreparing    SwiftImagePhase = "Preparing"
	SwiftImagePhaseSnapshotting SwiftImagePhase = "Snapshotting"
	SwiftImagePhaseReady        SwiftImagePhase = "Ready"
	SwiftImagePhaseFailed       SwiftImagePhase = "Failed"
)

// CloneStrategy controls how per-guest root disk PVCs are created from a
// SwiftImage. See docs/design/snapshots.md "SwiftImage Clone Strategy".
// +kubebuilder:validation:Enum=copy;snapshot
type CloneStrategy string

const (
	// CloneStrategyCopy is the legacy path: each SwiftGuest gets a Copy Job
	// that runs cp + qemu-img resize + sgdisk -e. Slow but works on any
	// CSI driver, including non-snapshot-capable ones (local-path, NFS).
	CloneStrategyCopy CloneStrategy = "copy"
	// CloneStrategySnapshot uses CSI VolumeSnapshot + dataSource clones for
	// per-guest root disks. Requires a snapshot-capable CSI driver and a
	// VolumeSnapshotClass. Substantially faster than copy on most drivers.
	CloneStrategySnapshot CloneStrategy = "snapshot"
)

// CloneSeedKind selects which kind of resource serves as the per-guest
// clone seed. Only VolumeSnapshot is implemented in Phase 1.
// +kubebuilder:validation:Enum=VolumeSnapshot
type CloneSeedKind string

const (
	CloneSeedKindVolumeSnapshot CloneSeedKind = "VolumeSnapshot"
)

// CloneSeed describes the resource used as the dataSource for per-guest
// clone PVCs derived from this SwiftImage. Populated by the SwiftImage
// controller when CloneStrategy is "snapshot" and the snapshot is ready.
type CloneSeed struct {
	// Kind is "VolumeSnapshot" for the Phase 1 csi-volume-snapshot path.
	Kind CloneSeedKind `json:"kind"`
	// Name of the seed resource, in the same namespace as the SwiftImage.
	// Same-namespace is enforced — see Phase 0 spike finding §6a.
	Name string `json:"name"`
	// Namespace of the seed resource. Always equal to the SwiftImage's
	// namespace; recorded for callers that consume CloneSeed without
	// re-resolving the SwiftImage.
	Namespace string `json:"namespace"`
	// SourceSizeBytes is the size of the seed's underlying volume. Phase 1
	// uses this to pick the clone PVC's initial requested size (must equal
	// source size on Longhorn; see Phase 0 §5).
	SourceSizeBytes int64 `json:"sourceSizeBytes,omitempty"`
}

// HTTPSource specifies an HTTP(S) URL to fetch the image from.
type HTTPSource struct {
	URL string `json:"url"`
}

// PVCCloneSource specifies a source PVC to clone from.
type PVCCloneSource struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// UploadSource is a placeholder for future upload support. Not yet implemented.
type UploadSource struct {
	// Placeholder; upload UX to be defined later.
}

// SecretObjectReference names a Secret in the SwiftImage's namespace. Mirrors
// the snapshot API's type so the OCI source reads symmetrically with the OCI
// snapshot backend.
type SecretObjectReference struct {
	// Name of the Secret.
	Name string `json:"name"`
}

// OCIImageSource pulls a golden VM disk artifact from an OCI registry (P3). The
// artifact is a single raw disk layer (artifactType
// application/vnd.kubeswift.vmimage.v1, layer title image.raw); the import
// materializes it into the import PVC and runs the shared resize + sgdisk +
// GRUB/serial patch tail — identical to the http path minus the download step.
// See docs/design/oras-golden-image.md.
type OCIImageSource struct {
	// Repository is the OCI repository WITHOUT a tag
	// (e.g. ghcr.io/org/golden-ubuntu-noble).
	Repository string `json:"repository"`
	// Tag selects the artifact by tag. Mutually exclusive with Digest.
	// +optional
	Tag string `json:"tag,omitempty"`
	// Digest pins the artifact by manifest digest (sha256:...). Mutually
	// exclusive with Tag; recommended for reproducibility (a tag is mutable).
	// +optional
	Digest string `json:"digest,omitempty"`
	// Insecure allows a plaintext (http) registry. UNSAFE — in-cluster / test
	// registry only.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// CredentialsSecretRef references a kubernetes.io/dockerconfigjson Secret
	// (same namespace) for registry auth. Empty = anonymous.
	// +optional
	CredentialsSecretRef *SecretObjectReference `json:"credentialsSecretRef,omitempty"`
	// VerifyKeySecretRef references a Secret (same namespace) holding a cosign
	// PUBLIC key under the key "cosign.pub". When set, the import verifies the
	// artifact's cosign signature (as produced by `swiftctl image publish
	// --sign-key`) BEFORE trusting its bytes, and FAILS the import if the
	// signature does not verify — no unsigned/tampered golden disk is imported.
	// Requires a TLS registry: cosign verify does not support a plaintext (http)
	// registry, so verifyKeySecretRef together with insecure is rejected at
	// admission.
	// +optional
	VerifyKeySecretRef *SecretObjectReference `json:"verifyKeySecretRef,omitempty"`
}

// ImageSource defines the source of an image.
type ImageSource struct {
	HTTP     *HTTPSource     `json:"http,omitempty"`
	Upload   *UploadSource   `json:"upload,omitempty"`
	PVCClone *PVCCloneSource `json:"pvcClone,omitempty"`
	// OCI pulls a golden raw disk artifact from an OCI registry (P3).
	// +optional
	OCI *OCIImageSource `json:"oci,omitempty"`
}

// PVCObjectReference references a PVC.
type PVCObjectReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// PreparedArtifactRef references the prepared runtime artifact.
type PreparedArtifactRef struct {
	PVCRef *PVCObjectReference `json:"pvcRef,omitempty"`
	Format DiskFormat          `json:"format"`
	Size   *resource.Quantity  `json:"size,omitempty"`
}

// OSType is the operating-system family the image contains: "linux"
// (default) or "windows". It gates the Linux-only import steps (GRUB/serial
// patch, growpart resize expectation) in the import pipeline. For a Windows
// image the operator supplies a virtio-ready disk (viostor pre-installed); the
// import keeps qcow2->raw + resize but skips the Linux-only steps. Default
// linux. (Defined per-API-group to avoid a cross-group import; mirrors
// api/swift/v1alpha1.OSType.)
// +kubebuilder:validation:Enum=linux;windows
type OSType string

const (
	OSTypeLinux   OSType = "linux"
	OSTypeWindows OSType = "windows"
)

// DiskFormat is the disk image format.
// +kubebuilder:validation:Enum=raw;qcow2
type DiskFormat string

const (
	DiskFormatRaw   DiskFormat = "raw"
	DiskFormatQcow2 DiskFormat = "qcow2"
)

// SwiftImageRootDiskSpec specifies root disk options for the import PVC.
type SwiftImageRootDiskSpec struct {
	// Size is the requested storage for the import PVC. Defaults to 10Gi if not set.
	// Should match or exceed SwiftGuestClass.rootDisk.size for guests using this image.
	Size *resource.Quantity `json:"size,omitempty"`
}

// SwiftImageSpec defines the desired state of SwiftImage.
type SwiftImageSpec struct {
	Source   ImageSource             `json:"source"`
	Format   DiskFormat              `json:"format"`
	RootDisk *SwiftImageRootDiskSpec `json:"rootDisk,omitempty"`
	// OSType is the OS family this image contains: "linux" (default) or
	// "windows". Gates the Linux-only import steps (GRUB/serial patch,
	// growpart resize expectation). Default linux; existing images are
	// unaffected. (Windows guest support — see docs/design/windows-guest-support.md.)
	// +kubebuilder:default=linux
	// +optional
	OSType OSType `json:"osType,omitempty"`
	// CloneStrategy controls how per-guest root disk PVCs are produced.
	// Defaults to "copy" for backward compatibility — existing images keep
	// working without changes. "snapshot" requires a snapshot-capable CSI
	// driver (see docs/images/clone-strategies.md for the validated list).
	// +kubebuilder:default=copy
	// +optional
	CloneStrategy CloneStrategy `json:"cloneStrategy,omitempty"`
	// CloneStorageClassName overrides the storage class used for per-guest
	// clone PVCs. Defaults to the import PVC's storage class. Most CSI
	// drivers cannot snapshot/clone across storage classes, so changing
	// this on a snapshot-strategy image is generally an operator error and
	// will surface as a provisioning failure.
	// +optional
	CloneStorageClassName string `json:"cloneStorageClassName,omitempty"`
	// VolumeSnapshotClassName names the snapshot.storage.k8s.io
	// VolumeSnapshotClass used when CloneStrategy is "snapshot". Required
	// for that strategy; ignored otherwise. The validation webhook
	// rejects "snapshot" without this field set.
	// +optional
	VolumeSnapshotClassName string `json:"volumeSnapshotClassName,omitempty"`
}

// SwiftImageStatus defines the observed state of SwiftImage.
type SwiftImageStatus struct {
	Phase            SwiftImagePhase      `json:"phase,omitempty"`
	Conditions       []metav1.Condition   `json:"conditions,omitempty"`
	PreparedArtifact *PreparedArtifactRef `json:"preparedArtifact,omitempty"`
	SourceFormat     DiskFormat           `json:"sourceFormat,omitempty"`
	PreparedFormat   DiskFormat           `json:"preparedFormat,omitempty"`
	// CloneSeed describes the resource per-guest cloning will reference.
	// Populated when CloneStrategy is "snapshot" and the seed VolumeSnapshot
	// reaches readyToUse=true. Nil for the legacy copy strategy.
	CloneSeed *CloneSeed `json:"cloneSeed,omitempty"`
	// SizeHint is an internal field used to pass measured size between Validating and Preparing phases.
	SizeHint int64 `json:"sizeHint,omitempty"`
}

// SwiftImage is the Schema for the swiftimages API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftimages,scope=Namespaced,shortName=si
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.preparedArtifact.size`
// +kubebuilder:printcolumn:name="Format",type=string,JSONPath=`.status.preparedFormat`,priority=1
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.cloneStrategy`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SwiftImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftImageSpec   `json:"spec,omitempty"`
	Status SwiftImageStatus `json:"status,omitempty"`
}

// SwiftImageList contains a list of SwiftImage.
// +kubebuilder:object:root=true
type SwiftImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftImage `json:"items"`
}
