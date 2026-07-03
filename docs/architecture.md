# KubeSwift Architecture

KubeSwift is a Kubernetes-native VM runtime. Virtual machines are first-class Kubernetes workloads defined by CRDs and reconciled by controllers. Each VM runs as a pod: the launcher container (swiftletd) spawns Cloud Hypervisor or QEMU and manages the VM lifecycle.

This document covers the full system architecture: control plane, runtime, boot paths, networking, status reporting, and GPU support.

> **Visual reference:** [architecture/diagrams.md](architecture/diagrams.md) renders the same architecture as diagrams — system overview, CRD relationship map, launcher pod anatomy & boot data path, status reporting, the live-migration sequence, and the snapshot/clone flows.

## System diagram

```
User (kubectl / swiftctl / helm)
        |
        v
Kubernetes API Server
  |-- SwiftGuest          (swift.kubeswift.io/v1alpha1)
  |-- SwiftGuestClass     (swift.kubeswift.io/v1alpha1)
  |-- SwiftGuestPool      (swift.kubeswift.io/v1alpha1)
  |-- SwiftImage          (image.kubeswift.io/v1alpha1)
  |-- SwiftSeedProfile    (seed.kubeswift.io/v1alpha1)
  |-- SwiftKernel         (kernel.kubeswift.io/v1alpha1)
  |-- SwiftGPUProfile     (gpu.kubeswift.io/v1alpha1)
  |-- SwiftGPUNode        (gpu.kubeswift.io/v1alpha1)
  |-- SwiftSnapshot       (snapshot.kubeswift.io/v1alpha1)
  |-- SwiftRestore        (snapshot.kubeswift.io/v1alpha1)
  |-- SwiftSnapshotSchedule (snapshot.kubeswift.io/v1alpha1)
  |-- SwiftMigration      (migration.kubeswift.io/v1alpha1)
        |
        v
KubeSwift Controller Manager (Go, controller-runtime)
  |-- SwiftImage controller    watches SwiftImage; runs import + measure jobs
  |-- SwiftKernel controller   watches SwiftKernel + Nodes; runs per-node pull jobs
  |-- SwiftGPU controller      watches SwiftGPUProfile + SwiftGPUNode; allocates GPUs
  |-- SwiftGuest controller    watches SwiftGuest; creates launcher pod; maps annotations to status
        |
        v
SwiftGuest Pod (one per SwiftGuest)
  |-- init container: network-init   (bridge, tap, iptables, dnsmasq)  [when network=true]
  |-- init container: gpu-init        (VFIO bind, FM partition activate) [GPU path only]
  |-- launcher container: swiftletd  (Rust)
        |
        v
  Cloud Hypervisor v52.0   (primary: Linux + Windows disk boot, kernel boot, Tier 1 PCIe GPU)
  QEMU                     (secondary: HGX SXM Tier 2/3 GPU workloads only)
        |
        v
  Guest VM
```

## Control plane

### SwiftImage controller

Watches `SwiftImage` objects. When a new image is created, the controller creates an import job that:

1. Downloads the source image (HTTP URL)
2. Converts qcow2 to raw format using `qemu-img convert`
3. Mounts each partition, detects GPT layout via od-based parsing, patches GRUB for serial console output
4. Writes `image.raw` to a PVC
5. A second measure job reads `image.raw.size` and writes the byte count back to `status.preparedArtifact.size`

The controller advances `status.phase` through: Pending → Importing → Validating → Preparing → Ready (or Failed).

### SwiftKernel controller

Watches `SwiftKernel` objects and `Node` objects. For each labeled node (`kubeswift.io/kernel-node=true`), the controller creates a pull job that runs `oras pull` to fetch the kernel OCI artifact. The artifact contains two layers:

- `bzImage` — media type `application/vnd.kubeswift.kernel.binary`
- `rootfs.cpio.gz` — media type `application/vnd.kubeswift.initramfs.binary`

Artifacts land at `/var/lib/kubeswift/kernels/<namespace>-<name>/` on the node. The path is deterministic and never stored in status. The controller tracks per-node pull progress in `status.nodeStatuses[]`. When all labeled nodes are Ready, `status.phase` becomes Ready.

### SwiftGPU controller

Watches `SwiftGuest` objects with `gpuProfileRef` set and `SwiftGPUNode` inventory. On reconcile, the controller:

