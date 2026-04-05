package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SwiftGPUNode represents the GPU inventory on a single Kubernetes node.
// One SwiftGPUNode exists per node labeled kubeswift.io/gpu-node=true.
// The status is populated by the GPU discovery DaemonSet; spec is intentionally empty.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sgn
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.status.gpuCount`
// +kubebuilder:printcolumn:name="Free",type=integer,JSONPath=`.status.freeGPUs`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.gpuModel`
type SwiftGPUNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Status            SwiftGPUNodeStatus `json:"status,omitempty"`
}

// SwiftGPUNodeStatus describes the GPU inventory and topology on one node.
type SwiftGPUNodeStatus struct {
	// Phase of discovery: Discovering | Ready | Error
	Phase string `json:"phase,omitempty"`

	// LastDiscovery is the timestamp of the last successful discovery run.
	// +optional
	LastDiscovery *metav1.Time `json:"lastDiscovery,omitempty"`

	// GPUCount is the total number of GPUs on this node.
	GPUCount int `json:"gpuCount,omitempty"`

	// FreeGPUs is the number of unallocated GPUs.
	FreeGPUs int `json:"freeGPUs,omitempty"`

	// GPUModel is the model of the GPUs (assumes homogeneous node).
	GPUModel string `json:"gpuModel,omitempty"`

	// GPUVendor is the vendor of the GPUs (assumes homogeneous node): "NVIDIA", "AMD", "Intel".
	GPUVendor string `json:"gpuVendor,omitempty"`

	// Host describes the physical host topology.
	Host HostTopology `json:"host,omitempty"`

	// GPUs is the list of individual GPU devices on this node.
	GPUs []GPUDevice `json:"gpus,omitempty"`

	// NVSwitches is the list of NVSwitch devices (HGX nodes only).
	// +optional
	NVSwitches []NVSwitchDevice `json:"nvSwitches,omitempty"`

	// FabricManager describes the host Fabric Manager state.
	// +optional
	FabricManager *FabricManagerStatus `json:"fabricManager,omitempty"`
}

// HostTopology describes the physical CPU, NUMA, and hugepage topology of the node.
type HostTopology struct {
	// CPUTopology from lscpu.
	CPUTopology CPUTopologyInfo `json:"cpuTopology,omitempty"`
	// NUMANodes describes each NUMA node's CPUs and memory.
	NUMANodes []NUMANodeInfo `json:"numaNodes,omitempty"`
	// IOMMUEnabled is true if IOMMU is active on the host.
	IOMMUEnabled bool `json:"iommuEnabled,omitempty"`
	// Hugepages1Gi tracks 1GiB hugepage availability.
	Hugepages1Gi HugepageInfo `json:"hugepages1Gi,omitempty"`
}

// CPUTopologyInfo describes the CPU layout as reported by lscpu.
type CPUTopologyInfo struct {
	Sockets        int `json:"sockets,omitempty"`
	CoresPerSocket int `json:"coresPerSocket,omitempty"`
	ThreadsPerCore int `json:"threadsPerCore,omitempty"`
	TotalCPUs      int `json:"totalCPUs,omitempty"`
}

// NUMANodeInfo describes one NUMA node's CPU mask and memory.
type NUMANodeInfo struct {
	ID       int    `json:"id"`
	CPUs     string `json:"cpus"`     // e.g. "0-47,96-143"
	MemoryMi int64  `json:"memoryMi"` // MiB
}

// HugepageInfo tracks 1GiB hugepage availability on the node.
type HugepageInfo struct {
	Total int `json:"total,omitempty"`
	Free  int `json:"free,omitempty"`
}

// GPUDevice describes one GPU on the node.
type GPUDevice struct {
	// Index is the GPU index on this node (0-7).
	Index int `json:"index"`
	// PCIAddress is the full PCI BDF address (e.g. "0000:17:00.0").
	PCIAddress string `json:"pciAddress"`
	// Vendor is the GPU manufacturer: "NVIDIA", "AMD", "Intel", or "Unknown (<vendor-id>)".
	Vendor string `json:"vendor"`
	// Model is the human-readable GPU model string.
	Model string `json:"model"`
	// DeviceID is the PCI vendor:device ID (e.g. "10de:2336").
	DeviceID string `json:"deviceId"`
	// NUMANode is the NUMA node this GPU is physically attached to.
	NUMANode int `json:"numaNode"`
	// IOMMUGroup is the IOMMU group number.
	IOMMUGroup int `json:"iommuGroup"`
	// Driver is the currently bound kernel driver (e.g. "vfio-pci", "nvidia").
	Driver string `json:"driver"`
	// BARSizes lists the PCI BAR sizes; used to decide whether x-no-mmap=true is needed.
	// +optional
	BARSizes []BARSize `json:"barSizes,omitempty"`
	// Allocated is true if this GPU is currently assigned to a SwiftGuest.
	Allocated bool `json:"allocated"`
	// AllocatedTo is "namespace/name" of the SwiftGuest using this GPU.
	// +optional
	AllocatedTo string `json:"allocatedTo,omitempty"`
}

// BARSize records one PCI Base Address Register region and its size.
type BARSize struct {
	Region int   `json:"region"`
	SizeMi int64 `json:"sizeMi"` // MiB
}

// NVSwitchDevice describes one NVSwitch on the node (HGX nodes only).
type NVSwitchDevice struct {
	PCIAddress string `json:"pciAddress"`
	DeviceID   string `json:"deviceId"`
	NUMANode   int    `json:"numaNode"`
}

// FabricManagerStatus describes the host NVIDIA Fabric Manager state.
type FabricManagerStatus struct {
	Installed  bool                `json:"installed"`
	Version    string              `json:"version,omitempty"`
	Running    bool                `json:"running"`
	Partitions []FMPartitionStatus `json:"partitions,omitempty"`
}

// FMPartitionStatus describes one Fabric Manager NVSwitch partition.
type FMPartitionStatus struct {
	// ID is the Fabric Manager partition ID.
	ID int `json:"id"`
	// GPUIndices lists which GPU indices belong to this partition.
	GPUIndices []int `json:"gpuIndices"`
	// Active is true if this partition is currently activated.
	Active bool `json:"active"`
	// AllocatedTo is "namespace/name" of the SwiftGuest using this partition.
	// +optional
	AllocatedTo string `json:"allocatedTo,omitempty"`
}

// SwiftGPUNodeList contains a list of SwiftGPUNode.
// +kubebuilder:object:root=true
type SwiftGPUNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwiftGPUNode `json:"items"`
}
