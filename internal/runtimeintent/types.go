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
