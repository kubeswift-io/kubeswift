package runtimeintent

// RuntimeIntent is the node-local runtime specification.
// It contains only what swiftletd needs to launch Cloud Hypervisor or QEMU.
type RuntimeIntent struct {
	RootDisk   RootDiskSpec    `json:"rootDisk"`
	SeedPath   string          `json:"seedPath"`
	CPU        int             `json:"cpu"`
	Memory     int             `json:"memory"`    // MiB
	Lifecycle  string          `json:"lifecycle"` // "start" or "stop"
	GuestID    string          `json:"guestId"`
	Network    bool            `json:"network"`              // true when guest has network (TAP, DHCP)
	KernelBoot *KernelBootSpec `json:"kernelBoot,omitempty"` // when set, boot via --kernel + --initramfs
	Hypervisor string          `json:"hypervisor,omitempty"` // "cloud-hypervisor" (default) or "qemu"
	GPU        *GPUIntent      `json:"gpu,omitempty"`        // populated when gpuProfileRef is set
	DataDisk   *RootDiskSpec   `json:"dataDisk,omitempty"`   // secondary data disk (appears as /dev/vdb)
}

// RootDiskSpec specifies the root disk for the VM.
type RootDiskSpec struct {
	Path   string `json:"path"`
	Format string `json:"format"` // "raw" or "qcow2"
}

// KernelBootSpec specifies kernel boot parameters for direct kernel boot.
type KernelBootSpec struct {
	KernelPath    string `json:"kernelPath"`    // full path to bzImage
	InitramfsPath string `json:"initramfsPath"` // full path to rootfs.cpio.gz
	Cmdline       string `json:"cmdline"`
}

// GPUIntent describes GPU passthrough configuration passed to swiftletd.
// Populated when the SwiftGPU controller has allocated devices.
type GPUIntent struct {
	// Devices lists VFIO GPU devices to pass through to the guest.
	Devices []VFIODeviceIntent `json:"devices"`
	// Firmware is the guest firmware type: "cloudhv" (CH) or "ovmf" (QEMU).
	Firmware string `json:"firmware"`
	// NUMA describes the virtual NUMA layout. Nil = flat single-node topology.
	NUMA *NUMAIntent `json:"numa,omitempty"`
	// VCPUPinning maps vCPU IDs to host physical CPU IDs.
	VCPUPinning []VCPUPin `json:"vcpuPinning,omitempty"`
	// Hugepages specifies the hugepage size: "1G", "2M", or "" (none).
	Hugepages string `json:"hugepages,omitempty"`
	// FabricManagerPartitionID is the FM partition to activate. -1 means none.
	FabricManagerPartitionID int `json:"fabricManagerPartitionId"`
	// NVSwitches lists NVSwitch VFIO devices (Tier 3 full passthrough only).
	NVSwitches []VFIODeviceIntent `json:"nvSwitches,omitempty"`
}

// VFIODeviceIntent describes one VFIO device to pass through to the guest.
type VFIODeviceIntent struct {
	// HostPath is the sysfs path (e.g. "/sys/bus/pci/devices/0000:17:00.0/").
	HostPath string `json:"hostPath"`
	// PCIAddress is the BDF (e.g. "0000:17:00.0").
	PCIAddress string `json:"pciAddress"`
	// PCIeRootPort: if true, place this device behind a pcie-root-port (QEMU Tier 2/3).
	PCIeRootPort bool `json:"pcieRootPort"`
	// GPUDirectClique is the x_nv_gpudirect_clique value (Cloud Hypervisor Tier 1 only).
	GPUDirectClique int `json:"gpuDirectClique"`
	// NoMmap: if true, add x-no-mmap=true to the QEMU device (GPUs with >64GB BARs).
	NoMmap bool `json:"noMmap"`
	// NUMANode is the virtual NUMA node this device is associated with.
	NUMANode int `json:"numaNode"`
}

// NUMAIntent describes the virtual NUMA topology for the guest.
type NUMAIntent struct {
	Nodes []NUMANodeIntent `json:"nodes"`
}

// NUMANodeIntent describes one virtual NUMA node.
type NUMANodeIntent struct {
	ID       int    `json:"id"`
	CPUs     string `json:"cpus"`     // e.g. "0-39"
	MemoryMi int64  `json:"memoryMi"` // MiB
}

// VCPUPin maps one virtual CPU to a physical host CPU.
type VCPUPin struct {
	VCPU    int `json:"vcpu"`
	HostCPU int `json:"hostCpu"`
}
