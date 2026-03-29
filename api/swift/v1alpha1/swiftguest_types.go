package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPolicy defines the desired run state of a guest.
// +kubebuilder:validation:Enum=Running;Stopped;RestartOnFailure;Always
type RunPolicy string

const (
	RunPolicyRunning          RunPolicy = "Running"
	RunPolicyStopped          RunPolicy = "Stopped"
	RunPolicyRestartOnFailure RunPolicy = "RestartOnFailure"
	RunPolicyAlways           RunPolicy = "Always"
)

// SwiftGuestPhase is the phase of a SwiftGuest.
// +kubebuilder:validation:Enum=Pending;Scheduling;Running;Stopped;Failed
type SwiftGuestPhase string

const (
	SwiftGuestPhasePending    SwiftGuestPhase = "Pending"
	SwiftGuestPhaseScheduling SwiftGuestPhase = "Scheduling"
	SwiftGuestPhaseRunning    SwiftGuestPhase = "Running"
	SwiftGuestPhaseStopped    SwiftGuestPhase = "Stopped"
	SwiftGuestPhaseFailed     SwiftGuestPhase = "Failed"
)

// SwiftGuestSpec defines the desired state of SwiftGuest.
type SwiftGuestSpec struct {
	ImageRef       *corev1.LocalObjectReference `json:"imageRef,omitempty"`
	KernelRef      *corev1.LocalObjectReference `json:"kernelRef,omitempty"`
	KernelCmdline  string                       `json:"kernelCmdline,omitempty"`
	GuestClassRef  corev1.LocalObjectReference  `json:"guestClassRef"`
	SeedProfileRef *corev1.LocalObjectReference `json:"seedProfileRef,omitempty"`
	RunPolicy      RunPolicy                    `json:"runPolicy,omitempty"`
}

// GuestRuntimeStatus holds runtime process information.
type GuestRuntimeStatus struct {
	PID        int64  `json:"pid,omitempty"`
	Hypervisor string `json:"hypervisor,omitempty"`
}

// GuestConsoleStatus holds console access information.
type GuestConsoleStatus struct {
	SerialSocket string `json:"serialSocket,omitempty"`
}

// GuestNetworkInterface represents a single network interface with its IP.
type GuestNetworkInterface struct {
	Name string `json:"name,omitempty"`
	IP   string `json:"ip,omitempty"`
}

// GuestNetworkStatus holds discovered guest network information.
type GuestNetworkStatus struct {
	PrimaryIP  string                  `json:"primaryIP,omitempty"`
	Interface  string                  `json:"interface,omitempty"`
	Ready      bool                    `json:"ready,omitempty"`
	Interfaces []GuestNetworkInterface `json:"interfaces,omitempty"`
}

// SwiftGuestStatus defines the observed state of SwiftGuest.
type SwiftGuestStatus struct {
	Phase           SwiftGuestPhase         `json:"phase,omitempty"`
	Conditions      []metav1.Condition      `json:"conditions,omitempty"`
	NodeName        string                  `json:"nodeName,omitempty"`
	PodRef          *corev1.ObjectReference `json:"podRef,omitempty"`
	Network         *GuestNetworkStatus     `json:"network,omitempty"`
	Runtime         *GuestRuntimeStatus     `json:"runtime,omitempty"`
	Console         *GuestConsoleStatus     `json:"console,omitempty"`
	RestartCount    int32                   `json:"restartCount,omitempty"`
	LastRestartTime *metav1.Time            `json:"lastRestartTime,omitempty"`
}

// SwiftGuest is the Schema for the swiftguests API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=swiftguests,scope=Namespaced,shortName=sg
type SwiftGuest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SwiftGuestSpec   `json:"spec,omitempty"`
	Status SwiftGuestStatus `json:"status,omitempty"`
}

// SwiftGuestList contains a list of SwiftGuest.
// +kubebuilder:object:root=true
type SwiftGuestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGuest `json:"items"`
}
