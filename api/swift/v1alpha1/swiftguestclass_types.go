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
}

// SwiftGuestClass is the Schema for the swiftguestclasses API.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=swiftguestclasses,scope=Cluster,shortName=sgc
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
