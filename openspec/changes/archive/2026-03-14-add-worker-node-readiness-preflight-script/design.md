## Context

KubeSwift worker nodes run swiftletd pods that launch Cloud Hypervisor. Cloud Hypervisor requires KVM, kernel modules, and `/dev/kvm` on the host. The operator checklist (`docs/operator-checklist-ubuntu-x86_64.md`) documents these prerequisites. A preflight script automates these checks and provides a single, reproducible way to validate worker-node readiness before joining a cluster or running smoke tests.

## Goals / Non-Goals

**Goals:**

- Provide a downloadable, read-only preflight script for KubeSwift worker nodes
- Align checks with KubeSwift's actual runtime assumptions (swiftletd, Cloud Hypervisor) per operator checklist
- Clearly distinguish hard failures (FAIL) from warnings (WARN)
- Define exact exit code behavior for automation
- Include documentation suitable for users preparing worker nodes

**Non-Goals:**

- Installers, host remediation, or package installation
- Cluster join automation
- Control-plane host validation
- Full non-Ubuntu support (best-effort warnings only)

## Decisions

### Script Location

**Decision:** Place the script at `scripts/kubeswift-preflight.sh`.

**Rationale:** `scripts/` is a common convention for operator-facing, standalone scripts. Users can download via `curl` or `wget` from the repository.

### Output Format

**Decision:** Each check outputs a single line: `[PASS]`, `[WARN]`, or `[FAIL]` followed by a short description. At the end, print a summary section with counts and overall result.

**Example:**
```
[PASS] Architecture: x86_64
[PASS] Kernel: 5.15.0-91-generic (>= 5.6 recommended)
[FAIL] /dev/kvm: not readable
...
--- Summary ---
PASS: 8  WARN: 1  FAIL: 1
Overall: FAIL
```

**Rationale:** Simple, grep-friendly, and easy to parse. Automation uses exit code; humans get immediate feedback.

### Exit Code Behavior (Exact)

**Decision:** The script SHALL exit with exactly one of these codes:

| Exit Code | Condition | Meaning |
|-----------|-----------|---------|
| 0 | All checks PASS | Node is ready for KubeSwift worker workloads |
| 1 | One or more FAIL | Node is not ready; Cloud Hypervisor will not run |
| 2 | One or more WARN, zero FAIL | Node may work but has caveats; fix warnings before joining |
| 3 | Script cannot run | Unsupported environment, missing `uname` or required command |

**Rationale:** Automation: `0` → proceed; `1` → abort; `2` → optional proceed with caution; `3` → report script error.

### Hard Failures vs Warnings

**Decision:** FAIL = checks that block Cloud Hypervisor from running. WARN = checks that are recommended or may cause issues but do not block CH.

**Hard failures (FAIL):** Architecture, kernel minimum, hardware virtualization, KVM modules, `/dev/kvm`. These map directly to Cloud Hypervisor requirements in the operator checklist.

**Warnings (WARN):** Kernel below recommended (4.11–5.5), swap enabled, kvm package missing (modules may not persist), container runtime missing, cgroup v1, non-Ubuntu. These do not block CH but may affect cluster join or runtime behavior.

### Checks and Mapping to KubeSwift Runtime

Checks are derived from `docs/operator-checklist-ubuntu-x86_64.md` and Cloud Hypervisor requirements. Only checks that map to KubeSwift's actual runtime assumptions are included.

| Check | FAIL | WARN | PASS | Source |
|-------|------|------|------|--------|
| Architecture | != x86_64 | — | x86_64 | Operator checklist, CH x86_64 |
| Kernel version | < 4.11 | 4.11–5.5 | >= 5.6 | CH: 4.11 min, 5.6+ recommended |
| Hardware virtualization (vmx/svm) | Absent | — | Present | Operator checklist |
| KVM modules | Not loaded | — | kvm + kvm_intel or kvm_amd | Operator checklist |
| /dev/kvm | Missing or not readable | — | Exists and readable | Operator checklist, swiftletd |
| KVM package | — | Not installed | kvm or qemu-kvm | Operator checklist (modules persist) |
| Container runtime | — | Absent | containerd or cri-o | Worker node needs runtime |
| cgroup v2 | — | v1 or hybrid | Unified | Kubernetes 1.25+ default |
| Swap | — | Enabled | Disabled or minimal | Kubernetes best practice |
| Distro | — | Non-Ubuntu | Ubuntu | Primary target |

**Excluded:** Sysctls (kubelet sets on join), control-plane checks. Not in scope for worker-node preflight.

### Read-Only and Safe

**Decision:** Script performs only read operations. No writes, no package installs, no sysctl changes.

**Rationale:** Pre-check only; operators must apply fixes themselves.

### Documentation

**Decision:** Add `docs/worker-node-preflight.md` with: how to download and run, interpretation of PASS/WARN/FAIL, exact exit code behavior, and mapping to operator checklist. Suitable for users preparing worker nodes.

**Rationale:** Users need a single doc to understand and use the script.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Script becomes stale | Keep checks aligned with operator checklist; add "last validated against" note in script header |
| False positives | Document known gaps; encourage smoke test as final validation |
| Non-Ubuntu support | Emit WARN on non-Ubuntu; document Ubuntu x86_64 as primary target |
