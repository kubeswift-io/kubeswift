package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/kubeswift-io/kubeswift/api/storage/v1alpha1"
)

// SwiftImagePublishPhase is where a publish request is.
// +kubebuilder:validation:Enum=Pending;Publishing;Ready;Failed
type SwiftImagePublishPhase string

const (
	// SwiftImagePublishPending: waiting for its source, its destination or a
	// free publish slot (see the Ready condition's reason).
	SwiftImagePublishPending SwiftImagePublishPhase = "Pending"
	// SwiftImagePublishPublishing: the publish Job is pushing the disk.
	SwiftImagePublishPublishing SwiftImagePublishPhase = "Publishing"
	// SwiftImagePublishReady: the image is in the registry (and the SwiftImage
	// createImage asked for exists).
	SwiftImagePublishReady SwiftImagePublishPhase = "Ready"
	// SwiftImagePublishFailed: terminal; create a new request to retry.
	SwiftImagePublishFailed SwiftImagePublishPhase = "Failed"
)

// ImagePublishSource is the disk a SwiftImagePublish publishes. Exactly one
// field is set.
type ImagePublishSource struct {
	// SnapshotRef names a Ready SwiftSnapshot in this namespace whose backend
	// is csi-volume-snapshot. The root disk's VolumeSnapshot is restored into
	// a temporary PVC, published, and the PVC deleted. The source guest may be
	// running or gone; the disk is crash-consistent.
	// +optional
	SnapshotRef *corev1.LocalObjectReference `json:"snapshotRef,omitempty"`
}

// ImagePublishDestination is where the image is pushed. Either a storage
// location (locationRef, or the namespace's or cluster's default) plus
// image, or an explicit repository with its own connection settings.
// +kubebuilder:validation:XValidation:rule="!(has(self.locationRef) && has(self.repository))",message="locationRef and repository are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.repository) != has(self.image)",message="set image (with a storage location) or repository (explicit), not both"
// +kubebuilder:validation:XValidation:rule="has(self.repository) || !((has(self.insecure) && self.insecure) || has(self.caBundle) || has(self.credentialsSecretRef) || has(self.signingKeySecretRef) || has(self.verifyKeySecretRef))",message="insecure, caBundle, credentialsSecretRef, signingKeySecretRef and verifyKeySecretRef go with an explicit repository; a storage location carries its own"
type ImagePublishDestination struct {
	// LocationRef names the storage location to publish to. Unset: the
	// namespace's default SwiftStorageLocation, else the cluster's default
	// SwiftClusterStorageLocation. The image goes to the location's
	// repository plus <namespace>/images/<image> (a cluster location) or
	// images/<image> (a namespace location).
	// +optional
	LocationRef *storagev1alpha1.StorageLocationRef `json:"locationRef,omitempty"`

	// Image is the image's name under the location's repository: lower-case
	// path components.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`
	// +optional
	Image string `json:"image,omitempty"`

	// Repository is the full repository to push to (no tag), instead of a
	// storage location. Its connection settings are the fields below.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9.-]+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+$`
	// +optional
	Repository string `json:"repository,omitempty"`

	// Tag is the tag the image is pushed under.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`
	Tag string `json:"tag"`

	// OverwriteTag allows moving Tag when it already names a different image.
	// By default such a publish fails with TagInUse and the tag is left
	// alone; publishing the same image again is never a conflict.
	// +optional
	OverwriteTag bool `json:"overwriteTag,omitempty"`

	// Insecure allows a plaintext (http) registry. Explicit repository only.
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// CABundle is a PEM bundle trusted for the registry, on top of the system
	// roots. Explicit repository only.
	// +optional
	CABundle string `json:"caBundle,omitempty"`

	// CredentialsSecretRef names a kubernetes.io/dockerconfigjson Secret in
	// this namespace. Explicit repository only.
	// +optional
	CredentialsSecretRef *SecretObjectReference `json:"credentialsSecretRef,omitempty"`

	// SigningKeySecretRef names a Secret in this namespace holding cosign.key
	// (and cosign.password). When set, the image is signed. Explicit
	// repository only; a location signs with its own signing key.
	// +optional
	SigningKeySecretRef *SecretObjectReference `json:"signingKeySecretRef,omitempty"`

	// VerifyKeySecretRef names a Secret in this namespace holding cosign.pub,
	// given to the SwiftImage createImage makes. Explicit repository only.
	// +optional
	VerifyKeySecretRef *SecretObjectReference `json:"verifyKeySecretRef,omitempty"`
}

