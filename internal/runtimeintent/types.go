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
	OSType     string          `json:"osType,omitempty"`     // "linux" (default) or "windows" — windows adds kvm_hyperv on the CH --cpus arg
	// CoreScheduling is the CH vCPU core-scheduling policy ("vm"/"vcpu"), empty
	// when off. When set, swiftletd appends core_scheduling=<v> to --cpus.
	CoreScheduling string     `json:"coreScheduling,omitempty"`
	GPU            *GPUIntent `json:"gpu,omitempty"` // populated when gpuProfileRef is set
	// DataDisks are the secondary VM disks, in deterministic order. Each becomes
	// one CH --disk and enumerates in the guest as /dev/vdc, /dev/vdd, ... in
	// this order. Path is opaque to swiftletd (a file for Filesystem disks, a
	// /dev/... device for Block disks — the W9 contract). Replaces the legacy
	// singular `dataDisk` field; swiftletd keeps a compat reader for old intents.
	DataDisks []DataDiskSpec `json:"dataDisks,omitempty"`
	// NICs is the list of network interfaces for the VM.
	// If empty and Network is true, a single default NIC is created (backward compat).
	NICs []NICIntent `json:"nics,omitempty"`
	// PrimaryUDNInterface is the pod's OVN-Kubernetes primary UserDefinedNetwork
	// interface (ovn-udn1) when the guest rides its namespace's primary UDN
	// (Model A). It is a TOP-LEVEL attribute of the primary NIC because the
	// primary is singular and a default guest (no spec.interfaces -> empty NICs)
	// is the common Model A case — a per-NIC field would never be emitted for it.
	// When set, network-init.sh bridges this interface to br0/tap0
	// (setup_primary_udn_nic): the guest adopts the OVN-assigned, IP-derived MAC +
	// IP (OVN port_security pins them), and eth0 stays on the cluster default for
	// the swiftletd->apiserver control path. Empty for every other networking mode.
	PrimaryUDNInterface string `json:"primaryUDNInterface,omitempty"`
	// Ports declares service ports to expose from the guest's primary NIC
	// (service exposure). When non-empty,
	// network-init.sh pins the primary VM IP and installs an in-pod DNAT
	// podIP:port -> vmIP:targetPort for each entry. nat binding only.
	Ports []PortIntent `json:"ports,omitempty"`
	// VhostUserDevices is the list of operator-backed vhost-user devices
	// (vhost-user-blk disks and generic vhost-user devices). swiftletd hands
	// each to Cloud Hypervisor via --disk vhost_user=on,socket= (blk) or
	// --generic-vhost-user virtio_id=,socket= (generic). CH path only.
	VhostUserDevices []VhostUserDeviceIntent `json:"vhostUserDevices,omitempty"`
	// Filesystems is the list of virtiofs shares. For each, swiftletd spawns a
	// virtiofsd backend (shared-dir = SourcePath, socket = SocketPath) before
	// Cloud Hypervisor and passes CH `--fs tag=<Tag>,socket=<SocketPath>`.
	// CH path only.
	Filesystems []FilesystemIntent `json:"filesystems,omitempty"`
	// Restore is set when this launcher pod is meant to bring up the VM
	// from a Tier B local snapshot via Cloud Hypervisor's --restore.
	// When non-nil, swiftletd skips seed.iso construction and the normal
	// CH spawn, and instead invokes
	// `cloud-hypervisor --api-socket=... --restore source_url=file://<path>/`.
	// The VM comes up Paused; the SwiftRestore controller drives the
	// resume separately via the snapshot-action annotation surface.
	Restore *RestoreIntent `json:"restore,omitempty"`

	// Vsock is set ONLY for a SOURCE guest that opted into the in-guest identity
	// agent. It carries the per-guest CID; swiftletd computes the socket path
	// under the runtime dir and emits `--vsock cid=<N>,socket=<path>`. A clone
	// (cloneFromSnapshot) leaves this nil — CH reopens the captured vsock device
	// from config.json on restore (the configjson patcher rewrites only the
	// socket path).
	Vsock *VsockIntent `json:"vsock,omitempty"`
}