1. Resolves the SwiftGPUProfile to determine count, model, tier, and topology requirements
2. Finds a SwiftGPUNode with enough free GPUs matching the model filter
3. Marks GPUs as allocated in SwiftGPUNode status (`allocated=true`, `allocatedTo="namespace/name"`)
4. Determines the hypervisor: QEMU for `tier=hgx-shared` or `tier=hgx-full`; Cloud Hypervisor for `tier=pcie`
5. For shared mode: selects a Fabric Manager partition and sets it as pending activation
6. Sets `GPUAllocated=True` condition on SwiftGuest

The SwiftGPU discovery DaemonSet runs on nodes labeled `kubeswift.io/gpu-node=true` and populates SwiftGPUNode status with GPU inventory, NUMA topology, hugepage availability, and Fabric Manager state.

### SwiftGuest controller

The central controller. It watches `SwiftGuest`, `SwiftImage`, `SwiftKernel`, `SwiftGPUProfile`, and `SwiftGPUNode`. On each reconcile:

1. Validates mutual exclusivity: `imageRef` and `kernelRef` cannot both be set; `gpuProfileRef` and `kernelRef` cannot both be set
2. For GPU guests: waits for `GPUAllocated=True` condition before creating the pod
3. Resolves refs to build a `RuntimeIntent` JSON — the serialized launch spec written to a ConfigMap
4. Creates the launcher pod with appropriate volumes, init containers, and node selectors
5. After the pod exists, reads pod annotations set by swiftletd and maps them to SwiftGuest status fields

The controller respects `runPolicy=Stopped` and does not recreate the pod when stopped. Launcher pods always have `restartPolicy: Never` — the controller owns VM lifecycle, not Kubernetes.

## Runtime plane

### RuntimeIntent

The RuntimeIntent is a JSON document written to a ConfigMap and mounted at `/var/lib/kubeswift/intent/runtime-intent.json` in the launcher pod. It contains everything swiftletd needs:

```json
{
  "guestId": "default/sample",
  "cpu": 2,
  "memory": 2048,
  "lifecycle": "start",
  "network": true,
  "hypervisor": "cloud-hypervisor",
  "rootDisk": {"path": "/var/lib/kubeswift/disks/root/image.raw", "format": "raw"},
  "seedPath": "/var/lib/kubeswift/seed",
  "kernelBoot": null,
  "gpu": null
}
```

For GPU guests, the `gpu` field contains VFIO device paths, NUMA topology, vCPU pinning, hugepage size, and Fabric Manager partition ID.

### swiftletd

swiftletd is the Rust launcher daemon. It reads the RuntimeIntent and:

1. Checks `lifecycle` — if "stop", it exits without launching
2. Checks `hypervisor` — dispatches to `CloudHypervisorProcess` or `QemuProcess`
3. For QEMU GPU boot: copies OVMF_VARS.fd to the runtime directory before spawning
4. Spawns the hypervisor process and waits for the serial socket to appear
5. Calls `report.rs` to write pod annotations: PID, hypervisor, serial socket path
6. Polls `/var/lib/kubeswift/run/<guestId>/dnsmasq.leases` to discover the guest IP
7. Calls `lease.rs` to write the guest IP as a pod annotation

The runtime directory is `/var/lib/kubeswift/run/<namespace>-<name>/` and contains:

- `ch.sock` or `qmp.sock` — hypervisor control socket
- `serial.sock` — serial console socket
- `OVMF_VARS.fd` — per-VM mutable firmware variables (QEMU path)
- `dnsmasq.leases` — DHCP lease file (when networking is enabled)

### Status reporting via pod annotations

swiftletd does not patch SwiftGuest status directly (except for the `GuestRunning` condition, which uses kube-rs DynamicObject). All other status is communicated through pod annotations, which the SwiftGuest controller reads on reconcile:

| Annotation | Set by | Maps to |
|------------|--------|---------|
| `kubeswift.io/guest-ip` | lease.rs | `status.network.primaryIP` |
| `kubeswift.io/guest-interfaces` | lease.rs | `status.network.interfaces[]` |
| `kubeswift.io/guest-runtime-pid` | report.rs | `status.runtime.pid` |
| `kubeswift.io/guest-serial-socket` | report.rs | `status.console.serialSocket` |
| `kubeswift.io/guest-hypervisor` | report.rs | `status.runtime.hypervisor` |
| `kubeswift.io/gpu-devices` | report.rs | `status.gpu.devices[]` |

The controller requeues every 5 seconds while the guest is Running but has no IP yet, to handle annotation cache staleness.

## Boot paths

### Disk boot (imageRef)

