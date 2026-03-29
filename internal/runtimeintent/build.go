package runtimeintent

// ResolvedGuest is a minimal interface for building RuntimeIntent.
// The full type lives in internal/resolved/.
type ResolvedGuest interface {
	HasSeed() bool
	HasKernel() bool
	GetRootDiskFormat() string
	GetCPU() int
	GetMemoryMiB() int
	GetLifecycle() string
	GetGuestID() string
	GetKernelPath() string
	GetInitramfsPath() string
	GetKernelCmdline() string
}

// Build creates a RuntimeIntent from ResolvedGuest using canonical paths.
func Build(rg ResolvedGuest) *RuntimeIntent {
	if rg.HasKernel() {
		lifecycle := rg.GetLifecycle()
		if lifecycle == "" {
			lifecycle = "start"
		}
		return &RuntimeIntent{
			RootDisk:  RootDiskSpec{Path: "", Format: ""},
			SeedPath:  "",
			CPU:       rg.GetCPU(),
			Memory:    rg.GetMemoryMiB(),
			Lifecycle: lifecycle,
			GuestID:   rg.GetGuestID(),
			Network:   false,
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
		SeedPath:  seedPath,
		CPU:       rg.GetCPU(),
		Memory:    rg.GetMemoryMiB(),
		Lifecycle: lifecycle,
		GuestID:   rg.GetGuestID(),
		Network:   rg.HasSeed(),
	}
}
