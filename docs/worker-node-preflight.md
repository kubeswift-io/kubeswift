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

# Download and Run

## Download from the repository

```bash
curl -fsSL https://raw.githubusercontent.com/projectbeskar/kubeswift/main/scripts/kubeswift-worker-preflight.sh -o kubeswift-worker-preflight.sh
chmod +x kubeswift-worker-preflight.sh