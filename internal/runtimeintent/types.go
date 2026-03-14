package runtimeintent

// RuntimeIntent is the node-local runtime specification.
// It contains only what swiftletd needs to launch Cloud Hypervisor.
type RuntimeIntent struct {
	RootDisk  RootDiskSpec `json:"rootDisk"`
	SeedPath  string       `json:"seedPath"`
	CPU       int          `json:"cpu"`
	Memory    int          `json:"memory"` // MiB
	Lifecycle string       `json:"lifecycle"` // "start" or "stop"
	GuestID   string       `json:"guestId"`
}

// RootDiskSpec specifies the root disk for the VM.
type RootDiskSpec struct {
	Path   string `json:"path"`
	Format string `json:"format"` // "raw" or "qcow2"
}
