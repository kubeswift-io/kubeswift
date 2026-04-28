# KubeSwift

KubeSwift is a Kubernetes-native virtual machine runtime. VMs are first-class Kubernetes workloads defined with custom resources and reconciled by controllers. The default hypervisor is [Cloud Hypervisor](https://www.cloud-hypervisor.org/); QEMU is used automatically for GPU workloads that require PCIe topology.

KubeSwift is **not** a container sandbox (it is not Kata Containers). It is a VM platform — each SwiftGuest becomes one pod, and swiftletd inside that pod launches the hypervisor.

## Features

- **Disk boot** — Cloud images (Ubuntu Noble, Rocky Linux, etc.) via CLOUDHV.fd firmware. Per-guest root disk cloning with class-based sizing from SwiftGuestClass
- **Kernel boot** — Direct bzImage + initramfs boot via SwiftKernel OCI artifacts; sub-second cold start
- **GPU passthrough** — Three-tier model: PCIe GPUs on Cloud Hypervisor, HGX SXM GPUs on QEMU with PCIe topology, full HGX passthrough. Multi-vendor discovery (NVIDIA, AMD, Intel)
- **GPU Discovery** — DaemonSet auto-discovers GPUs by PCI class, NUMA topology, NVSwitches, and Fabric Manager state per node. NVIDIA-specific features (x_nv_gpudirect_clique, NVSwitch, Fabric Manager) only applied for NVIDIA devices
- **Multi-NIC networking** — Multiple network interfaces per VM via Multus CNI. Primary NIC (management) + secondary NICs backed by NetworkAttachmentDefinitions. Supports macvlan, bridge, OVN-Kubernetes, and any Multus-compatible CNI
- **SR-IOV NIC passthrough** — Hardware NIC VFs passed directly to VMs via VFIO for native performance. GPUDirect RDMA, DPDK, NFV workloads
- **SwiftGuestPool** — Fleet management for identical VMs. ReplicaSet semantics with rolling updates (maxUnavailable/maxSurge), topology spread (Pack/Spread), and PVC per replica (StatefulSet-like persistent storage). Scale via `kubectl scale sgpool`
- **Per-guest root disk cloning** — Each VM gets its own writable copy of the SwiftImage, sized from SwiftGuestClass. SwiftImage PVC is a compact template. Two clone strategies: `copy` (default, works on any CSI driver) and `snapshot` (CSI VolumeSnapshot + dataSource clones, faster on snapshot-capable drivers) — see [docs/images/clone-strategies.md](docs/images/clone-strategies.md)
- **VM snapshots and restore** — Two backends: `csi-volume-snapshot` (disk-only, crash-consistent, CSI VolumeSnapshot-backed — see [docs/snapshots/csi-snapshots.md](docs/snapshots/csi-snapshots.md)) and `local` (memory + disk, hostPath-backed, with clone identity regeneration — see [docs/snapshots/local-snapshots.md](docs/snapshots/local-snapshots.md), [docs/snapshots/identity-regeneration.md](docs/snapshots/identity-regeneration.md), [docs/snapshots/pause-window.md](docs/snapshots/pause-window.md)). `swiftctl snapshot` / `swiftctl restore` drive both backends.
- **Data disks** — Optional secondary data disk (`dataDiskRef`) on any boot path; appears as /dev/vdb in guest
- **Networking** — tap + bridge + dnsmasq DHCP; guest IP propagated to status. Multi-NIC via Multus, macvlan, VLANs, OVN-Kubernetes overlay and localnet topologies
- **swiftctl CLI** — Console access, lifecycle control, SSH, describe, logs, debug
- **Observability** — Prometheus metrics: boot time, running count, failure count, import time
- **cloud-init** — SwiftSeedProfile (NoCloud datasource) for user-data, SSH keys, network config
- **RunPolicy** — Running, Stopped, RestartOnFailure, Always with exponential backoff
- **Security hardened** — All containers use minimum capabilities (drop ALL + specific adds), no privileged containers

## Architecture overview

```
kubectl / swiftctl
        |
Kubernetes API Server  (CRDs)
        |
KubeSwift Controllers (Go, controller-runtime)
  |- SwiftImage controller      (import, convert, prepare)
  |- SwiftGuest controller      (pod lifecycle, root disk clone, status)
  |- SwiftKernel controller     (per-node OCI pull)
  |- SwiftGPU controller        (allocation, VFIO, Fabric Manager)
  |- SwiftGuestPool controller  (fleet management, rolling updates)
        |
SwiftGuest Pod
  |- init: network-init  (bridge, tap, iptables)
  |- init: gpu-init      (VFIO bind, FM partition) [GPU path only]
  |- launcher: swiftletd (Rust)
        |
  Cloud Hypervisor v51.1  (default)
  or QEMU               (GPU Tier 2/3)
        |
  Guest VM
```

See [docs/architecture.md](docs/architecture.md) for the full diagram, networking model, and status reporting details.

## CRDs

| CRD | Short name | API group | Scope | Description |
|-----|-----------|-----------|-------|-------------|
| SwiftGuest | `sg` | `swift.kubeswift.io` | Namespaced | A running VM instance |
| SwiftGuestClass | `sgc` | `swift.kubeswift.io` | Cluster | CPU/memory/disk template |
| SwiftImage | `si` | `image.kubeswift.io` | Namespaced | Disk image source (HTTP or PVC clone) |
| SwiftSeedProfile | `ssp` | `seed.kubeswift.io` | Namespaced | cloud-init NoCloud configuration |
| SwiftKernel | `sk` | `kernel.kubeswift.io` | Namespaced | Kernel + initramfs OCI artifact |
| SwiftGPUProfile | `sgp` | `gpu.kubeswift.io` | Namespaced | GPU passthrough request |
| SwiftGPUNode | `sgn` | `gpu.kubeswift.io` | Cluster | Per-node GPU inventory (populated by discovery) |
| SwiftGuestPool | `sgpool` | `swift.kubeswift.io` | Namespaced | Fleet of identical VMs with scaling, rolling updates, and per-replica PVCs |

8 CRDs, all `v1alpha1`.

## Quick start

### Install

```bash
helm install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --version 0.1.0 \
  -n kubeswift-system \
  --create-namespace
```

### Boot a VM (disk boot)

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml

# Wait for image import (5-15 minutes)
kubectl get swiftimage ubuntu-noble -w

# Wait for VM to start
kubectl get swiftguest sample -w

# Connect to serial console
swiftctl console sample

# SSH into VM
swiftctl ssh sample -u kubeswift -i ~/.ssh/id_rsa
```

### Boot a microVM (kernel boot)

```bash
kubectl label node <node> kubeswift.io/kernel-node=true
kubectl apply -f config/samples/kernel-boot/swiftkernel-faas.yaml
kubectl get swiftkernel faas-minimal -w  # wait for Ready

kubectl apply -f config/samples/kernel-boot/swiftguest-faas.yaml
kubectl get swiftguest faas-test -w
```

### Boot a GPU VM

```bash
kubectl label node <node> kubeswift.io/gpu-node=true
kubectl apply -f config/manager/controller-manager-rbac.yaml

# PCIe GPU (Cloud Hypervisor)
kubectl apply -f config/samples/gpu-pcie/swiftgpuprofile-a100-pcie.yaml
kubectl apply -f config/samples/gpu-pcie/swiftguest-gpu.yaml
kubectl get swiftguest gpu-test -w

# Verify
swiftctl ssh gpu-test -- nvidia-smi
```

See [docs/gpu-passthrough.md](docs/gpu-passthrough.md) for Tier 2 HGX SXM setup and detailed prerequisites.

### Run a VM fleet (SwiftGuestPool)

```bash
kubectl apply -f config/samples/pool/swiftguestpool-basic.yaml
kubectl get sgpool basic-pool -w

# Scale
kubectl scale sgpool basic-pool --replicas=4

# Check members
kubectl get sg -l swift.kubeswift.io/pool=basic-pool
```

### Add a secondary NIC

```bash
# Install Multus (if not already installed)
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml

# Create a network
kubectl apply -f config/samples/multi-nic/nad-bridge.yaml

# Create a VM with two NICs
kubectl apply -f config/samples/multi-nic/swiftguest-multi-nic.yaml
```

### Smoke test

```bash
make smoke-test
```

## Documentation

| Topic | Link |
|-------|------|
| Architecture | [docs/architecture.md](docs/architecture.md) |
| CRD reference | [docs/crds.md](docs/crds.md) |
| Quickstart | [docs/quickstart.md](docs/quickstart.md) |
| GPU passthrough | [docs/gpu-passthrough.md](docs/gpu-passthrough.md) |
| swiftctl CLI | [docs/swiftctl.md](docs/swiftctl.md) |
| Multi-NIC networking | [docs/multi-nic.md](docs/multi-nic.md) |
| Networking operations | [docs/networking/operations-guide.md](docs/networking/operations-guide.md) |
| SR-IOV passthrough | [docs/networking/sriov.md](docs/networking/sriov.md) |
| OVN-Kubernetes | [docs/networking/ovn-kubernetes.md](docs/networking/ovn-kubernetes.md) |
| VMware/Proxmox comparison | [docs/networking/virtualization-comparison.md](docs/networking/virtualization-comparison.md) |
| SwiftGuestPool API | [docs/api/swiftguestpool.md](docs/api/swiftguestpool.md) |
| SwiftGuestPool guide | [docs/swiftguestpool-guide.md](docs/swiftguestpool-guide.md) |
| SwiftGuestPool use cases | [docs/swiftguestpool-use-cases.md](docs/swiftguestpool-use-cases.md) |
| VM snapshots — disk-only (CSI) | [docs/snapshots/csi-snapshots.md](docs/snapshots/csi-snapshots.md) |
| VM snapshots — memory + disk (local) | [docs/snapshots/local-snapshots.md](docs/snapshots/local-snapshots.md) |
| Clone identity regeneration | [docs/snapshots/identity-regeneration.md](docs/snapshots/identity-regeneration.md) |
| Snapshot pause window | [docs/snapshots/pause-window.md](docs/snapshots/pause-window.md) |
| Snapshot operator walkthrough (8 scenarios + findings) | [docs/snapshots/operator-walkthrough.md](docs/snapshots/operator-walkthrough.md) |
| SwiftImage clone strategies | [docs/images/clone-strategies.md](docs/images/clone-strategies.md) |
| Security audit | [docs/security-audit.md](docs/security-audit.md) |
| Development | [docs/development.md](docs/development.md) |
| Docs index | [docs/README.md](docs/README.md) |

## Build

```bash
make build            # build Go binaries
make build-images     # build container images
make push-images      # push to ghcr.io
make deploy           # apply CRDs + deploy controller
cargo build --release # build Rust crates (from rust/)
go test ./...
cargo test
```

## Status

Experimental / pre-1.0. API may change. Linux x86_64 only.

**Working:** disk boot (CLOUDHV.fd), kernel boot, QEMU boot (OVMF), networking (tap+bridge+dnsmasq), multi-NIC (Multus integration), SR-IOV NIC passthrough (VFIO), SwiftGuestPool (scaling, rolling updates, topology spread, PVC per replica), per-guest root disk cloning (class-based sizing), swiftctl CLI, cloud-init, Prometheus metrics, GPU passthrough (Phases 1-3), multi-vendor GPU discovery (NVIDIA/AMD/Intel), GPU Discovery DaemonSet, dataDiskRef/dataDiskRefs, security-hardened containers, OVN-Kubernetes integration guide, VMware/Proxmox comparison guide.

**Next:** Tier 2 GPU validation (HGX SXM), Multi-NIC hardware validation (Multus + macvlan), SR-IOV hardware validation (ConnectX NIC), additional kernel profiles (gpu-workload, vhost-user), Windows guest support, HPA auto-scaling for pools, GPU Phase 4 (full HGX passthrough).

See [kubeswift_context.md](kubeswift_context.md) for the full roadmap.

## CRD short names

```bash
kubectl get sg       # SwiftGuest
kubectl get sgc      # SwiftGuestClass
kubectl get si       # SwiftImage
kubectl get ssp      # SwiftSeedProfile
kubectl get sk       # SwiftKernel
kubectl get sgpool   # SwiftGuestPool
kubectl get sgp      # SwiftGPUProfile
kubectl get sgn      # SwiftGPUNode
```

## License

See [LICENSE](LICENSE).
