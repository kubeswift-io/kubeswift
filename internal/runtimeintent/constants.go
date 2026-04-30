package runtimeintent

// Canonical mount paths. Must match SwiftGuest controller pod spec and swiftletd expectations.
const (
	DisksRootPath     = "/var/lib/kubeswift/disks/root"
	DisksDataPath     = "/var/lib/kubeswift/disks/data"
	RootDiskImageFile = "image.raw" // Import job writes to PVC root; CH expects file path.
	DataDiskImageFile = "image.raw" // Data disk uses the same PVC layout as root disk.
	SeedPath          = "/var/lib/kubeswift/seed"
	IntentPath        = "/var/lib/kubeswift/intent/runtime-intent.json"

	// DiskRootDevicePath is the in-pod device path for a Block-mode
	// root disk (W9). MUST equal the swiftguest controller's
	// DiskRootDevicePath constant — the two are independently asserted
	// by package-boundary tests because the runtimeintent package and
	// the controller package cannot import each other (the controller
	// imports runtimeintent, not the other way around). When updating
	// either constant, update both.
	DiskRootDevicePath = "/dev/kubeswift-root"
)
