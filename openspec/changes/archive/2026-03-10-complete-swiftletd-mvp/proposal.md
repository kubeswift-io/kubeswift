## Why

The implement-swiftletd-mvp change introduced intent parsing, path handling, and NoCloud generation in rust/swiftletd and rust/swift-seed—but swiftletd does not yet create the per-guest runtime directory, launch Cloud Hypervisor, monitor the process, or report VM state to the control plane. Without these, SwiftGuest pods run a container that exits after building seed media; no VM is launched. This change completes the swiftletd MVP so that end-to-end guest boot works: runtime setup, CH launch, lifecycle monitoring, and status reporting.

## What Changes

- Implement per-guest runtime directory setup (rust/swift-runtime)
- Implement Cloud Hypervisor process launch and Unix socket API client (rust/swift-ch-client)
- Integrate runtime dir and CH launch into swiftletd main flow
- Implement CH process lifecycle monitoring (exit detection, state: Running/Stopped/Failed)
- Implement status reporting: patch SwiftGuest status (GuestRunning condition) via Kubernetes API
- Use local Unix sockets only for CH API; no TCP exposure

**Intentionally excluded:**

- Live migration, snapshots
- VFIO, vhost-user
- TCP exposure of Cloud Hypervisor API

## Capabilities

### New Capabilities

- `swiftletd-mvp`: Complete swiftletd MVP: per-guest runtime directory (rust/swift-runtime); Cloud Hypervisor launch and monitoring (rust/swift-ch-client); lifecycle state; status reporting to control plane; local Unix sockets only.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: rust/swiftletd/, rust/swift-seed/, rust/swift-ch-client/, rust/swift-runtime/
- **Prerequisites**: implement-swiftletd-mvp (intent parsing, NoCloud generation), complete-swiftguest-controller (pod envelope, runtime intent)
- **Dependencies**: Cloud Hypervisor binary, Kubernetes API (in-cluster config for status patch)
- **Risks**: CH API compatibility; process monitoring must handle crashes; RBAC for status patch
- **Rollback**: Revert to placeholder container; pods run but no VMs
