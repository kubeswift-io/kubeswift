# swiftctl-operability Specification

## Purpose

Defines the swiftctl CLI as the canonical operator CLI for SwiftGuest operability. Covers lifecycle commands (start, stop, restart), console access, namespace and kubeconfig handling, release integration, and documentation. Scope is limited to four commands; non-MVP commands (VNC, migration, snapshot, image upload, plugin packaging) are explicitly deferred.

## ADDED Requirements

### Requirement: swiftctl lifecycle commands

swiftctl MUST provide start, stop, and restart commands that operate on SwiftGuest resources. Commands MUST use SwiftGuest `spec.runPolicy` and pod deletion to achieve desired state. Behavior MUST match the actual KubeSwift architecture and smoke-tested controller/swiftletd behavior.

#### Scenario: swiftctl start

- **WHEN** an operator runs `swiftctl start <guest>` for a Stopped or non-existent guest
- **THEN** swiftctl patches `spec.runPolicy=Running`, deletes the guest pod if it exists, and the controller recreates the pod with `lifecycle=start` so swiftletd launches the VM

#### Scenario: swiftctl stop

- **WHEN** an operator runs `swiftctl stop <guest>` for a Running guest
- **THEN** swiftctl patches `spec.runPolicy=Stopped`, deletes the guest pod, and the controller recreates a pod with `lifecycle=stop` so swiftletd exits without launching

#### Scenario: swiftctl restart

- **WHEN** an operator runs `swiftctl restart <guest>` for a Running guest
- **THEN** swiftctl deletes the guest pod and the controller recreates it, causing the VM to restart

#### Scenario: restart fails when runPolicy is Stopped

- **WHEN** an operator runs `swiftctl restart <guest>` and the guest has `spec.runPolicy=Stopped`
- **THEN** swiftctl exits with a clear error and non-zero exit code

### Requirement: swiftctl console access

swiftctl MUST provide a console command that attaches to the VM serial console for interactive keyboard access. It MUST use exec into the launcher pod to run `socat -,crnl UNIX-CONNECT:<path>` for the serial socket produced by Cloud Hypervisor. It MUST NOT invent runtime endpoints or transport mechanisms that do not exist.

#### Scenario: Console attaches when guest is Running

- **WHEN** an operator runs `swiftctl console <guest>` and the guest phase is Running
- **THEN** swiftctl resolves the guest pod, execs into the launcher container, and runs `socat -,crnl UNIX-CONNECT:/var/lib/kubeswift/run/<namespace>-<name>/serial.sock` with TTY for interactive access

#### Scenario: Console fails when guest not Running

- **WHEN** an operator runs `swiftctl console <guest>` and the guest phase is not Running
- **THEN** swiftctl exits with a clear error message and non-zero exit code

### Requirement: Namespace and kubeconfig support

swiftctl MUST support namespace selection via `-n` or `--namespace` and MUST use standard kubeconfig handling (`--kubeconfig`, `--context`, KUBECONFIG environment variable).

#### Scenario: Namespace from flag

- **WHEN** an operator runs `swiftctl -n myns console sample`
- **THEN** swiftctl looks up SwiftGuest `sample` in namespace `myns`

#### Scenario: Kubeconfig from environment

- **WHEN** KUBECONFIG is set and the operator runs swiftctl
- **THEN** swiftctl uses that kubeconfig for cluster access

### Requirement: Error handling and exit codes

swiftctl MUST provide clear, actionable error messages on stderr and MUST use exit code 0 for success and non-zero for failure.

#### Scenario: Guest not found

- **WHEN** the specified SwiftGuest does not exist in the namespace
- **THEN** swiftctl prints "swiftguest \"<name>\" not found in namespace <ns>" to stderr and exits with code 1

#### Scenario: Console when pod not found

- **WHEN** swiftctl console is run but the guest pod does not exist
- **THEN** swiftctl prints a clear error and exits with code 1

### Requirement: Release integration

swiftctl MUST be integrated into the existing release plumbing: build flow, packaging flow, release documentation, and version stamping.

#### Scenario: Build flow

- **WHEN** an operator runs `make build-go`
- **THEN** the swiftctl binary is built with version stamping (VERSION, GIT_COMMIT, BUILD_DATE via ldflags)

#### Scenario: Version stamping

- **WHEN** an operator runs `swiftctl --version`
- **THEN** swiftctl prints version info consistent with other KubeSwift binaries

#### Scenario: Stable release artifacts

- **WHEN** a stable release is created (tag v*.*.*)
- **THEN** the swiftctl binary is built and attached to the GitHub Release for download

### Requirement: swiftctl documentation

The repository MUST document how swiftctl interacts with SwiftGuest resources and the runtime model. Documentation MUST describe what each command does to SwiftGuest spec/status or runtime endpoints.

#### Scenario: Command documentation

- **WHEN** an operator reads the swiftctl documentation
- **THEN** they see descriptions of start, stop, restart, and console, including the SwiftGuest and pod operations each performs

#### Scenario: Console transport documented

- **WHEN** an operator reads the console documentation
- **THEN** they understand that console uses exec + socat to connect to the serial socket in the launcher pod, not port-forward or websocket
