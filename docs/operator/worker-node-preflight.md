# KubeSwift Worker-Node Preflight

The **KubeSwift worker-node preflight script** validates whether a Linux host is suitable to run as a **KubeSwift worker node** before you join it to a cluster or use it for KubeSwift smoke tests.

It is intended as a **host readiness pre-check** for operators. It verifies the host-level prerequisites required by the current KubeSwift runtime model, including requirements relevant to:

- **Kubernetes worker nodes**
- **swiftletd**
- **Cloud Hypervisor**
- **KVM-backed guest execution**

The script checks things such as:

- operating system and architecture
- Linux kernel version
- CPU virtualization support
- KVM kernel modules
- `/dev/kvm` availability and accessibility
- cgroup v2 mode
- swap status
- networking kernel modules
- required networking sysctls
- supported container runtime presence
- a few recommended capacity checks

The script prints **PASS / WARN / FAIL** lines for each check, followed by a final summary.

## Important characteristics

- **Read-only**: the script does not modify the host
- **Safe by default**: it only performs local inspection
- **Worker-node focused**: it does not validate developer workstation tooling
- **Not an installer**: it does not attempt to remediate failures
- **Not a cluster join tool**: it does not install Kubernetes or join the node

---

## Download and Run

### Download from the repository

```bash
curl -fsSL https://raw.githubusercontent.com/kubeswift-io/kubeswift/main/scripts/kubeswift-preflight.sh -o kubeswift-preflight.sh
chmod +x kubeswift-preflight.sh
./kubeswift-preflight.sh
```

### Run from local clone

```bash
./scripts/kubeswift-preflight.sh
# or
make preflight
```

## Result interpretation

- **PASS** — Check succeeded
- **WARN** — Non-fatal; may affect some workloads
- **FAIL** — Fatal; resolve before using the node for KubeSwift guests

The script exits with code 0 if all checks pass; non-zero if any FAIL.

## Related docs

- [Operator checklist](operator-checklist-ubuntu-x86_64.md) — Host prerequisites for smoke test
- [Smoke verification](smoke-verification.md)
