package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwiftKernelPhase is the phase of a SwiftKernel.
// +kubebuilder:validation:Enum=Pending;Pulling;Ready;Failed
type SwiftKernelPhase string

const (
	SwiftKernelPhasePending SwiftKernelPhase = "Pending"
	SwiftKernelPhasePulling SwiftKernelPhase = "Pulling"
	SwiftKernelPhaseReady   SwiftKernelPhase = "Ready"
	SwiftKernelPhaseFailed  SwiftKernelPhase = "Failed"
)

// OCIRef references an OCI artifact containing kernel artifacts.
type OCIRef struct {
	Image      string `json:"image"`
	PullSecret string `json:"pullSecret,omitempty"`
}

// SwiftKernelSpec defines the desired state of SwiftKernel.
type SwiftKernelSpec struct {
	OCIRef        OCIRef `json:"ociRef"`
	KernelCmdline string `json:"kernelCmdline,omitempty"`
	Profile       string `json:"profile,omitempty"`
}

// SwiftKernelStatus defines the observed state of SwiftKernel.
type SwiftKernelStatus struct {
	Phase           SwiftKernelPhase   `json:"phase,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
	LocalPath       string             `json:"localPath,omitempty"`
	KernelDigest    string             `json:"kernelDigest,omitempty"`
	InitramfsDigest string             `json:"initramfsDigest,omitempty"`
}

// SwiftKernel is the Schema for the swiftkernels API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftkernels,scope=Namespaced,shortName=sk
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=".spec.profile"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type SwiftKernel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftKernelSpec   `json:"spec,omitempty"`
	Status SwiftKernelStatus `json:"status,omitempty"`
}

// SwiftKernelList contains a list of SwiftKernel.
// +kubebuilder:object:root=true
type SwiftKernelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftKernel `json:"items"`
}