// ImagePublishCreateImage asks for a SwiftImage of the published image.
type ImagePublishCreateImage struct {
	// Name of the SwiftImage, created in this namespace. It pulls the pushed
	// digest (not the tag) with the destination's connection settings and
	// verify key. Publishing fails if a SwiftImage of that name exists and was
	// not created by this request.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// SwiftImagePublishSpec is a one-shot request: it cannot change once
// created. Deleting it never deletes the published image.
// +kubebuilder:validation:XValidation:rule="has(self.source.snapshotRef)",message="source must set snapshotRef"
type SwiftImagePublishSpec struct {
	// Source is the disk to publish.
	Source ImagePublishSource `json:"source"`

	// Destination is where to push it.
	Destination ImagePublishDestination `json:"destination"`

	// OSType is recorded in the image's config: linux or windows. Unset: the
	// source guest's image's osType when the guest still exists, else linux.
	// +kubebuilder:validation:Enum=linux;windows
	// +optional
	OSType OSType `json:"osType,omitempty"`

	// StorageClassName is the class of the temporary PVC a snapshot is
	// restored into. Unset: the source guest's root disk class, which works
	// while that guest exists.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// CreateImage, when set, creates a SwiftImage of the published image.
	// +optional
	CreateImage *ImagePublishCreateImage `json:"createImage,omitempty"`
}

// ImagePublishDestinationStatus is the destination as resolved, once, when
// the request started.
type ImagePublishDestinationStatus struct {
	// Location is "Explicit" or <Kind>/<name> of the storage location used.
	Location string `json:"location"`
	// Repository is the repository pushed to (no tag).
	Repository string `json:"repository"`
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// +optional
	CABundle string `json:"caBundle,omitempty"`
	// +optional
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`
	// +optional
	SigningKeySecretName string `json:"signingKeySecretName,omitempty"`
	// VerifyKey is the location's inline cosign public key, when it has one.
	// +optional
	VerifyKey string `json:"verifyKey,omitempty"`
	// +optional
	VerifyKeySecretName string `json:"verifyKeySecretName,omitempty"`
}

// ImagePublishSourceStatus is the source as resolved.
type ImagePublishSourceStatus struct {
	// Kind and Name of the source object; UID ties the provenance to it.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// +optional
	UID string `json:"uid,omitempty"`
	// VolumeSnapshot restored, for a snapshot source.
	// +optional
	VolumeSnapshot string `json:"volumeSnapshot,omitempty"`
	// PVCName is the volume the publish Job read.
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// Block is true when that volume is a raw block device.
	// +optional
	Block bool `json:"block,omitempty"`
	// SizeBytes is the disk's size.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// OSType recorded in the image's config.
	// +optional
	OSType OSType `json:"osType,omitempty"`
}

// SwiftImagePublishStatus reports the publish.
type SwiftImagePublishStatus struct {
	// +optional
	Phase SwiftImagePublishPhase `json:"phase,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Destination as resolved when the request started.
	// +optional
	Destination *ImagePublishDestinationStatus `json:"destination,omitempty"`
	// Source as resolved.
	// +optional
	Source *ImagePublishSourceStatus `json:"source,omitempty"`
	// Reference is <repository>:<tag>.
	// +optional
	Reference string `json:"reference,omitempty"`
	// Digest is the pushed manifest's digest.
	// +optional
	Digest string `json:"digest,omitempty"`
	// SizeBytes is the image's disk size (its totalSize).
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// TransferredBytes is what was uploaded; chunks the registry already had,
	// and all-zero chunks, are not.
	// +optional
	TransferredBytes int64 `json:"transferredBytes,omitempty"`
	// Signed is true when the image was signed.
	// +optional
	Signed bool `json:"signed,omitempty"`
	// Image is the SwiftImage createImage made.
	// +optional
	Image string `json:"image,omitempty"`
	// StartTime is when the publish Job was created.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is when the request became Ready or Failed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// SwiftImagePublish publishes a disk as a golden image in an OCI registry.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftimagepublishes,scope=Namespaced,shortName=sip
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Reference",type=string,JSONPath=`.status.reference`
// +kubebuilder:printcolumn:name="Signed",type=boolean,JSONPath=`.status.signed`,priority=1
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="spec is immutable; create a new SwiftImagePublish"
type SwiftImagePublish struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftImagePublishSpec   `json:"spec"`
	Status SwiftImagePublishStatus `json:"status,omitempty"`
}

// SwiftImagePublishList contains a list of SwiftImagePublish.
// +kubebuilder:object:root=true
type SwiftImagePublishList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftImagePublish `json:"items"`
}
