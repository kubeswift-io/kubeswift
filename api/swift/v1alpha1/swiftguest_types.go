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

// ConditionGPUAllocated is set on SwiftGuest when the SwiftGPU controller has
// allocated GPU devices and the guest is ready to be scheduled.
const ConditionGPUAllocated = "GPUAllocated"

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
	// ImageRef references the SwiftImage to boot from (disk boot).
	// Mutually exclusive with kernelRef.
	// +optional
	ImageRef *corev1.LocalObjectReference `json:"imageRef,omitempty"`
	// KernelRef references the SwiftKernel to boot from (kernel boot).
	// Mutually exclusive with imageRef and gpuProfileRef.
	// +optional
	KernelRef     *corev1.LocalObjectReference `json:"kernelRef,omitempty"`
	KernelCmdline string                       `json:"kernelCmdline,omitempty"`
	GuestClassRef corev1.LocalObjectReference  `json:"guestClassRef"`
	// SeedProfileRef references a SwiftSeedProfile for cloud-init (disk boot only).
	// +optional
	SeedProfileRef *corev1.LocalObjectReference `json:"seedProfileRef,omitempty"`
	RunPolicy      RunPolicy                    `json:"runPolicy,omitempty"`
	// GPUProfileRef references a SwiftGPUProfile for GPU passthrough.
	// When set, the SwiftGPU controller allocates GPUs before the pod is created.
	// Mutually exclusive with kernelRef (GPU boot requires disk boot with UEFI).
	// +optional
	GPUProfileRef *corev1.LocalObjectReference `json:"gpuProfileRef,omitempty"`
	// DataDiskRef references a SwiftImage to attach as a secondary data disk.
	// The referenced image must be in Ready state. The disk appears as /dev/vdb
	// inside the guest. Works with all boot paths (disk, kernel, GPU).
	// +optional
	DataDiskRef *corev1.LocalObjectReference `json:"dataDiskRef,omitempty"`
	// Interfaces defines the network interfaces for this guest.
	// If nil or empty, a single default interface is created (backward compatible).
	// The first interface without NetworkRef is the primary interface (DHCP, management).
	// Interfaces with NetworkRef are secondary interfaces backed by Multus/NADs.
	// +optional
	Interfaces []GuestInterface `json:"interfaces,omitempty"`
}

// InterfaceType constants for GuestInterface.Type.
const (
	InterfaceTypeBridge = "bridge"
	InterfaceTypeSRIOV  = "sriov"
)

// GuestInterface defines a single network interface for a SwiftGuest.
type GuestInterface struct {
	// Name is a unique identifier for this interface.
	// Used in status reporting and logging.
	Name string `json:"name"`
	// Type specifies the interface type.
	//   bridge: (default) tap+bridge, virtio-net in guest. Used for overlay and standard networks.
	//   sriov:  SR-IOV VF passthrough via VFIO. Guest sees hardware NIC. Requires SR-IOV NAD.
	// +kubebuilder:validation:Enum=bridge;sriov
	// +kubebuilder:default=bridge
	// +optional
	Type string `json:"type,omitempty"`
	// NetworkRef references a NetworkAttachmentDefinition for this interface.
	// If nil, this is the primary interface using KubeSwift's default tap+bridge networking.
	// If set, Multus attaches the pod to the referenced NAD.
	// +optional
	NetworkRef *NetworkReference `json:"networkRef,omitempty"`
	// ResourceName is the SR-IOV device plugin resource name (e.g., "intel.com/sriov_netdevice").
	// Required when type is "sriov". The device plugin allocates a VF and the controller
	// adds this resource to the pod's resource limits.
	// +optional
	ResourceName string `json:"resourceName,omitempty"`
}

// NetworkReference references a Multus NetworkAttachmentDefinition.
type NetworkReference struct {
	// Name of the NetworkAttachmentDefinition.
	Name string `json:"name"`
	// Namespace of the NetworkAttachmentDefinition.
	// Defaults to the SwiftGuest's namespace if omitted.
	// +optional
	Namespace string `json:"namespace,omitempty"`
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
	MAC  string `json:"mac,omitempty"`
	IP   string `json:"ip,omitempty"`
}

// GuestNetworkStatus holds discovered guest network information.
type GuestNetworkStatus struct {
	PrimaryIP  string                  `json:"primaryIP,omitempty"`
	Interface  string                  `json:"interface,omitempty"`
	Ready      bool                    `json:"ready,omitempty"`
	Interfaces []GuestNetworkInterface `json:"interfaces,omitempty"`
}

// GPUStatus holds GPU allocation and runtime information for a SwiftGuest.
// Populated when spec.gpuProfileRef is set.
type GPUStatus struct {
	// Devices lists the PCI addresses of allocated GPUs (e.g. "0000:41:00.0").
	Devices []string `json:"devices,omitempty"`
	// PartitionID is the Fabric Manager partition ID for shared NVSwitch mode.
	// -1 means no partition (isolated or full passthrough mode).
	PartitionID int `json:"partitionId,omitempty"`
	// NUMANodes lists the NUMA node IDs the allocated GPUs are attached to.
	NUMANodes []int `json:"numaNodes,omitempty"`
	// Hypervisor is the resolved hypervisor for this guest ("cloud-hypervisor" or "qemu").
	Hypervisor string `json:"hypervisor,omitempty"`
	// NodeName is the Kubernetes node where GPUs were allocated.
	NodeName string `json:"nodeName,omitempty"`
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
	// GPU contains GPU allocation and runtime status.
	// Populated when spec.gpuProfileRef is set.
	// +optional
	GPU *GPUStatus `json:"gpu,omitempty"`
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
