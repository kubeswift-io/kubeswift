# KubeSwift Operator Checklist (Ubuntu x86_64)

Host-level prerequisites for running KubeSwift worker nodes on Ubuntu x86_64.
Covers base VM execution, GPU passthrough, SR-IOV NIC passthrough, and
multi-network (Multus / OVN-Kubernetes) capabilities.

Each section is conditional — only configure what your workload requires.

---

## 1. Base Requirements (all nodes)

These are required on every KubeSwift worker node.

### 1.1 Kernel and KVM

| Item | Requirement | Verify |
|------|-------------|--------|
| Kernel | 5.15+ recommended (4.11+ minimum) | `uname -r` |
| KVM modules | `kvm`, `kvm_intel` or `kvm_amd` loaded | `lsmod \| grep kvm` |
| Hardware virtualization | Intel VT-x or AMD-V enabled in BIOS | `grep -Ec '(vmx\|svm)' /proc/cpuinfo` > 0 |
| `/dev/kvm` | Present and accessible | `ls -la /dev/kvm` |

```bash
# Load KVM modules permanently
sudo modprobe kvm kvm_intel  # or kvm_amd
echo -e "kvm\nkvm_intel" | sudo tee /etc/modules-load.d/kubeswift-kvm.conf
```

### 1.2 Packages

