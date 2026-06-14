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

	// VirtiofsBasePath is the in-pod parent directory under which the pod
	// builder mounts each virtiofs share's source (hostPath or PVC) at
	// VirtiofsBasePath/<name>. swiftletd's spawned virtiofsd uses this as
	// --shared-dir. The unix socket virtiofsd binds is derived separately
	// by swiftletd from the runtime dir (not under this path).
	VirtiofsBasePath = "/var/lib/kubeswift/virtiofs"

	// DataDiskDevicePathPrefix is the in-pod device-path prefix for a
	// Block-mode secondary data disk (blank or attached Block PVC). The
	// full path is DataDiskDevicePath(name). Mirrors DiskRootDevicePath
	// for the root disk (W9). Stable per disk; the in-guest letter
	// (/dev/vdc, /dev/vdd, ...) is NOT — mount data disks by UUID/label.
	DataDiskDevicePathPrefix = "/dev/kubeswift-data-"
)

// DataDiskDevicePath returns the in-pod block-device path for a named
// Block-mode secondary data disk.
func DataDiskDevicePath(name string) string {
	return DataDiskDevicePathPrefix + name
}

// DataDiskDir returns the in-pod filesystem mount directory for a named
// Filesystem-mode secondary data disk (the directory that contains its
// image.raw). The legacy singular dataDiskRef ("data") keeps the historical
// DisksDataPath; every other disk gets a per-name directory so multiple
// filesystem data disks never collide.
func DataDiskDir(name string) string {
	if name == "data" {
		return DisksDataPath
	}
	return DisksDataPath + "-" + name
}
