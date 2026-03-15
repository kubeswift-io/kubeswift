# swiftletd-mvp Specification (Delta)

## MODIFIED Requirements

### Requirement: Cloud Hypervisor process launch

swiftletd MUST launch Cloud Hypervisor as a child process. It MUST use the runtime intent (disk path, cpu, memory, network) to configure the VM. It MUST use rust/swift-ch-client for spawn and socket communication. It MUST pass `--serial socket=<runtime-dir>/serial.sock` and `--console off` to Cloud Hypervisor so VM serial console is exposed via a Unix socket for interactive operator access via swiftctl console. When the runtime intent indicates network is enabled, it MUST pass `--net tap=<tap_name>` so the VM has a virtio-net interface attached to the pod network.

#### Scenario: CH process spawned

- **WHEN** swiftletd launches the VM
- **THEN** it spawns the Cloud Hypervisor binary with arguments derived from the runtime intent via swift-ch-client

#### Scenario: Root disk and resources from intent

- **WHEN** CH is launched
- **THEN** disk path, cpu, and memory are taken from the runtime intent

#### Scenario: VM serial console via socket

- **WHEN** swiftletd launches Cloud Hypervisor
- **THEN** it passes `--serial socket=<runtime-dir>/serial.sock` and `--console off` so VM serial console is exposed via a Unix socket in the per-guest runtime directory, enabling swiftctl console to connect via socat for interactive access

#### Scenario: VM network via TAP when enabled

- **WHEN** runtime intent has network enabled and a TAP device is available (e.g., created by init container)
- **THEN** swiftletd passes `--net tap=<tap_name>` to Cloud Hypervisor so the VM has a virtio-net interface
