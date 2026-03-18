package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftImagePhase is the phase of a SwiftImage.
// +kubebuilder:validation:Enum=Pending;Importing;Validating;Preparing;Ready;Failed
type SwiftImagePhase string

const (
	SwiftImagePhasePending    SwiftImagePhase = "Pending"
	SwiftImagePhaseImporting  SwiftImagePhase = "Importing"
	SwiftImagePhaseValidating SwiftImagePhase = "Validating"
	SwiftImagePhasePreparing  SwiftImagePhase = "Preparing"
	SwiftImagePhaseReady      SwiftImagePhase = "Ready"
	SwiftImagePhaseFailed     SwiftImagePhase = "Failed"
)

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

// ImageSource defines the source of an image.
type ImageSource struct {
	HTTP     *HTTPSource     `json:"http,omitempty"`
	Upload   *UploadSource   `json:"upload,omitempty"`
	PVCClone *PVCCloneSource `json:"pvcClone,omitempty"`
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
}

// SwiftImageStatus defines the observed state of SwiftImage.
type SwiftImageStatus struct {
	Phase            SwiftImagePhase      `json:"phase,omitempty"`
	Conditions       []metav1.Condition   `json:"conditions,omitempty"`
	PreparedArtifact *PreparedArtifactRef `json:"preparedArtifact,omitempty"`
	SourceFormat     DiskFormat           `json:"sourceFormat,omitempty"`
	PreparedFormat   DiskFormat           `json:"preparedFormat,omitempty"`
}

// SwiftImage is the Schema for the swiftimages API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftimages,scope=Namespaced,shortName=si
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
