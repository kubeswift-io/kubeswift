# swiftletd-mvp Specification

## Purpose
TBD - created by archiving change complete-swiftletd-mvp. Update Purpose after archive.
## Requirements
### Requirement: Per-guest runtime directory

swiftletd MUST create a per-guest runtime directory before launching Cloud Hypervisor. The directory MUST contain paths for seed media output and the Cloud Hypervisor API socket.

#### Scenario: Runtime directory created

- **WHEN** swiftletd prepares for launch
- **THEN** it creates a per-guest directory (e.g., /var/lib/kubeswift/run/<guest-id>/) using rust/swift-runtime

#### Scenario: Directory contains required paths

- **WHEN** the runtime directory is prepared
- **THEN** it includes a subdirectory for seed media and a path for the Cloud Hypervisor API socket

### Requirement: Cloud Hypervisor process launch

swiftletd MUST launch Cloud Hypervisor as a child process. It MUST use the runtime intent (disk path, cpu, memory) to configure the VM. It MUST use rust/swift-ch-client for spawn and socket communication. It MUST pass `--serial socket=<runtime-dir>/serial.sock` and `--console off` to Cloud Hypervisor so VM serial console is exposed via a Unix socket for interactive operator access via swiftctl console.

#### Scenario: CH process spawned

- **WHEN** swiftletd launches the VM
- **THEN** it spawns the Cloud Hypervisor binary with arguments derived from the runtime intent via swift-ch-client

#### Scenario: Root disk and resources from intent

- **WHEN** CH is launched
- **THEN** disk path, cpu, and memory are taken from the runtime intent

#### Scenario: VM serial console via socket

- **WHEN** swiftletd launches Cloud Hypervisor
- **THEN** it passes `--serial socket=<runtime-dir>/serial.sock` and `--console off` so VM serial console is exposed via a Unix socket in the per-guest runtime directory, enabling swiftctl console to connect via socat for interactive access

### Requirement: Cloud Hypervisor API via local Unix socket only

swiftletd MUST use local Unix sockets for Cloud Hypervisor API access. It MUST NOT expose the Cloud Hypervisor API over TCP.

#### Scenario: Unix socket for CH API

- **WHEN** swiftletd communicates with Cloud Hypervisor
- **THEN** it uses a local Unix socket (e.g., --api-socket /path/to/socket)

#### Scenario: No TCP binding

- **WHEN** Cloud Hypervisor is launched
- **THEN** the API is not bound to any TCP address; only Unix socket is used

### Requirement: Monitor process lifecycle

swiftletd MUST monitor the Cloud Hypervisor process lifecycle. It MUST detect when CH exits (graceful shutdown or crash) and update internal state accordingly.

#### Scenario: Monitor CH process

- **WHEN** Cloud Hypervisor is running
- **THEN** swiftletd monitors the process (wait or periodic check)

#### Scenario: Detect exit and distinguish outcome

- **WHEN** the Cloud Hypervisor process exits
- **THEN** swiftletd detects the exit and distinguishes graceful (exit 0) from error (non-zero)

### Requirement: Report runtime state to control plane

swiftletd MUST report runtime state (VM running, stopped, failed) back to the control plane. It MUST update SwiftGuest status or conditions so the controller and users can see VM state.

#### Scenario: Report VM running

- **WHEN** the VM is successfully launched and running
- **THEN** swiftletd reports this state to the control plane (e.g., patch GuestRunning=True)

#### Scenario: Report VM stopped or failed

- **WHEN** the VM stops or crashes
- **THEN** swiftletd reports the state (e.g., GuestRunning=False, condition reason updated)

### Requirement: NoCloud output in runtime directory

When seed is present, swiftletd MUST generate NoCloud media into the per-guest runtime directory (not a hardcoded path). The output MUST be usable by Cloud Hypervisor for cloud-init.

#### Scenario: NoCloud in runtime dir

- **WHEN** seed inputs are present
- **THEN** swiftletd calls swift-seed to build NoCloud and writes output to the runtime directory seed subdirectory

#### Scenario: Seed path from intent

- **WHEN** runtime intent has non-empty seed path
- **THEN** swiftletd reads seed inputs from that path and writes NoCloud output to runtime dir

### Requirement: Crate layout

swiftletd, swift-seed, swift-ch-client, and swift-runtime MUST reside under rust/ in github.com/projectbeskar/kubeswift.

#### Scenario: swift-runtime for directory setup

- **WHEN** per-guest runtime directory is created
- **THEN** the logic resides in rust/swift-runtime

#### Scenario: swift-ch-client for CH API

- **WHEN** swiftletd spawns or communicates with Cloud Hypervisor
- **THEN** it uses rust/swift-ch-client for the API client and process spawn