// VsockIntent is the vsock device for the in-guest identity agent.
type VsockIntent struct {
	// CID is the guest context id (>= 3), deterministic per guest
	// (DeriveVsockCID); rides the snapshot on restore.
	CID uint32 `json:"cid"`
}

// FilesystemIntent is one virtiofs share. swiftletd runs a virtiofsd
// backend on SourcePath listening at SocketPath, then hands CH
// `--fs tag=<Tag>,socket=<SocketPath>`.
type FilesystemIntent struct {
	// Name is the per-guest identifier (drives socket/source naming).
	Name string `json:"name"`
	// Tag is the virtiofs mount tag the guest uses.
	Tag string `json:"tag"`
	// SourcePath is the in-pod directory virtiofsd shares (--shared-dir).
	// swiftletd derives the unix socket from the runtime dir.
	SourcePath string `json:"sourcePath"`
	// ReadOnly is informational; the pod builder mounts the source read-only
	// when set (that is the enforcement).
	ReadOnly bool `json:"readOnly,omitempty"`
}

// RestoreIntent points swiftletd at a snapshot directory for a
// restore-receive launch. The directory must already be present in
// the launcher pod's mount namespace at SnapshotPath; the SwiftGuest
// pod builder mounts it from the on-node hostPath (read-only for an
// in-place restore, or via a stager init container that materializes
// a patched copy in a writable emptyDir for clones — see
// docs/snapshots/local-snapshots.md).
type RestoreIntent struct {
	// SnapshotPath is the absolute in-pod path of the snapshot directory
	// (the dir CH reads config.json, state.json, and memory-ranges from).
	SnapshotPath string `json:"snapshotPath"`
	// AutoResume makes swiftletd pass `resume=true` on the CH `--restore`
	// (CH v52) so the restored guest comes up RUNNING instead of paused.
	// Set ONLY for cloneFromSnapshot (which has no SwiftRestore controller to
	// drive a Resuming phase) — it replaces the resumeCloneIfNeeded action
	// round-trip (Bug #73). SwiftRestore-driven restores leave it false and
	// keep driving resume themselves.
	AutoResume bool `json:"autoResume,omitempty"`
	// MemoryRestoreMode selects CH's `memory_restore_mode` (CH v52):
	// "ondemand" registers guest memory with userfaultfd so the VM resumes
	// immediately and pages fault in lazily (lower restore-to-resume
	// latency for large guests); "copy" is the eager default. Empty omits
	// the field (CH default). cloneFromSnapshot sets "ondemand"; SwiftRestore
	// from spec.memoryRestoreMode.
	MemoryRestoreMode string `json:"memoryRestoreMode,omitempty"`
}

// VhostUserDeviceIntent is one operator-backed vhost-user device. swiftletd
// hands it to Cloud Hypervisor opaquely; the socket is the operator's backend
// listener, mounted into the launcher by the pod builder.
type VhostUserDeviceIntent struct {
	// Name is the per-guest identifier.
	Name string `json:"name"`
	// Type is "blk" (vhost-user-blk disk) or "generic" (any vhost-user device).
	Type string `json:"type"`
	// Socket is the in-pod path of the operator's vhost-user backend socket.
	Socket string `json:"socket"`
	// VirtioID is the virtio device-type id for a generic device (number or
	// symbolic name). Empty for blk.
	VirtioID string `json:"virtioId,omitempty"`
	// QueueSizes optionally sets per-queue sizes for a generic device.
	QueueSizes []int32 `json:"queueSizes,omitempty"`
}

