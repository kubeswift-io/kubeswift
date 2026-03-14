package runtimeintent

// Canonical mount paths. Must match SwiftGuest controller pod spec and swiftletd expectations.
const (
	DisksRootPath = "/var/lib/kubeswift/disks/root"
	SeedPath      = "/var/lib/kubeswift/seed"
	IntentPath    = "/var/lib/kubeswift/intent/runtime-intent.json"
)
