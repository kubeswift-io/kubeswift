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

// PodAnnotationGuestIP is the annotation key for the guest's primary IP (set by swiftletd when discovered).
const PodAnnotationGuestIP = "kubeswift.io/guest-ip"

// PodAnnotationGuestInterfaces is the annotation key for guest network interfaces JSON (set by swiftletd lease poller).
const PodAnnotationGuestInterfaces = "kubeswift.io/guest-interfaces"

// PodAnnotationGuestRuntimePID is the annotation key for the CH process PID (set by swiftletd on socket ready).
const PodAnnotationGuestRuntimePID = "kubeswift.io/guest-runtime-pid"

// PodAnnotationGuestSerialSocket is the annotation key for the serial socket path (set by swiftletd on socket ready).
const PodAnnotationGuestSerialSocket = "kubeswift.io/guest-serial-socket"

// PodAnnotationGuestHypervisor is the annotation key for the hypervisor type (set by swiftletd on socket ready).
const PodAnnotationGuestHypervisor = "kubeswift.io/guest-hypervisor"

// Mount path constants. Must match internal/runtimeintent and rust/swiftletd.
const (
	DisksRootPath = "/var/lib/kubeswift/disks/root"
	DisksDataPath = "/var/lib/kubeswift/disks/data"
	SeedPath      = "/var/lib/kubeswift/seed"
	IntentPath    = "/var/lib/kubeswift/intent"
	IntentFile    = "runtime-intent.json"
	RunDirPath    = "/var/lib/kubeswift/run"
)
