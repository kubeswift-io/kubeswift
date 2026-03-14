package resolved

// System default values. Used when guest and class omit a field.
const (
	DefaultArchitecture   = "x86_64"
	DefaultFirmware       = "uefi"
	DefaultBus            = "virtio"
	DefaultInterfaceModel = "virtio-net"
	DefaultShutdownMethod = "acpi"
	DefaultRunPolicy      = "Running"
	DefaultDiskFormat     = "raw"
)