// NICIntent describes a single network interface for the VM.
type NICIntent struct {
	// Name is the interface identifier (matches spec.interfaces[].name).
	Name string `json:"name"`
	// Type is "bridge" (tap+bridge+virtio-net), "sriov" (VFIO passthrough),
	// or "vhost-user" (operator-provided vhost-user-net backend).
	// Defaults to "bridge" if empty.
	Type string `json:"type"`
	// TapDevice is the tap device name inside the pod namespace (tap0, tap1, etc.)
	// Empty for SR-IOV interfaces (no tap device — VFIO passthrough).
	TapDevice string `json:"tapDevice,omitempty"`
	// MAC is the MAC address for this interface (bridge type only).
	// SR-IOV interfaces use the VF's hardware MAC.
	MAC string `json:"mac,omitempty"`
	// Primary indicates this is the primary NIC with DHCP/dnsmasq.
	Primary bool `json:"primary"`
	// MultusInterface is the name of the Multus-created interface (net1, net2, etc.)
	// Empty for the primary NIC.
	MultusInterface string `json:"multusInterface,omitempty"`
	// Bridge is the bridge device name (br0, br1, etc.)
	// Empty for SR-IOV interfaces.
	Bridge string `json:"bridge,omitempty"`
	// SRIOVDevice contains SR-IOV VF info for VFIO passthrough.
	// Only populated when Type is "sriov".
	SRIOVDevice *SRIOVDeviceIntent `json:"sriovDevice,omitempty"`
	// VhostUserSocket is the in-pod path of the operator's vhost-user-net
	// backend listener socket. Only populated when Type is "vhost-user";
	// swiftletd hands it to CH as `--net vhost_user=on,socket=<path>`.
	VhostUserSocket string `json:"vhostUserSocket,omitempty"`
}

// PortIntent is one exposed service port. network-init.sh installs
// `iptables -t nat -A PREROUTING -p <protocol> --dport <port>
// -j DNAT --to-destination <vmIP>:<targetPort>` for each entry.
type PortIntent struct {
	// Name is the port identifier (matches spec.network.ports[].name).
	Name string `json:"name,omitempty"`
	// Port is the port arriving on the pod IP.
	Port int32 `json:"port"`
	// TargetPort is the in-guest listening port.
	TargetPort int32 `json:"targetPort"`
	// Protocol is "tcp" (default), "udp", or "sctp" (lowercase for iptables -p).
	Protocol string `json:"protocol,omitempty"`
}

// SRIOVDeviceIntent describes an SR-IOV VF to pass through via VFIO.
type SRIOVDeviceIntent struct {
	// ResourceName is the SR-IOV device plugin resource name (e.g., "intel.com/sriov_netdevice").
	// swiftletd reads the PCIDEVICE_<resource> env var at runtime to discover the VF BDF address.
	ResourceName string `json:"resourceName"`
}

// RootDiskSpec specifies the root disk for the VM.
type RootDiskSpec struct {
	Path   string `json:"path"`
	Format string `json:"format"` // "raw" or "qcow2"
}

// DataDiskSpec specifies one secondary VM disk for swiftletd. Path is opaque
// (a filesystem image.raw or a /dev/... block device); Name is for logging/
// status correlation.
type DataDiskSpec struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Format string `json:"format"` // "raw"
}

// KernelBootSpec specifies kernel boot parameters for direct kernel boot.
type KernelBootSpec struct {
	KernelPath    string `json:"kernelPath"`    // full path to bzImage
	InitramfsPath string `json:"initramfsPath"` // full path to rootfs.cpio.gz
	Cmdline       string `json:"cmdline"`
}

// GPUIntent describes GPU passthrough configuration passed to swiftletd.
// Populated when the SwiftGPU controller has allocated devices (native) or when
// the guest opts into the DRA backend (deviceSource: env).
type GPUIntent struct {
	// Devices lists VFIO GPU devices to pass through to the guest. omitempty:
	// the DRA backend writes a nil list (devices come from CDI env at runtime),
	// and a literal "devices": null breaks swiftletd's serde (null != absent
	// for #[serde(default)]) — cluster-e2e finding, 2026-06-12.
	Devices []VFIODeviceIntent `json:"devices,omitempty"`
	// DeviceSource selects where swiftletd obtains the device list:
	//   ""    — Devices above (native backend; controller-time allocation).
	//   "env" — synthesize from the GPU_PCI_ADDRESSES env var, injected by the
	//           DRA reference driver's CDI containerEdits at container create
	//           (scheduler-time allocation: the controller cannot know the
	//           devices when it writes this intent). Clique -1, NUMA 0 in v1.
	// Explicit marker — no silent empty-devices magic. (DRA Workstream A.)
	DeviceSource string `json:"deviceSource,omitempty"`
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
