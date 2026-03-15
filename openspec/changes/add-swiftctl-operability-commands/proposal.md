# Add swiftctl Operability Commands

## Why

KubeSwift now has a working first-boot path and real-cluster smoke-test success, but operability is still weak. Operators need a CLI for day-1/day-2 guest interactions—especially console access and basic lifecycle operations—to debug, inspect, and manage SwiftGuests. The repository already includes `cmd/swiftctl/main.go`, but it is currently a no-op. Turning swiftctl into the first real KubeSwift operator CLI is the natural next priority after first-boot success: it delivers immediate operator value without changing the control-plane architecture.

## What Changes

- Implement a focused first swiftctl CLI with four commands: `start`, `stop`, `restart`, `console`
- **Lifecycle commands** (start, stop, restart): map to SwiftGuest `spec.runPolicy` and pod deletion; no new APIs
- **Console command**: exec into launcher pod, tail VM console file; requires swiftletd to pass `--console file=` to Cloud Hypervisor
- Support namespace (`-n`) and kubeconfig/context; clear errors and exit codes
- Integrate swiftctl into existing release plumbing: build flow, packaging, release docs, version stamping
- Document how swiftctl interacts with SwiftGuest and the runtime model

**Out of scope (explicitly deferred):** VNC/SPICE, image upload, migration, snapshot commands, kubectl plugin/krew packaging, `swiftctl list`, `swiftctl ssh`.

## Capabilities

### New Capabilities

- `swiftctl-operability`: Defines the swiftctl CLI command set, lifecycle semantics (start/stop/restart), console access mechanism (exec + tail of VM console file), namespace and kubeconfig handling, error handling, release integration, and documentation requirements.

### Modified Capabilities

- `swiftletd-mvp`: Add requirement that swiftletd passes Cloud Hypervisor `--console file=<path>` so VM serial/console output is written to a file in the per-guest runtime directory, enabling `swiftctl console` to stream it via exec.

## Impact

- **Paths:** `cmd/swiftctl/`, `internal/cli/` (optional helpers), `rust/swift-ch-client/`, `rust/swiftletd/`, `docs/`, `Makefile`, `.github/workflows/`
- **Binaries:** `swiftctl` (Go)
- **APIs:** SwiftGuest `spec.runPolicy` (existing); no API changes
- **Dependencies:** cobra, client-go (or controller-runtime client)
- **Risks:** Console access depends on exec into pod; clusters with restricted exec may not support it
- **Rollback:** Revert swiftctl and swiftletd changes; SwiftGuest lifecycle via kubectl patch remains available
