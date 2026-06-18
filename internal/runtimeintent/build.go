package runtimeintent

// ResolvedGuest is a minimal interface for building RuntimeIntent.
// The full type lives in internal/resolved/.
type ResolvedGuest interface {
	HasSeed() bool
	HasKernel() bool
	HasNetwork() bool
	GetDataDisks() []DataDiskSpec
	GetRootDiskFormat() string
	// GetRootDiskVolumeMode returns "Filesystem" or "Block". Empty is
	// treated as "Filesystem" (pre-W9 default). Block resolves the
	// RootDisk.Path to DiskRootDevicePath instead of the filesystem
	// image.raw path; swiftletd hands the device path opaquely to
	// Cloud Hypervisor's --disk path=... which works for both file
	// and device targets.
	GetRootDiskVolumeMode() string
	GetCPU() int
	GetMemoryMiB() int
	GetLifecycle() string
	GetGuestID() string
	GetKernelPath() string
	GetInitramfsPath() string
	GetKernelCmdline() string
	GetHypervisor() string
	GetOSType() string
	GetNICs() []NICIntent
	// GetPrimaryUDNInterface returns the OVN-Kubernetes primary-UDN interface
	// (ovn-udn1) when the guest rides its namespace primary UDN (Model A), else "".
	GetPrimaryUDNInterface() string
	GetExposedPorts() []PortIntent
	GetFilesystems() []FilesystemIntent
	GetVhostUserDevices() []VhostUserDeviceIntent
	GetCoreScheduling() string
	// GetVsockCID returns the vsock CID for a SOURCE guest that opted into the
	// in-guest identity agent, or 0 when the agent is not enabled (or the guest
	// is a clone — a clone reopens the captured vsock device from config.json).
	GetVsockCID() uint32
}

// Build creates a RuntimeIntent from ResolvedGuest using canonical paths.
func Build(rg ResolvedGuest) *RuntimeIntent {
	dataDisks := rg.GetDataDisks()

	nics := rg.GetNICs()
	primaryUDN := rg.GetPrimaryUDNInterface()
	ports := rg.GetExposedPorts()
	filesystems := rg.GetFilesystems()
	vhostUserDevices := rg.GetVhostUserDevices()
	coreScheduling := rg.GetCoreScheduling()

	var vsock *VsockIntent
	if cid := rg.GetVsockCID(); cid != 0 {
		vsock = &VsockIntent{CID: cid}
	}

	if rg.HasKernel() {
		lifecycle := rg.GetLifecycle()
		if lifecycle == "" {
			lifecycle = "start"
		}
		return &RuntimeIntent{
			RootDisk:            RootDiskSpec{Path: "", Format: ""},
			SeedPath:            "",
			CPU:                 rg.GetCPU(),
			Memory:              rg.GetMemoryMiB(),
			Lifecycle:           lifecycle,
			GuestID:             rg.GetGuestID(),
			Network:             rg.HasNetwork(),
			Hypervisor:          rg.GetHypervisor(),
			OSType:              rg.GetOSType(),
			DataDisks:           dataDisks,
			NICs:                nics,
			PrimaryUDNInterface: primaryUDN,
			Ports:               ports,
			Filesystems:         filesystems,
			VhostUserDevices:    vhostUserDevices,
			CoreScheduling:      coreScheduling,
			Vsock:               vsock,
			KernelBoot: &KernelBootSpec{
				KernelPath:    rg.GetKernelPath(),
				InitramfsPath: rg.GetInitramfsPath(),
				Cmdline:       rg.GetKernelCmdline(),
			},
		}
	}

	seedPath := ""
	if rg.HasSeed() {
		seedPath = SeedPath
	}
	lifecycle := rg.GetLifecycle()
	if lifecycle == "" {
		lifecycle = "start"
	}
	rootDiskPath := DisksRootPath + "/" + RootDiskImageFile
	if rg.GetRootDiskVolumeMode() == "Block" {
		// Block-mode root disk: swiftletd's CH spawn will pass this
		// path to --disk path=<value>. CH treats it opaquely (file or
		// device); the kubelet surfaces the PVC at this path via
		// VolumeDevices in the launcher pod (see pod.go::rootDiskMount).
		rootDiskPath = DiskRootDevicePath
	}

	return &RuntimeIntent{
		RootDisk: RootDiskSpec{
			Path:   rootDiskPath,
			Format: rg.GetRootDiskFormat(),
		},
		SeedPath:            seedPath,
		CPU:                 rg.GetCPU(),
		Memory:              rg.GetMemoryMiB(),
		Lifecycle:           lifecycle,
		GuestID:             rg.GetGuestID(),
		Network:             rg.HasNetwork(),
		Hypervisor:          rg.GetHypervisor(),
		OSType:              rg.GetOSType(),
		DataDisks:           dataDisks,
		NICs:                nics,
		PrimaryUDNInterface: primaryUDN,
		Ports:               ports,
		Filesystems:         filesystems,
		VhostUserDevices:    vhostUserDevices,
		CoreScheduling:      coreScheduling,
		Vsock:               vsock,
	}
}
