package runtimeintent

// ResolvedGuest is a minimal interface for building RuntimeIntent.
// The full type lives in internal/resolved/.
type ResolvedGuest interface {
	HasSeed() bool
	HasKernel() bool
	HasNetwork() bool
	HasDataDisk() bool
	GetRootDiskFormat() string
	GetCPU() int
	GetMemoryMiB() int
	GetLifecycle() string
	GetGuestID() string
	GetKernelPath() string
	GetInitramfsPath() string
	GetKernelCmdline() string
	GetHypervisor() string
	GetNICs() []NICIntent
}

// Build creates a RuntimeIntent from ResolvedGuest using canonical paths.
func Build(rg ResolvedGuest) *RuntimeIntent {
	var dataDisk *RootDiskSpec
	if rg.HasDataDisk() {
		dataDisk = &RootDiskSpec{
			Path:   DisksDataPath + "/" + DataDiskImageFile,
			Format: "raw",
		}
	}

	nics := rg.GetNICs()

	if rg.HasKernel() {
		lifecycle := rg.GetLifecycle()
		if lifecycle == "" {
			lifecycle = "start"
		}
		return &RuntimeIntent{
			RootDisk:   RootDiskSpec{Path: "", Format: ""},
			SeedPath:   "",
			CPU:        rg.GetCPU(),
			Memory:     rg.GetMemoryMiB(),
			Lifecycle:  lifecycle,
			GuestID:    rg.GetGuestID(),
			Network:    rg.HasNetwork(),
			Hypervisor: rg.GetHypervisor(),
			DataDisk:   dataDisk,
			NICs:       nics,
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
	return &RuntimeIntent{
		RootDisk: RootDiskSpec{
			Path:   DisksRootPath + "/" + RootDiskImageFile,
			Format: rg.GetRootDiskFormat(),
		},
		SeedPath:   seedPath,
		CPU:        rg.GetCPU(),
		Memory:     rg.GetMemoryMiB(),
		Lifecycle:  lifecycle,
		GuestID:    rg.GetGuestID(),
		Network:    rg.HasNetwork(),
		Hypervisor: rg.GetHypervisor(),
		DataDisk:   dataDisk,
		NICs:       nics,
	}
}
