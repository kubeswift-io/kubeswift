package swiftguest

import "os"

// LauncherImageEnv is the env var to override the launcher image. When set, it overrides LauncherImageDefault.
const LauncherImageEnv = "KUBESWIFT_LAUNCHER_IMAGE"

// LauncherImageDefault is the default container image for the guest pod (swiftletd + Cloud Hypervisor).
const LauncherImageDefault = "ghcr.io/projectbeskar/kubeswift/swiftletd:latest"

// LauncherImage returns the launcher image, from KUBESWIFT_LAUNCHER_IMAGE env or LauncherImageDefault.
func LauncherImage() string {
	if img := os.Getenv(LauncherImageEnv); img != "" {
		return img
	}
	return LauncherImageDefault
}

// Mount path constants. Must match internal/runtimeintent and rust/swiftletd.
const (
	DisksRootPath = "/var/lib/kubeswift/disks/root"
	SeedPath      = "/var/lib/kubeswift/seed"
	IntentPath    = "/var/lib/kubeswift/intent"
	IntentFile    = "runtime-intent.json"
	RunDirPath    = "/var/lib/kubeswift/run"
)
