package version

// Build-time variables (set via -ldflags).
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)
