# Worker Node Readiness Preflight

## ADDED Requirements

### Requirement: Downloadable preflight script for worker nodes

The repository SHALL provide a standalone bash script at `scripts/kubeswift-preflight.sh` that validates whether a Linux host is suitable to run as a KubeSwift worker node. The script SHALL be downloadable and runnable without kubectl, Go, Rust, or local source-build tooling. The script SHALL NOT install packages, modify host configuration, or perform remediation.

#### Scenario: User downloads and runs preflight on Ubuntu x86_64 host with KVM

- **WHEN** user downloads and runs the preflight script on an Ubuntu x86_64 host with KVM enabled and modules loaded
- **THEN** all applicable checks report PASS and the script exits with code 0

#### Scenario: User runs preflight on host without /dev/kvm

- **WHEN** user runs the preflight script on a host where `/dev/kvm` is missing or not readable
- **THEN** the `/dev/kvm` check reports FAIL and the script exits with code 1

#### Scenario: Script runs without development tooling

- **WHEN** user runs the preflight script on a host that has no kubectl, Go, Rust, or build tooling
- **THEN** the script runs to completion and produces output

### Requirement: Hard failures distinguished from warnings

The script SHALL output `[FAIL]` for checks that block Cloud Hypervisor from running (architecture, kernel minimum, hardware virtualization, KVM modules, `/dev/kvm`). The script SHALL output `[WARN]` for checks that are recommended but do not block CH (kernel below 5.6, swap, kvm package, container runtime, cgroup v1, non-Ubuntu).

#### Scenario: Hard failure blocks readiness

- **WHEN** KVM modules are not loaded
- **THEN** the script outputs `[FAIL]` for that check and exits with code 1

#### Scenario: Warning does not block readiness

- **WHEN** swap is enabled but all FAIL checks pass
- **THEN** the script outputs `[WARN]` for swap and exits with code 2

### Requirement: Preflight produces clear PASS/WARN/FAIL output

Each check SHALL output a single line with `[PASS]`, `[WARN]`, or `[FAIL]` followed by a short description. The script SHALL print a human-readable summary at the end with counts and overall result.

#### Scenario: Output format

- **WHEN** the script runs
- **THEN** each check line matches the pattern `[PASS]`, `[WARN]`, or `[FAIL]` followed by a description
- **AND** the script prints a summary section with PASS/WARN/FAIL counts and overall result

### Requirement: Exact exit code behavior

The script SHALL exit with exactly one of: 0 (all PASS), 1 (one or more FAIL), 2 (one or more WARN, zero FAIL), 3 (script cannot run).

#### Scenario: Exit code 0 on full pass

- **WHEN** all checks PASS
- **THEN** the script exits with code 0

#### Scenario: Exit code 1 on any failure

- **WHEN** one or more checks FAIL
- **THEN** the script exits with code 1

#### Scenario: Exit code 2 on warnings only

- **WHEN** one or more checks WARN and no check FAILs
- **THEN** the script exits with code 2

#### Scenario: Exit code 3 on script error

- **WHEN** the script cannot run (e.g., missing `uname`)
- **THEN** the script exits with code 3

### Requirement: Preflight is read-only

The script SHALL perform only read operations. It SHALL NOT modify host configuration, install packages, or change sysctls.

#### Scenario: No host modifications

- **WHEN** the script runs
- **THEN** it does not write to system paths, install packages, or modify sysctls

### Requirement: Checks align with KubeSwift runtime assumptions

The script SHALL validate prerequisites required by swiftletd and Cloud Hypervisor per `docs/operator-checklist-ubuntu-x86_64.md`. Checks SHALL include: architecture (x86_64), kernel version (4.11+ min, 5.6+ recommended), hardware virtualization (vmx/svm), KVM modules, `/dev/kvm`, kvm package, container runtime, cgroup v2, swap. The script SHALL NOT include checks for sysctls or control-plane requirements.

#### Scenario: Checks map to operator checklist

- **WHEN** the script runs
- **THEN** it performs checks that correspond to the requirements in `docs/operator-checklist-ubuntu-x86_64.md`

### Requirement: Documentation for users preparing worker nodes

The repository SHALL include `docs/worker-node-preflight.md` that describes how to download and run the script, how to interpret PASS/WARN/FAIL results, the exact exit code behavior, and how checks map to KubeSwift runtime requirements. The documentation SHALL be suitable for users preparing worker nodes.

#### Scenario: User finds usage instructions

- **WHEN** user looks for preflight documentation
- **THEN** they find `docs/worker-node-preflight.md` with download instructions, run instructions, result interpretation, and exit codes
