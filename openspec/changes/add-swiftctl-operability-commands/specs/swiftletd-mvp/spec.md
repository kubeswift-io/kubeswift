# swiftletd-mvp Specification (Delta)

## MODIFIED Requirements

### Requirement: Cloud Hypervisor process launch

swiftletd MUST launch Cloud Hypervisor as a child process. It MUST use the runtime intent (disk path, cpu, memory) to configure the VM. It MUST use rust/swift-ch-client for spawn and socket communication. It MUST pass `--console file=<runtime-dir>/console.log` to Cloud Hypervisor so VM serial/virtio-console output is written to a file for operator access via swiftctl console.

#### Scenario: CH process spawned

- **WHEN** swiftletd launches the VM
- **THEN** it spawns the Cloud Hypervisor binary with arguments derived from the runtime intent via swift-ch-client

#### Scenario: Root disk and resources from intent

- **WHEN** CH is launched
- **THEN** disk path, cpu, and memory are taken from the runtime intent

#### Scenario: VM console output to file

- **WHEN** swiftletd launches Cloud Hypervisor
- **THEN** it passes `--console file=<runtime-dir>/console.log` so VM serial/console output is written to a file in the per-guest runtime directory, enabling swiftctl console to stream it via exec
