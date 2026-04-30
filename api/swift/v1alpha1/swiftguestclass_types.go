package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DiskFormat is the disk image format.
// +kubebuilder:validation:Enum=raw;qcow2
type DiskFormat string

const (
	DiskFormatRaw   DiskFormat = "raw"
	DiskFormatQcow2 DiskFormat = "qcow2"
)

// RootDiskSpec defines the root disk for a guest.
type RootDiskSpec struct {
	Size   resource.Quantity `json:"size"`
	Format DiskFormat        `json:"format"`
}

// SwiftGuestClassSpec defines the desired state of SwiftGuestClass.
type SwiftGuestClassSpec struct {
	CPU      resource.Quantity `json:"cpu"`
	Memory   resource.Quantity `json:"memory"`
	RootDisk RootDiskSpec      `json:"rootDisk"`
	// Storage is the cluster default for PVCs the SwiftGuest controller
	// creates (today: the root-disk clone PVC). Per-guest overrides on
	// SwiftGuest.spec.storage compose per-field on top of this. Nil/unset
	// keeps the legacy behaviour: ReadWriteOnce + Filesystem, with
	// StorageClassName inherited from the source SwiftImage's PVC.
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`
}

// SwiftGuestClass is the Schema for the swiftguestclasses API.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftguestclasses,scope=Cluster,shortName=sgc
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=`.spec.cpu`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.spec.memory`
// +kubebuilder:printcolumn:name="AccessMode",type=string,JSONPath=`.spec.storage.accessMode`
// +kubebuilder:printcolumn:name="VolumeMode",type=string,JSONPath=`.spec.storage.volumeMode`
type SwiftGuestClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SwiftGuestClassSpec `json:"spec,omitempty"`
}

// SwiftGuestClassList contains a list of SwiftGuestClass.
// +kubebuilder:object:root=true
type SwiftGuestClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGuestClass `json:"items"`
}