The default boot path for cloud images. CLOUDHV.fd (EDK2/OVMF UEFI firmware for Cloud Hypervisor) provides UEFI boot.

```
SwiftGuest (imageRef=ubuntu-noble, seedProfileRef=ssh)
  |
  v
Controller resolves SwiftImage → PVC path
Controller renders cloud-init → ConfigMap
Controller writes RuntimeIntent (rootDisk.path, seedPath)
  |
  v
Pod:
  init: network-init (bridge, tap, dnsmasq)
  launcher: swiftletd
    |
    v
cloud-hypervisor \
  --kernel /usr/share/kubeswift-firmware/CLOUDHV.fd \
  --disk path=image.raw path=seed.iso \
  --net tap=tap0 \
  --serial socket=serial.sock
```

CLOUDHV.fd is loaded via `--kernel`, never `--firmware`. The `--firmware` flag is reserved for OVMF/EDK2 in the QEMU path.

All modern Linux distributions are supported: Ubuntu 22.04+, Rocky 9, Fedora, Debian 12. Ubuntu Noble (24.04) is the primary guest OS.

### Kernel boot (kernelRef)

Direct kernel boot without firmware, GRUB, or cloud-init. Used for purpose-built microVMs.

```
SwiftGuest (kernelRef=faas-minimal)
  |
  v
Controller resolves SwiftKernel → localPath
  /var/lib/kubeswift/kernels/default-faas-minimal/
Controller writes RuntimeIntent (kernelBoot.kernelPath, kernelBoot.initramfsPath, cmdline)
  |
  v
Pod:
  nodeSelector: kubeswift.io/kernel-node=true
  init: network-init (bridge, tap, dnsmasq)
  launcher: swiftletd
    |
    v
cloud-hypervisor \
  --kernel bzImage \
  --initramfs rootfs.cpio.gz \
  --cmdline "console=ttyS0 root=/dev/ram0 rdinit=/init" \
  --net tap=tap0 \
  --serial socket=serial.sock
```

No root disk PVC, no seed ConfigMap, no cloud-init. The kernel artifact path is deterministic: `/var/lib/kubeswift/kernels/<namespace>-<name>/`.

### Windows boot (osType: windows)

Windows guests are a variant of the disk-boot path, not a third boot source. Set `spec.osType: windows` on the SwiftGuest (default is `linux`); the guest boots on the **Cloud Hypervisor disk path** with two Windows-specific runtime flags.

```
cloud-hypervisor \
  --kernel /usr/share/kubeswift-firmware/CLOUDHV.fd \
  --cpus boot=2,kvm_hyperv=on \
  --disk path=image.raw,image_type=raw path=seed.iso \
  --net tap=tap0 \
  --serial socket=serial.sock
```

Key differences from the Linux disk path:

- `--cpus boot=N,kvm_hyperv=on` — Windows needs the KVM Hyper-V enlightenments enabled, or it silently hangs in early multiprocessor/HAL bring-up.
- `--disk image_type=raw` — Windows' viostor (virtio-blk) driver requires the explicit raw disk type (a Cloud Hypervisor v52.0 capability).
- The qcow2→raw import **skips** the Linux GRUB serial-console patch — Windows has no GRUB. The import otherwise runs unprivileged like the Linux path.
- Provisioning is **cloudbase-init over the same NoCloud `cidata` seed** (no seed-mechanism change) — the seed ISO carries `meta-data`/`user-data` that cloudbase-init reads on first boot.
- The guest image must be **virtio-ready**: viostor (virtio-blk) and NetKVM (virtio-net) drivers injected during image prep.

See [docs/windows/overview.md](windows/overview.md) for the authoritative operator runbook (image prep, RDP, provisioning notes, limitations).

### GPU boot (gpuProfileRef)

GPU boot always combines `imageRef` with `gpuProfileRef`. The hypervisor is selected automatically based on the GPU tier.

**Tier 1 (PCIe GPUs, Cloud Hypervisor):**

```
cloud-hypervisor \
  --kernel /usr/share/kubeswift-firmware/CLOUDHV.fd \
  --disk path=image.raw path=seed.iso \
  --memory size=32768M,hugepages=on,hugepage_size=1G \
  --net tap=tap0 \
  --device path=/sys/bus/pci/devices/0000:41:00.0/,x_nv_gpudirect_clique=0
```

**Tier 2 (HGX SXM GPUs, QEMU):**

