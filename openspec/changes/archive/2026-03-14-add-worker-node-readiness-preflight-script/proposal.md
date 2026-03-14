## Why

KubeSwift worker nodes run swiftletd pods that launch Cloud Hypervisor. Cloud Hypervisor requires KVM, kernel modules, and `/dev/kvm` on the host. Operators preparing worker nodes often discover these requirements only after scheduling fails or swiftletd crashes. A downloadable, read-only preflight script lets operators validate worker-node readiness *before* joining the cluster or running smoke tests.

## What Changes

- Add a downloadable preflight script at `scripts/kubeswift-preflight.sh`
- Script checks only what KubeSwift's runtime (swiftletd, Cloud Hypervisor) actually requires: architecture, kernel, hardware virtualization, KVM modules, `/dev/kvm`, plus recommended checks (kvm package, container runtime, cgroup v2, swap)
- Script produces PASS/WARN/FAIL per check, with hard failures clearly distinguished from warnings
- Script is standalone: no kubectl, Go, Rust, or build tooling required
- Add `docs/worker-node-preflight.md` for users preparing worker nodes: how to download, run, interpret results, and exit codes

## Capabilities

### New Capabilities

- `worker-node-readiness-preflight`: Downloadable preflight script for KubeSwift worker nodes. Read-only, no remediation. Checks aligned with `docs/operator-checklist-ubuntu-x86_64.md` and Cloud Hypervisor requirements. Exact exit code behavior for automation.

### Modified Capabilities

- (none)

## Impact

- **Paths:** `scripts/kubeswift-preflight.sh`, `docs/worker-node-preflight.md`
- **Dependencies:** None – script must not require kubectl, Go, Rust, or build tooling
- **Out of scope:** Installers, host remediation, package installation, cluster join automation, control-plane validation