| Package | Purpose | Install |
|---------|---------|---------|
| `qemu-kvm` | KVM kernel modules and tools | `apt install qemu-kvm` |
| `kubectl` | Cluster interaction and smoke tests | [kubernetes.io/docs/tasks/tools](https://kubernetes.io/docs/tasks/tools/) |

Cloud Hypervisor and QEMU are bundled in the swiftletd container image — no host install required.

### 1.3 Networking

| Item | Requirement | Verify |
|------|-------------|--------|
| `overlay` module | Container networking | `lsmod \| grep overlay` |
| `br_netfilter` module | Bridge traffic to iptables | `lsmod \| grep br_netfilter` |
| `net.ipv4.ip_forward` | IP forwarding enabled | `sysctl net.ipv4.ip_forward` |
| `net.bridge.bridge-nf-call-iptables` | Bridge nf-call | `sysctl net.bridge.bridge-nf-call-iptables` |

```bash
# Load modules permanently
cat <<'EOF' | sudo tee /etc/modules-load.d/kubeswift-k8s.conf
overlay
br_netfilter
EOF
sudo modprobe overlay br_netfilter

# Set sysctls permanently
cat <<'EOF' | sudo tee /etc/sysctl.d/99-kubeswift.conf
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF
sudo sysctl --system
```

### 1.4 Container Runtime

| Runtime | Verify |
|---------|--------|
| containerd (recommended) | `systemctl is-active containerd` |
| CRI-O | `systemctl is-active crio` |

Ensure `SystemdCgroup = true` in `/etc/containerd/config.toml`.

### 1.5 Kubernetes

| Item | Requirement | Notes |
|------|-------------|-------|
| CRDs | All KubeSwift CRDs applied | `make deploy` or `kubectl apply -k config/crd` |
| Controllers | controller-manager running | `kubectl get pods -n kubeswift-system` |
| RBAC | Applied in target namespace | `kubectl apply -k config/rbac` |
| StorageClass | Default SC with >=10Gi | SwiftImage import and root disk use PVCs |
| Node resources | >=2 CPU, >=2Gi RAM (per guest) | Default SwiftGuestClass: 2 CPU, 2Gi |

### 1.6 Swap

Swap must be disabled for Kubernetes:

```bash
sudo swapoff -a
sudo sed -ri '/\sswap\s/s/^#?/#/' /etc/fstab
```

---

## 2. GPU Passthrough (conditional)

Required only on nodes labeled `kubeswift.io/gpu-node=true` that will run GPU-backed VMs.

### 2.1 IOMMU

| Item | Requirement | Verify |
|------|-------------|--------|
| IOMMU enabled in BIOS | Intel VT-d or AMD-Vi | BIOS settings |
| IOMMU kernel parameter | `intel_iommu=on` or `amd_iommu=on` | `cat /proc/cmdline` |
| IOMMU active | DMAR/IOMMU messages in dmesg | `dmesg \| grep -e DMAR -e IOMMU` |

```bash
# Add IOMMU to kernel command line (Intel example)
sudo sed -i 's/GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"/GRUB_CMDLINE_LINUX_DEFAULT="\1 intel_iommu=on iommu=pt"/' /etc/default/grub
sudo update-grub
sudo reboot
```

### 2.2 VFIO

| Item | Requirement | Verify |
|------|-------------|--------|
| `vfio-pci` module | Loaded | `lsmod \| grep vfio` |
| `vfio_iommu_type1` module | Loaded | `lsmod \| grep vfio_iommu` |

```bash
# Load VFIO modules permanently
cat <<'EOF' | sudo tee /etc/modules-load.d/kubeswift-vfio.conf
vfio
vfio_iommu_type1
vfio_pci
EOF
sudo modprobe vfio vfio_iommu_type1 vfio_pci
```

### 2.3 GPU Node Label

```bash
kubectl label node <node-name> kubeswift.io/gpu-node=true
```

The GPU discovery DaemonSet runs on labeled nodes and populates SwiftGPUNode status
automatically. See [GPU Passthrough](gpu-passthrough.md) for the full workflow.

### 2.4 Hugepages (Tier 2/3 HGX GPUs)

Required for QEMU GPU workloads with `hugepages: "1Gi"` in SwiftGPUProfile:

```bash
# Allocate 1GiB hugepages (example: 400 pages = 400 GiB)
echo 400 | sudo tee /proc/sys/vm/nr_hugepages

# Make permanent
echo "vm.nr_hugepages = 400" | sudo tee /etc/sysctl.d/99-kubeswift-hugepages.conf
sudo sysctl --system
```

Verify: `cat /proc/meminfo | grep HugePages_`

### 2.5 Fabric Manager (HGX SXM GPUs only)

For Tier 2/3 HGX workloads with shared NVSwitch:

| Item | Requirement | Verify |
|------|-------------|--------|
| NVIDIA Fabric Manager | Installed and running | `systemctl is-active nvidia-fabricmanager` |
| FM version | Must exactly match guest nvidia-open driver | `fmpm -v` |

---

## 3. SR-IOV NIC Passthrough (conditional)

Required only on nodes with SR-IOV capable NICs that will pass VFs to VMs.

### 3.1 IOMMU

Same as GPU — IOMMU must be enabled (section 2.1). SR-IOV VFs use the same VFIO mechanism.

### 3.2 VFIO Modules

Same as GPU — `vfio`, `vfio_iommu_type1`, `vfio_pci` must be loaded (section 2.2).

### 3.3 SR-IOV VF Configuration

| Item | Requirement | Verify |
|------|-------------|--------|
| SR-IOV capable NIC | Mellanox ConnectX-6/7, Intel E810, etc. | `lspci \| grep -i ethernet` |
| VFs created on PF | `sriov_numvfs > 0` | `cat /sys/class/net/<pf>/device/sriov_numvfs` |
| VFs visible | VF PCI devices present | `lspci \| grep "Virtual Function"` |

```bash
# Check SR-IOV capability
cat /sys/class/net/ens1f0/device/sriov_totalvfs

# Create VFs (example: 8 VFs on ens1f0)
echo 8 | sudo tee /sys/class/net/ens1f0/device/sriov_numvfs

# Make permanent via udev rule
cat <<'EOF' | sudo tee /etc/udev/rules.d/99-sriov.rules
ACTION=="add", SUBSYSTEM=="net", KERNELS=="0000:??:00.0", ATTR{device/sriov_numvfs}="8"
EOF
```

### 3.4 SR-IOV Device Plugin

The SR-IOV Network Device Plugin (or SR-IOV Network Operator) must be deployed in the
cluster to advertise VFs as extended resources:

```bash
# Verify device plugin is running
kubectl get pods -n kube-system | grep sriov

# Verify VF resources are advertised
kubectl describe node <name> | grep -A5 "Allocatable:" | grep sriov
```

### 3.5 Multus CNI

Multus is required for SR-IOV NADs. See section 4.

See [SR-IOV NIC Passthrough](networking/sriov.md) for the full setup guide and
GPUDirect RDMA configuration.

---

## 4. Multi-NIC / Multus CNI (conditional)

Required when SwiftGuests use `spec.interfaces` with secondary NICs (bridge or SR-IOV).
Not needed for single-NIC guests (the default).

### 4.1 Multus Installation

| Item | Requirement | Verify |
|------|-------------|--------|
| Multus CNI | Installed as meta-plugin | `kubectl get pods -n kube-system \| grep multus` |
| NAD CRD | `net-attach-def` CRD present | `kubectl get crd network-attachment-definitions.k8s.cni.cncf.io` |

```bash
# Install Multus (thick plugin, recommended)
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml

# Verify
kubectl get pods -n kube-system -l app=multus
```

### 4.2 NetworkAttachmentDefinitions

Create NADs for each secondary network before creating SwiftGuests that reference them.
KubeSwift does not validate NAD existence — if a referenced NAD is missing, Multus
will fail the pod at creation time.

Example NADs are in `config/samples/multi-nic/` and `config/samples/sriov/`.

See [Multi-NIC Support](multi-nic.md) for the interface spec reference.

---

## 5. OVN-Kubernetes (conditional)

Required only when using OVN-Kubernetes as the secondary network provider for
Layer 2/3, localnet, or UDN topologies.

### 5.1 OVN-Kubernetes Multi-Network

| Item | Requirement | Verify |
|------|-------------|--------|
| OVN-Kubernetes | Primary or secondary CNI | `kubectl get pods -n ovn-kubernetes \| grep ovnkube` |
| Multi-network enabled | `enable-multi-network-policies` | OVN-K operator config |
| Multus CNI | Required alongside OVN-K | See section 4.1 |

### 5.2 Localnet Bridge Mappings (VLAN access)

For localnet topology (VMs on physical VLANs), OVS bridge mappings must be configured
on each worker node:

```bash
# Manual OVS configuration
ovs-vsctl add-br br-data
ovs-vsctl add-port br-data eno2
ovs-vsctl set open . external-ids:ovn-bridge-mappings="data-physnet:br-data"
```

Or use NodeNetworkConfigurationPolicy (nmstate) for declarative configuration.
See [OVN-Kubernetes Integration](networking/ovn-kubernetes.md#3-vlan-segmentation)
for the full guide.

### 5.3 UserDefinedNetwork (UDN)

For tenant isolation via UDN CRDs:

| Item | Requirement | Verify |
|------|-------------|--------|
| OVN-Kubernetes v0.6.0+ | UDN support | `kubectl api-resources \| grep userdefinednetwork` |
| Namespace label | `k8s.ovn.org/primary-user-defined-network: ""` | Per-namespace |

See [OVN-Kubernetes Integration](networking/ovn-kubernetes.md#4-tenant-isolation-with-userdefinednetwork).

---

## 6. Host Paths, Mounts, Privileges

### Paths used by swiftletd (inside pod)

| Path | Source | Purpose |
|------|--------|---------|
| `/var/lib/kubeswift/run` | emptyDir | Per-guest runtime dir, CH/QEMU socket, seed output |
| `/var/lib/kubeswift/disks/root` | PVC | Root disk image (image.raw) |
| `/var/lib/kubeswift/disks/data` | PVC (optional) | Data disk image |
| `/var/lib/kubeswift/intent` | ConfigMap | runtime-intent.json |
| `/var/lib/kubeswift/seed` | ConfigMap (optional) | NoCloud seed data |
| `/dev/kvm` | hostPath CharDevice | KVM access |
| `/dev/vfio` | hostPath Directory | GPU and SR-IOV VFIO devices (when applicable) |
| `/sys/bus/pci` | hostPath Directory | gpu-init sysfs writes (GPU nodes only) |
| `/dev/hugepages` | emptyDir HugePages | Hugepage memory (GPU Tier 2/3) |
| `/var/lib/kubeswift/kernels/` | hostPath Directory | Kernel boot artifacts (kernel-node only) |

### Security contexts

All containers run with minimum capabilities (not privileged):

| Container | Capabilities |
|-----------|-------------|
| network-init | NET_ADMIN, NET_RAW |
| gpu-init | SYS_ADMIN |
| launcher (no GPU) | NET_ADMIN, SYS_ADMIN |
| launcher (GPU) | NET_ADMIN, SYS_ADMIN, SYS_RESOURCE, DAC_OVERRIDE |

---

## Preflight Script

Before joining a worker node or running the smoke test, run the preflight script
to validate host prerequisites:

```bash
./scripts/kubeswift-preflight.sh
# or: make preflight
```

The script checks base requirements (KVM, kernel, networking, container runtime)
and optional capabilities (IOMMU, VFIO, SR-IOV VFs, hugepages, Multus).

See [worker-node-preflight.md](worker-node-preflight.md) for result interpretation
and exit codes.

---

## Quick Verification Commands

```bash
# Base: kernel and KVM
uname -r
lsmod | grep kvm
ls -la /dev/kvm

# Base: cluster
kubectl get crd | grep kubeswift
kubectl get pods -n kubeswift-system

# GPU: IOMMU and VFIO
dmesg | grep -e DMAR -e IOMMU | head -5
lsmod | grep vfio
kubectl get swiftgpunode

# SR-IOV: VFs
lspci | grep "Virtual Function"
kubectl describe node <name> | grep sriov

# Multi-NIC: Multus
kubectl get pods -n kube-system | grep multus
kubectl get net-attach-def

# OVN-K: localnet bridge mappings
ovs-vsctl get open . external-ids:ovn-bridge-mappings

# After smoke test
kubectl get swiftguest -A
```

---

## Summary Checklist

### Base (all nodes)
- [ ] Run `./scripts/kubeswift-preflight.sh` and resolve any FAIL
- [ ] Kernel 5.15+ with KVM modules loaded
- [ ] Hardware virtualization enabled (VT-x/AMD-V)
- [ ] `/dev/kvm` present and accessible
- [ ] Swap disabled
- [ ] Container runtime running with SystemdCgroup=true
- [ ] KubeSwift CRDs, controllers, and RBAC deployed
- [ ] StorageClass with >=10Gi capacity

### GPU passthrough (conditional)
- [ ] IOMMU enabled in BIOS + kernel (`intel_iommu=on`)
- [ ] VFIO modules loaded (`vfio`, `vfio_pci`, `vfio_iommu_type1`)
- [ ] Node labeled `kubeswift.io/gpu-node=true`
- [ ] GPU discovery DaemonSet running
- [ ] Hugepages allocated (Tier 2/3 only)
- [ ] Fabric Manager installed + version matched (HGX only)

### SR-IOV passthrough (conditional)
- [ ] IOMMU enabled (same as GPU)
- [ ] VFIO modules loaded (same as GPU)
- [ ] VFs created on SR-IOV NIC
- [ ] SR-IOV device plugin running
- [ ] Multus CNI installed

### Multi-NIC (conditional)
- [ ] Multus CNI installed
- [ ] NetworkAttachmentDefinitions created for each secondary network

### OVN-Kubernetes (conditional)
- [ ] OVN-Kubernetes multi-network enabled
- [ ] Multus CNI installed
- [ ] OVS bridge mappings configured (localnet only)
- [ ] UDN CRD available (tenant isolation only)