```
qemu-system-x86_64 \
  -machine q35,accel=kvm \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE.fd \
  -drive if=pflash,format=raw,file=<runtime-dir>/OVMF_VARS.fd \
  -smp sockets=2,cores=40,threads=1 \
  -object memory-backend-file,id=mem0,size=960G,mem-path=/dev/hugepages,share=on,prealloc=on \
  -numa node,nodeid=0,cpus=0-39,memdev=mem0 \
  -device pcie-root-port,id=rp0,bus=pcie.0,chassis=1 \
  -device vfio-pci,host=0000:17:00.0,bus=rp0,x-no-mmap=true \
  -netdev tap,id=net0,ifname=tap0,script=no,downscript=no \
  -device virtio-net-pci,netdev=net0 \
  -chardev socket,id=serial0,path=<runtime-dir>/serial.sock,server=on,wait=off \
  -serial chardev:serial0 \
  -qmp unix:<runtime-dir>/qmp.sock,server=on,wait=off
```

QEMU uses QMP (unix socket) for process management (graceful shutdown, status queries). Cloud Hypervisor uses its HTTP API.

See [docs/gpu-passthrough.md](gpu-passthrough.md) for the full GPU architecture.

## Networking model

The same networking model applies to all boot paths when `network=true` in the RuntimeIntent.

```
Guest VM
  | virtio-net (eth0 inside guest)
  |
tap0  (created by network-init init container)
  |
br0   (192.168.99.1/24)
  |
pod network eth0  (NOT bridged — this was Bug 1)

dnsmasq: DHCP 192.168.99.10-20, gateway 192.168.99.1
iptables MASQUERADE on eth0 for guest outbound traffic
```

Key facts:
- `eth0` is never enslaved to `br0`. Bridging eth0 breaks pod networking.
- `br0` has its own IP (192.168.99.1) as the default gateway for guests.
- The network-init init container runs before swiftletd for both disk boot and kernel boot guests.
- dnsmasq is started by `launcher-entrypoint.sh` when `"network": true` in the RuntimeIntent.
- swiftletd polls `/var/lib/kubeswift/run/<guestId>/dnsmasq.leases` to discover the guest IP.
- The QEMU path uses the same tap0/br0 model via `-netdev tap,ifname=tap0`.

**Service exposure.** `spec.network.ports` programs an in-pod DNAT (`podIP:port → vmIP:targetPort` in `network-init.sh`) so a guest port becomes a normal Kubernetes Endpoint — the controller mints a ClusterIP/NodePort/LoadBalancer Service, `SwiftGuestPool.spec.service` load-balances one Service across replicas, and a VM→cluster egress reachability probe surfaces as the `EgressReady` condition. See [docs/networking/service-exposure.md](networking/service-exposure.md).

## API groups

| Group | CRDs | Notes |
|-------|------|-------|
| `swift.kubeswift.io/v1alpha1` | SwiftGuest, SwiftGuestClass, SwiftGuestPool | Core VM types + fleet management |
| `image.kubeswift.io/v1alpha1` | SwiftImage | Disk image lifecycle |
| `seed.kubeswift.io/v1alpha1` | SwiftSeedProfile | cloud-init NoCloud |
| `kernel.kubeswift.io/v1alpha1` | SwiftKernel | Direct kernel boot artifacts |
| `gpu.kubeswift.io/v1alpha1` | SwiftGPUProfile, SwiftGPUNode | GPU passthrough |
| `snapshot.kubeswift.io/v1alpha1` | SwiftSnapshot, SwiftRestore, SwiftSnapshotSchedule | Snapshot, restore, scheduled snapshots |
| `migration.kubeswift.io/v1alpha1` | SwiftMigration | Offline + live migration between nodes |
| `fleet.kubeswift.io/v1alpha1` | Cluster | Member cluster federated by the gateway hub |

13 CRDs across 8 API groups, all `v1alpha1`.

## Design principles

1. **Cloud Hypervisor first** — CH is the default. QEMU is only used for HGX SXM (Tier 2/3) GPUs that require a full PCIe hierarchy.
2. **Minimal changes** — One fix at a time, verified with real cluster output. No speculative patches.
3. **No silent failures** — Status fields must reflect real system state. Never drop errors silently.
4. **Kubernetes-native** — Everything observable via kubectl. Status fields must be accurate.
5. **Raw disk at runtime** — qcow2 is input format only. The runtime always uses raw.
6. **Distributed by design** — No single-node assumptions. Per-node artifact management via labels.
7. **Hardware-aware** — GPU workloads require correct PCIe topology, NUMA affinity, and driver version alignment.
