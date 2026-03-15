# Add SwiftGuest Networking and SSH Access

## Why

KubeSwift can now install on a real cluster and boot a SwiftGuest successfully, but guest operability is still limited. There is no reliable network reachability and SSH-based access path for the guest. Console access via serial remains incomplete (GRUB patch, getty, etc.), so SSH is especially important for day-1/day-2 guest access and debugging. The next milestone is to make a running SwiftGuest reachable over the network and accessible via SSH. Without this, operators cannot reliably manage, debug, or integrate guests into workflows.

## What Changes

- **Pod network connectivity:** Init container creates bridge+TAP; launcher starts dnsmasq; swiftletd passes `--net tap=tap0` to Cloud Hypervisor. Guest gets IP via DHCP on pod subnet.
- **SSH key injection:** Operators add `ssh_authorized_keys` to SwiftSeedProfile userData (cloud-init standard). No new API fields.
- **Guest IP discovery:** swiftletd reads dnsmasq lease file, patches pod annotation `kubeswift.io/guest-ip`; controller copies to SwiftGuest `status.network.primaryIP`.
- **Default network-config:** When SwiftSeedProfile has no networkData, inject `version: 2` with `dhcp4: true` so cloud-init uses DHCP.
- **Samples and docs:** SwiftSeedProfile with ssh_authorized_keys; operator workflow doc (create guest, discover IP, SSH).

**Out of scope (explicitly deferred):** Multus, SR-IOV, secondary NICs, Kubernetes Services for guests, static IP without DHCP, swiftctl ssh command, VNC/SPICE, migration, snapshot.

## Capabilities

### New Capabilities

- `swiftguest-networking`: Defines how the guest VM receives network connectivity via the pod network. Covers TAP creation, bridge setup, Cloud Hypervisor `--net` usage, and network-config delivery via seed for first-boot DHCP or static IP.
- `swiftguest-ssh-access`: Defines how SSH keys are delivered to the guest via cloud-init `ssh_authorized_keys`, how operators provide keys (SwiftSeedProfile userData or refs), and how operators discover the guest IP and connect via SSH.
- `swiftguest-status-network`: Defines SwiftGuest status fields for network information (interface, IP, etc.) and how that information is populated (e.g., from DHCP lease, cloud-init report, or derived from pod network).

### Modified Capabilities

- `swiftletd-mvp`: Add requirement that swiftletd creates a TAP device, attaches it to the pod network (bridge or equivalent), and passes `--net tap=<name>` to Cloud Hypervisor so the VM has a virtio-net interface.
- `swiftguest-controller-reconcile`: Add requirement that SwiftGuest status includes network information (IP, interface) when available, and that the controller or node runtime can populate it.
- `seed-rendering-nocloud`: Add requirement that SwiftSeedProfile supports `ssh_authorized_keys` in userData (cloud-init standard) and that operators can provide keys via inline userData or Secret/ConfigMap refs.

## Impact

- **Paths:** `rust/swift-ch-client/`, `rust/swiftletd/`, `internal/controller/swiftguest/`, `internal/runtimeintent/`, `internal/seed/`, `api/swift/`, `config/samples/`, `docs/`
- **APIs:** SwiftGuest `status.network` (new); SwiftSeedProfile userData format (ssh_authorized_keys support)
- **Binaries:** swiftletd (Rust); controller (Go)
- **Dependencies:** Bridge/TAP setup in pod (ip, bridge-utils or equivalent); Cloud Hypervisor `--net`
- **Risks:** Pod network changes (bridge, TAP) may affect pod networking; DHCP server in pod adds complexity; IP discovery requires coordination
- **Rollback:** Revert networking changes; guests fall back to serial-only access
