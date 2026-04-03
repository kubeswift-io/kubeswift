package runtimeintent

// Canonical mount paths. Must match SwiftGuest controller pod spec and swiftletd expectations.
const (
	DisksRootPath     = "/var/lib/kubeswift/disks/root"
	DisksDataPath     = "/var/lib/kubeswift/disks/data"
	RootDiskImageFile = "image.raw" // Import job writes to PVC root; CH expects file path.
	DataDiskImageFile = "image.raw" // Data disk uses the same PVC layout as root disk.
	SeedPath          = "/var/lib/kubeswift/seed"
	IntentPath        = "/var/lib/kubeswift/intent/runtime-intent.json"
)
