package swiftguest

// Mount path constants. Must match internal/runtimeintent and rust/swiftletd.
const (
	DisksRootPath = "/var/lib/kubeswift/disks/root"
	SeedPath      = "/var/lib/kubeswift/seed"
	IntentPath    = "/var/lib/kubeswift/intent"
	IntentFile    = "runtime-intent.json"
	RunDirPath    = "/var/lib/kubeswift/run"

	// LauncherImage is the container image for the guest pod (swiftletd + Cloud Hypervisor).
	// Build from images/swiftletd/Containerfile.
	LauncherImage = "ghcr.io/projectbeskar/kubeswift/swiftletd:latest"
)
