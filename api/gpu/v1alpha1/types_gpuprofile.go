package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SwiftGPUProfile defines a GPU passthrough request profile.
// Users create these to describe their GPU requirements.
// Multiple SwiftGuests can reference the same profile.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=sgp
// +kubebuilder:printcolumn:name="Count",type=integer,JSONPath=`.spec.count`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.partitionMode`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.tier`
type SwiftGPUProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SwiftGPUProfileSpec `json:"spec"`
}

// SwiftGPUProfileSpec defines the desired GPU configuration.
type SwiftGPUProfileSpec struct {
	// Count is the number of GPUs requested (1, 2, 4, or 8).
	// +kubebuilder:validation:Enum=1;2;4;8
	Count int `json:"count"`

	// Model is an optional GPU model filter.
	// Examples: "H200-SXM", "A100-PCIe", "L40S", "B200-SXM".
	// Empty string matches any model.
	// +optional
	Model string `json:"model,omitempty"`

	// Tier selects the GPU complexity tier and determines which hypervisor is used.
	//   pcie:        Tier 1 — Cloud Hypervisor, flat PCI topology, no NVSwitch
	//   hgx-shared:  Tier 2 — QEMU, pcie-root-port per GPU, host Fabric Manager
	//   hgx-full:    Tier 3 — QEMU, full PCIe hierarchy, NVSwitches in guest
	// +kubebuilder:validation:Enum=pcie;hgx-shared;hgx-full
	// +kubebuilder:default=pcie
	Tier string `json:"tier"`

	// PartitionMode controls how GPUs are allocated.
	//   isolated:  GPUs have no NVLink (single GPU per VM, no Fabric Manager)
	//   shared:    GPUs share NVSwitch fabric via host Fabric Manager partition
	//   full:      All GPUs + NVSwitches passed to single VM, FM runs in guest
	// +kubebuilder:validation:Enum=isolated;shared;full
	// +kubebuilder:default=isolated
	PartitionMode string `json:"partitionMode"`

	// PCIeTopology controls virtual PCIe hierarchy construction.
	// +optional
	PCIeTopology *PCIeTopologySpec `json:"pcieTopology,omitempty"`

	// NUMATopology controls virtual NUMA layout inside the guest.
	// If nil, a flat (single NUMA node) topology is used.
	// +optional
	NUMATopology *NUMATopologySpec `json:"numaTopology,omitempty"`

	// Hugepages specifies the hugepage size for GPU memory ("1Gi", "2Mi", or "").
	// "1Gi" is required for most GPU workloads. Empty string means no hugepages.
	// +optional
	Hugepages string `json:"hugepages,omitempty"`

	// VCPUPinning enables 1:1 vCPU to physical CPU core pinning.
	// +kubebuilder:default=false
	VCPUPinning bool `json:"vcpuPinning"`

	// FabricManager controls NVIDIA Fabric Manager behavior.
	// Only relevant for tier=hgx-shared or tier=hgx-full.
	// +optional
	FabricManager *FabricManagerSpec `json:"fabricManager,omitempty"`
}

// PCIeTopologySpec controls PCIe device placement in the guest.
type PCIeTopologySpec struct {
	// RootPortPerDevice places each GPU behind its own pcie-root-port (QEMU).
	// Required for Tier 2/3 GPUs where CUDA expects a multi-level PCIe hierarchy.
	// +kubebuilder:default=false
	RootPortPerDevice bool `json:"rootPortPerDevice"`

	// GPUDirectClique sets the x_nv_gpudirect_clique value for Cloud Hypervisor.
	// All GPUs in the same clique can do PCIe P2P DMA.
	// Only used when tier=pcie (Cloud Hypervisor path).
	// +kubebuilder:default=0
	GPUDirectClique int `json:"gpuDirectClique"`

	// NoMmap disables BAR mmap in QEMU for GPUs with very large BARs (>64GB).
	// Required for B200 (256GB BAR) to avoid multi-minute boot stalls.
	// +kubebuilder:default=false
	NoMmap bool `json:"noMmap"`
}

// NUMATopologySpec describes the virtual NUMA layout inside the guest.
type NUMATopologySpec struct {
	// Sockets is the number of virtual CPU sockets.
	Sockets int `json:"sockets"`
	// CoresPerSocket is the number of cores per virtual socket.
	CoresPerSocket int `json:"coresPerSocket"`
	// ThreadsPerCore is the number of SMT threads per core (usually 1).
	// +kubebuilder:default=1
	ThreadsPerCore int `json:"threadsPerCore"`
	// MemoryPerSocketMi is the memory per NUMA node in MiB.
	MemoryPerSocketMi int64 `json:"memoryPerSocketMi"`
}

// FabricManagerSpec controls NVIDIA Fabric Manager integration.
type FabricManagerSpec struct {
	// RunInGuest is true for full passthrough (Tier 3): NVSwitches and FM run in guest.
	// False for shared mode (Tier 2): FM runs on the host.
	// +kubebuilder:default=false
	RunInGuest bool `json:"runInGuest"`

	// RequiredVersion is the required nvidia-open driver version in the guest image.
	// Must exactly match the host Fabric Manager version for shared mode.
	// +optional
	RequiredVersion string `json:"requiredVersion,omitempty"`
}

// SwiftGPUProfileList contains a list of SwiftGPUProfile.
// +kubebuilder:object:root=true
type SwiftGPUProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGPUProfile `json:"items"`
}
