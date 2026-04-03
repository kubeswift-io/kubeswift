# KubeSwift

KubeSwift is a Kubernetes-native virtual machine runtime. VMs are first-class Kubernetes workloads defined with custom resources and reconciled by controllers. The default hypervisor is [Cloud Hypervisor](https://www.cloud-hypervisor.org/); QEMU is used automatically for GPU workloads that require PCIe topology.

KubeSwift is **not** a container sandbox (it is not Kata Containers). It is a VM platform — each SwiftGuest becomes one pod, and swiftletd inside that pod launches the hypervisor.

## Features

- **Disk boot** — Cloud images (Ubuntu Focal, Rocky Linux, etc.) via rust-hypervisor-firmware
- **Kernel boot** — Direct bzImage + initramfs boot via SwiftKernel OCI artifacts; sub-second cold start
- **GPU passthrough** — Three-tier model: PCIe GPUs on Cloud Hypervisor, HGX SXM GPUs on QEMU with PCIe topology, full HGX passthrough
- **GPU Discovery** — DaemonSet auto-discovers GPUs, NUMA topology, NVSwitches, and Fabric Manager state per node
- **Data disks** — Optional secondary data disk (`dataDiskRef`) on any boot path; appears as /dev/vdb in guest
- **swiftctl CLI** — Console access, lifecycle control, SSH, describe, logs, debug
- **Networking** — tap + bridge + dnsmasq DHCP; guest IP propagated to status
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
  |- SwiftImage controller   (import, convert, prepare)
  |- SwiftGuest controller   (pod lifecycle, status)
  |- SwiftKernel controller  (per-node OCI pull)
  |- SwiftGPU controller     (allocation, VFIO, Fabric Manager)
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

All CRDs are `v1alpha1`.

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
kubectl apply -f config/samples/swiftguestclass-default.yaml
kubectl apply -f config/samples/swiftimage-http.yaml
kubectl apply -f config/samples/swiftseedprofile-ssh.yaml
kubectl apply -f config/samples/swiftguest-sample.yaml

# Wait for image import (5-15 minutes)
kubectl get swiftimage ubuntu-cloud -w

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
kubectl apply -f config/samples/swiftkernel-faas.yaml
kubectl get swiftkernel faas-minimal -w  # wait for Ready

kubectl apply -f config/samples/swiftguest-faas.yaml
kubectl get swiftguest faas-test -w
```

### Boot a GPU VM

```bash
kubectl label node <node> kubeswift.io/gpu-node=true
kubectl apply -f config/manager/controller-manager-rbac.yaml

# PCIe GPU (Cloud Hypervisor)
kubectl apply -f config/samples/swiftgpuprofile-pcie.yaml
kubectl apply -f config/samples/swiftguest-gpu.yaml
kubectl get swiftguest gpu-test -w

# Verify
swiftctl ssh gpu-test -- nvidia-smi
```

See [docs/gpu-passthrough.md](docs/gpu-passthrough.md) for Tier 2 HGX SXM setup and detailed prerequisites.

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

**Working:** disk boot, kernel boot, networking, swiftctl, cloud-init, Prometheus metrics, GPU passthrough (Phases 1-3), GPU discovery DaemonSet, dataDiskRef, security-hardened containers.

**Next:** GPU hardware validation (Tier 1 PCIe end-to-end, Tier 2 HGX SXM), additional kernel profiles, Windows guest support, multi-NIC, SwiftGuestPool, GPU Phase 4 (full HGX passthrough).

See [kubeswift_context.md](kubeswift_context.md) for the full roadmap.

## License

See [LICENSE](LICENSE).
