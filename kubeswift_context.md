# KubeSwift Project Context
> This document is the canonical context anchor for AI-assisted KubeSwift development.
> It should be read at the start of every new session before any work begins.
> Last updated: April 2, 2026 — SwiftGPU Phases 1-3 complete, host runtime hardening done

---

## What is KubeSwift

KubeSwift is a Kubernetes-native virtual machine runtime built on Cloud Hypervisor (with QEMU as a secondary runtime for GPU workloads).
It allows Kubernetes users to run virtual machines as first-class Kubernetes workloads,
using custom resources and controllers.

KubeSwift is **not** a container sandbox (not Kata Containers).
It is a VM platform where virtual machines are first-class Kubernetes workloads.

It is similar in spirit to KubeVirt but with a much simpler architecture:
- Runtime: Cloud Hypervisor (default) + QEMU (GPU workloads requiring PCIe topology)
- Firmware: rust-hypervisor-firmware v0.5.0 (disk boot), direct bzImage (kernel boot), OVMF (QEMU/GPU boot)
- Distribution: OCI-native (Helm chart + container images)
- Goal: minimal architecture, strong operability, fast iteration

Repository: `https://github.com/projectbeskar/kubeswift` (private)
Images: `ghcr.io/projectbeskar/kubeswift/` (public packages)

---

## Current State (v0.1.0 + SwiftKernel + Networking + SwiftGPU)

### What works end-to-end
- SwiftImage import: downloads qcow2, converts to raw, patches GRUB for serial console
- SwiftGuest lifecycle: creates launcher pod, boots VM, reports status
- Networking: tap+bridge+dnsmasq DHCP, guest gets IP, IP propagated to status
- `swiftctl console`: connects to serial socket, interactive console works
- `swiftctl start/stop/restart/debug`: implemented and working
- `swiftctl ssh <guest>`: SSH into guest via launcher pod with --user and --identity flags
- `swiftctl describe <guest>`: rich human-readable status
- `swiftctl logs <guest>`: tail swiftletd launcher logs
- Rich guest status: runtime.pid, runtime.hypervisor, console.serialSocket, network.interfaces[]
- Graceful stop via SIGTERM + 30s fallback to pod delete
- RestartPolicy=Never on launcher pod
- Controller stopped guard — no pod recreation when runPolicy=Stopped
- Image pipeline: sourceFormat, preparedFormat, size measurement via post-import job
- Smoke test: passes end-to-end (`make smoke-test`)
- Observability: Prometheus metrics, kubeswift_guest_running_total, kubeswift_vm_boot_seconds,
  kubeswift_vm_failures_total, kubeswift_image_import_seconds
- **SwiftKernel: per-node OCI artifact pull, kernel boot path, verified on CH v51.1**
- **SwiftKernel networking: faas-minimal gets DHCP IP via virtio-net, status.network.primaryIP populated**
- **SwiftGPU controller: GPU allocation, deallocation, finalizer-based cleanup**
- **SwiftGPUProfile and SwiftGPUNode CRDs (api/gpu/v1alpha1/)**
- **GPU pod building: gpu-init container, VFIO volumes, hugepage volumes, node pinning**
- **QEMU hypervisor path in swiftletd (Phase 1): swift-qemu-client crate, QMP lifecycle, OVMF boot**
- **Hypervisor override annotation (kubeswift.io/hypervisor-override) for testing QEMU without GPU hardware**
- **Tier-based hypervisor selection: pcie -> Cloud Hypervisor, hgx-shared/hgx-full -> QEMU**
- **NUMA-aware GPU allocation: prefers same NUMA node, falls back to cross-NUMA**
- **Fabric Manager partition selection for shared NVSwitch mode (Tier 2)**
- **GPU deallocation on SwiftGuest deletion via kubeswift.io/gpu-allocation finalizer**
- **Comprehensive tests: selectGPUs, findFMPartition, countFreeGPUs, idempotent allocation, deallocation**
- **gpu-init.sh: VFIO bind + Fabric Manager partition activation init container**
- **Containerfile updated: qemu-system-x86, ovmf, gpu-init.sh included in swiftletd image**
- **GPU Discovery DaemonSet (cmd/gpu-discovery/): auto-discovers GPUs, NUMA topology, NVSwitches, Fabric Manager via sysfs + lspci + lscpu + fmpm**
- **Discovery merge logic preserves controller-owned allocation fields during status patches**
- **Separate gpu-discovery container image (images/gpu-discovery/Containerfile) with pciutils**
- **Helm chart: gpuDiscovery.enabled gate for DaemonSet + RBAC templates**

### Known working configuration
- Guest OS (disk boot): **Ubuntu Focal (20.04)** cloud image — Noble (24.04) is incompatible
  with rust-hypervisor-firmware due to EFI protocol gaps (TPM, MOK, writable NVRAM)
- Guest OS (kernel boot): **faas-minimal** — Linux 6.6.44 + BusyBox musl, built with buildroot
- Firmware (disk boot): rust-hypervisor-firmware v0.5.0, loaded via `--kernel` flag (NOT `--firmware`)
- Firmware (kernel boot): direct bzImage via `--kernel`, initramfs via `--initramfs`
- Cloud Hypervisor: v51.1
- Seed format: NoCloud flat layout (meta-data, user-data, network-config at ISO root)
- Seed ISO: genisoimage with `-rock -joliet -volid cidata` flags
- DHCP range: 10.244.125.10–20 on br0 (10.244.125.1)
- ORAS CLI: v1.3.1 (used in SwiftKernel pull jobs)
- GPU (Tier 1): SwiftGPUProfile tier=pcie, Cloud Hypervisor, x_nv_gpudirect_clique=0
- GPU (Tier 2): SwiftGPUProfile tier=hgx-shared, QEMU, pcie-root-port per device, OVMF, 1Gi hugepages
- GPU runtime: QEMU from qemu-system-x86 package (Debian bookworm), OVMF at /usr/share/OVMF/
- GPU init: gpu-init.sh binds GPUs to vfio-pci, activates FM partition via fmpm

### Deployed images (latest)
- `ghcr.io/projectbeskar/kubeswift/controller-manager:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/swiftletd:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.1` (OCI artifact — use this, not 6.6.0)

---

## Architecture

### High-Level
```
User
 │
 │ kubectl / helm / swiftctl
 │
 ▼
Kubernetes API Server
 │  CRDs
 ▼
KubeSwift Controllers (Go, controller-runtime)
 │  create launcher pod
 ▼
SwiftGuest Pod
 ├─ init container: network-init (bridge/tap/dnsmasq setup) — both boot paths when network=true
 ├─ init container: gpu-init (VFIO bind, partition activate) — GPU boot path only
 └─ launcher container: swiftletd (Rust)
        │  entrypoint: launcher-entrypoint.sh starts dnsmasq when network=true in intent
        ▼
     Cloud Hypervisor v51.1 (default)
        │  disk boot:   --kernel hypervisor-fw --disk image.raw --disk seed.iso --net tap=tap0
        │  kernel boot: --kernel bzImage --initramfs rootfs.cpio.gz --cmdline "..." --net tap=tap0
     OR
     QEMU (GPU workloads)
        │  gpu boot:    qemu-system-x86_64 -machine q35 -device pcie-root-port -device vfio-pci ...
        ▼
      Guest VM
```

### CRDs

**SwiftGuest** — represents a running VM
```yaml
spec:
  imageRef:       # SwiftImage to boot from (optional, mutually exclusive with kernelRef)
  kernelRef:      # SwiftKernel to boot from (optional, mutually exclusive with imageRef)
  kernelCmdline:  # Per-guest kernel cmdline override (kernel boot only)
  guestClassRef:  # SwiftGuestClass for resource defaults
  seedProfileRef: # SwiftSeedProfile for cloud-init (disk boot only)
  runPolicy:      # Running | Stopped | RestartOnFailure | Always
  gpuProfileRef:  # SwiftGPUProfile for GPU passthrough (optional, triggers QEMU path)
status:
  phase:          # Pending | Scheduling | Running | Failed | Stopped
  conditions:     # Resolved | PodScheduled | GuestRunning | GPUAllocated
  nodeName:
  podRef:
  runtime:
    pid:          # Hypervisor process PID
    hypervisor:   # "cloud-hypervisor" | "qemu"
  console:
    serialSocket: # path to serial.sock
  network:
    primaryIP:    # discovered from dnsmasq DHCP lease
    interfaces:   # list of {name, ip}
  gpu:            # populated when gpuProfileRef is set
    devices:      # list of allocated GPU PCI addresses
    partitionId:  # Fabric Manager partition ID (shared mode)
    numaTopology: # resolved NUMA mapping
```

**SwiftImage** — represents a VM disk image
```yaml
spec:
  source:
    http:
      url: https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img
  format: qcow2   # converted to raw during import
status:
  phase: Ready | Importing | Validating | Preparing | Failed
  sourceFormat:   # original input format (e.g. qcow2)
  preparedFormat: # runtime format (always raw)
  preparedArtifact:
    pvcRef:       # PVC containing image.raw
    format: raw
    size:         # actual measured size of image.raw
```

**SwiftKernel** — represents a MicroVM kernel + initramfs OCI artifact
```yaml
spec:
  ociRef:
    image: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.1
    pullSecret: ""      # optional, for private registries
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"  # default cmdline
  profile: faas-minimal # informational label
status:
  phase: Pending | Pulling | Ready | Failed
  conditions:     # Ready | Failed | NoKernelNodes
  nodeStatuses:   # per-node pull progress
  - nodeName: miles
    phase: Ready
```

**SwiftSeedProfile** — cloud-init / NoCloud configuration
```yaml
spec:
  datasource: NoCloud
  userData: |    # cloud-config YAML
  metaData: |
  networkData: |
```

**SwiftGuestClass** — default resource profile
```yaml
spec:
  cpu: 2
  memory: 2048Mi
  rootDisk:
    size: 40Gi
    format: raw
```

**SwiftGPUProfile** — GPU passthrough configuration
```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
spec:
  count: 4                         # number of GPUs requested
  model: ""                        # optional model filter (e.g. "H200-SXM", "A100-PCIe", "L40S")
  partitionMode: shared            # full | shared | isolated
  pcieTopology:
    rootPortPerDevice: true        # place each GPU behind a pcie-root-port (QEMU)
    gpuDirectClique: 0             # x_nv_gpudirect_clique value (CH flat path)
  numaTopology:
    enabled: true                  # enable NUMA-aware vCPU/memory layout
    sockets: 2                     # virtual CPU sockets
    coresPerSocket: 40             # cores per socket
    threadsPerCore: 1              # SMT threads
    memoryPerSocketMi: 983040      # memory per NUMA node in MiB
  hugepages: 1Gi                   # hugepage size (1Gi for GPU workloads)
  vcpuPinning: true                # enable 1:1 vCPU to physical CPU pinning
  fabricManager:
    runInGuest: false              # true = full passthrough (NVSwitches passed to guest)
    requiredVersion: "580.95.05"   # must match host Fabric Manager version
```

Note: firmware selection is automatic — the controller sets "hypervisor-fw" for CH (tier=pcie)
and "ovmf" for QEMU (tier=hgx-shared or tier=hgx-full). There is no `firmware` field on the CRD.

**SwiftGPUNode** — per-node GPU inventory
```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUNode
metadata:
  name: <node-name>                # one per GPU node
  labels:
    kubeswift.io/gpu-node: "true"
spec: {}                           # no user-editable spec — discovery populates status
status:
  phase: Ready | Discovering | Error
  lastDiscovery: "2026-04-01T12:00:00Z"
  host:
    cpuTopology:                   # from lscpu / lstopo
      sockets: 2
      coresPerSocket: 48
      threadsPerCore: 2
    numaNodes:
    - id: 0
      cpus: "0-47,96-143"
      memoryMi: 1048576
    - id: 1
      cpus: "48-95,144-191"
      memoryMi: 1048576
    iommuEnabled: true
    hugepages1Gi:
      total: 400
      free: 400
  gpus:
  - index: 0
    pciAddress: "0000:17:00.0"
    model: "NVIDIA H200 SXM"
    deviceId: "10de:2336"
    numaNode: 0
    iommuGroup: 15
    driver: vfio-pci              # or nvidia, nouveau
    barSizes:                     # for QEMU x-no-mmap decisions
    - region: 0
      sizeMi: 64
    - region: 2
      sizeMi: 262144             # 256GB BAR — needs x-no-mmap=true
    allocated: false
    allocatedTo: ""               # namespace/guest-name when allocated
  - index: 1
    pciAddress: "0000:3d:00.0"
    model: "NVIDIA H200 SXM"
    # ...
  nvSwitches:
  - pciAddress: "0000:0a:00.0"
    deviceId: "10de:22a4"
    numaNode: 0
  fabricManager:
    installed: true
    version: "580.95.05"
    running: true
    partitions:
    - id: 0
      gpuIndices: [0, 1]
      active: false
      allocatedTo: ""
    - id: 1
      gpuIndices: [2, 3]
      active: false
      allocatedTo: ""
    - id: 2
      gpuIndices: [0, 1, 2, 3]
      active: false
      allocatedTo: ""
    - id: 3
      gpuIndices: [0, 1, 2, 3, 4, 5, 6, 7]
      active: false
      allocatedTo: ""
```

### Repository Structure
```
api/
  image/v1alpha1/         SwiftImage types
  kernel/v1alpha1/        SwiftKernel types
  seed/v1alpha1/          SwiftSeedProfile types
  swift/v1alpha1/         SwiftGuest, SwiftGuestClass types
  gpu/v1alpha1/           SwiftGPUProfile, SwiftGPUNode types
  shared/                 common types

cmd/
  swiftctl/               CLI
  controller-manager/     main entry point
  webhook-server/         webhook entry point
  gpu-discovery/          GPU hardware discovery DaemonSet binary

internal/
  controller/swiftguest/  SwiftGuest controller + gpu.go (GPU intent builder, GPU pod builder, NUMA/pinning)
  controller/swiftimage/  SwiftImage controller
  controller/swiftkernel/ SwiftKernel controller (per-node pull)
  controller/swiftgpu/    SwiftGPU controller (controller.go, allocate.go, status.go + tests)
  runtimeintent/          VM launch spec builder (disk + kernel + GPU boot paths)
  resolved/               Resolution and merge logic
  seed/                   cloud-init ConfigMap builder
  scheme/                 scheme registration (all API groups including gpu.kubeswift.io)

rust/
  swiftletd/              VM launcher (disk boot + kernel boot + GPU boot paths)
  swift-runtime/          RuntimeDir management
  swift-ch-client/        Cloud Hypervisor spawn + API client
  swift-qemu-client/      QEMU spawn + QMP client (lib.rs, config.rs, qmp.rs)
  swift-seed/             NoCloud ISO builder

build/
  kernels/
    faas-minimal/         buildroot external tree for faas-minimal kernel profile
      configs/            defconfig, linux config, busybox config
      rootfs-overlay/     /init script
      Makefile            build + verify-boot targets
      verify-boot.sh      boot verification script

images/
  gpu-discovery/Containerfile Go binary + pciutils for GPU hardware discovery
  swiftletd/Containerfile   (now includes qemu-system-x86_64 + OVMF for GPU path)
  swiftletd/scripts/
    network-init.sh         bridge/tap setup (runs as init container)
    gpu-init.sh             VFIO bind + FM partition activate (runs as init container)
    launcher-entrypoint.sh  starts dnsmasq when network=true, then execs swiftletd

config/
  crd/bases/              Generated CRD YAML (now includes gpu.kubeswift.io CRDs)
  samples/                Sample manifests (incl. swiftgpuprofile-pcie.yaml, swiftgpuprofile-hgx.yaml, swiftguest-gpu.yaml, swiftgpunode-sample.yaml)
  daemonset/              DaemonSet manifests (swiftletd, gpu-discovery)
  rbac/                   RBAC for gpu-discovery ServiceAccount/ClusterRole/ClusterRoleBinding
charts/kubeswift/         Helm chart (includes all CRDs, gpu-discovery gated by gpuDiscovery.enabled)
test/smoke/               End-to-end smoke test
test/gpu/                 GPU passthrough smoke test
```

### Networking Model (both boot paths)
```
Guest VM
   │ virtio-net (eth0 inside guest)
   │
  tap0
   │
  br0 (10.244.125.1/24)
   │
  pod network (eth0, NOT bridged)

dnsmasq: DHCP range 10.244.125.10–20, router 10.244.125.1
iptables: MASQUERADE on eth0 for guest outbound traffic
```

Key facts:
- `eth0` is **not** enslaved to `br0` — this was Bug 1 and broke pod networking
- `br0` has its own IP (`10.244.125.1`) as the default gateway for guests
- network-init init container runs when `rg.HasNetwork()` — both disk and kernel boot
- launcher-entrypoint.sh starts dnsmasq when `"network": true` in runtime intent
- swiftletd polls dnsmasq lease file to discover guest IP
- Guest IP written to pod annotation `kubeswift.io/guest-ip`
- Controller reads annotation and writes to `SwiftGuest.status.network.primaryIP`
- Controller requeues every 5s while Running but no IP yet (cache staleness fix)
- **GPU path**: QEMU uses same tap0/br0 networking model via `-netdev tap,ifname=tap0`

### SwiftKernel Architecture
```
SwiftKernel created
  │
  ▼
Controller lists nodes with label kubeswift.io/kernel-node=true
  │
  ├─ No labeled nodes → phase=Pending, condition=NoKernelNodes, log warning
  │
  └─ For each labeled node:
       Create pull Job: swiftkernel-pull-<name>-<nodename>
       Job nodeSelector: kubeswift.io/kernel-node=true + kubernetes.io/hostname=<node>
       Job runs: oras pull <image> → /var/lib/kubeswift/kernels/<ns>-<name>/
       │
       ▼
       status.nodeStatuses[nodeName].phase = Ready
  │
  ▼
All nodes Ready → status.phase = Ready
```

Key facts:
- Node opt-in label: `kubeswift.io/kernel-node=true`
- Artifact path on node: `/var/lib/kubeswift/kernels/<namespace>-<name>/bzImage`
  and `/var/lib/kubeswift/kernels/<namespace>-<name>/rootfs.cpio.gz`
- LocalPath is a deterministic function, not stored in status
- Controller watches Node label changes and requeues all SwiftKernels
- ORAS image: `ghcr.io/oras-project/oras:v1.3.1`
- OCI artifact media types:
  - kernel: `application/vnd.kubeswift.kernel.binary`
  - initramfs: `application/vnd.kubeswift.initramfs.binary`
  - artifact type: `application/vnd.kubeswift.kernel.v1`

### SwiftGPU Architecture
```
SwiftGPUNode discovery (cmd/gpu-discovery/)
  │
  ▼
Discovery DaemonSet (config/daemonset/gpu-discovery.yaml)
  │ runs on nodes with kubeswift.io/gpu-node=true
  │ reads: lspci -Dnn, lscpu, sysfs (numa_node, iommu_group, driver, BAR sizes)
  │ reads: fmpm -v, fmpm -q, systemctl is-active nvidia-fabricmanager
  │ loop: discover → merge (preserve controller-owned fields) → patch status → sleep 60s
  │
  ▼
SwiftGPUNode status populated (GPUs, NUMA, NVSwitches, partitions)
  │
  ▼
SwiftGuest with gpuProfileRef created
  │
  ▼
SwiftGPU controller:
  1. Resolve SwiftGPUProfile
  2. Find SwiftGPUNode with enough free GPUs matching model/count
  3. Allocate GPUs: mark allocated=true, set allocatedTo
  4. Determine hypervisor: QEMU if pcieTopology.rootPortPerDevice, else CH
  5. Determine NUMA: pick GPUs on same NUMA node(s), compute vCPU pinning
  6. If shared mode: select Fabric Manager partition, set pendingActivation
  7. Set condition GPUAllocated=True on SwiftGuest
  │
  ▼
SwiftGuest controller sees GPUAllocated=True:
  1. Generate RuntimeIntent with GPU fields
  2. Create launcher pod:
     - nodeSelector: kubernetes.io/hostname=<allocated-node>
     - init container: gpu-init (VFIO bind + FM partition activate)
     - init container: network-init (bridge/tap/dnsmasq)
     - launcher container: swiftletd (reads intent, spawns QEMU or CH)
     - volumes: /dev/vfio, /dev/kvm, hugepages
  3. Update status
  │
  ▼
swiftletd (Rust):
  1. Load RuntimeIntent
  2. If intent.hypervisor == "qemu": use QemuLauncher
     - Generate QEMU args: -machine q35, OVMF, hugepages, NUMA cells,
       PCIe root ports, VFIO devices, virtio-net, serial socket
  3. If intent.hypervisor == "cloud-hypervisor": use CloudHypervisorLauncher
     - Generate CH args: --device with x_nv_gpudirect_clique
  4. Monitor process, discover IP, report annotations (same as today)
```

### GPU Compatibility Tiers

KubeSwift GPU support covers three tiers based on hardware complexity:

**Tier 1: PCIe GPUs (A100-PCIe, L40S, RTX 4090, etc.)**
- Cloud Hypervisor path: `--device path=<sysfs>,x_nv_gpudirect_clique=0`
- Flat PCI topology is fine — CUDA initializes correctly
- No NVSwitch, no Fabric Manager needed
- Single NUMA node sufficient for 1-2 GPUs
- No OVMF/UEFI required (rust-hypervisor-firmware works)
- Simplest path — works with existing CH architecture + VFIO

**Tier 2: HGX SXM with shared NVSwitch (H100-SXM, H200-SXM, B200-SXM)**
- QEMU path required: CUDA refuses to init on flat PCI topology
- Each GPU must sit behind a `pcie-root-port` in guest
- Fabric Manager runs on host, manages NVSwitch partitions
- NUMA topology must match physical layout (2 sockets, N NUMA nodes)
- vCPU pinning critical for performance
- OVMF firmware required for UEFI boot
- 1G hugepages required
- Large BARs (up to 256GB per GPU) — may need `x-no-mmap=true` on QEMU
- Guest NVIDIA driver version must match host Fabric Manager version exactly

**Tier 3: HGX full passthrough (all 8 GPUs + NVSwitches to one VM)**
- QEMU path with full PCIe hierarchy: expander buses, switches, root ports
- NVSwitches passed through to guest alongside GPUs
- Fabric Manager runs inside the guest
- Full PCIe topology reconstruction per NVIDIA reference architecture
- Most complex configuration — Phase 4 of implementation

### Kernel Boot Path (SwiftGuest with kernelRef)
```
SwiftGuest (kernelRef=faas-minimal)
  │
  ▼
Controller resolves SwiftKernel → localPath
  │
  ▼
RuntimeIntent:
  kernelBoot:
    kernelPath:    /var/lib/kubeswift/kernels/default-faas-minimal/bzImage
    initramfsPath: /var/lib/kubeswift/kernels/default-faas-minimal/rootfs.cpio.gz
    cmdline:       console=ttyS0 root=/dev/ram0 rdinit=/init
  network: true
  rootDisk: {path: "", format: ""}
  │
  ▼
Pod:
  nodeSelector: kubeswift.io/kernel-node=true
  initContainer: network-init (bridge/tap setup)
  volumes: run, runtime-intent, dev-kvm, kernel-artifacts (hostPath)
  NO: root-disk PVC, seed volume
  │
  ▼
launcher-entrypoint.sh: starts dnsmasq (network=true), execs swiftletd
  │
  ▼
swiftletd:
  cloud-hypervisor \
    --kernel bzImage \
    --initramfs rootfs.cpio.gz \
    --cmdline "console=ttyS0 root=/dev/ram0 rdinit=/init" \
    --serial socket=serial.sock \
    --console off \
    --net tap=tap0
```

### Image Import Pipeline
```
SwiftImage created
  │
  ▼
Import Job (ubuntu:22.04, privileged)
  │  curl download
  │  qemu-img convert qcow2 → raw (if needed)
  │  GPT partition detection via od (NOT fdisk — fails on GPT)
  │  mount each partition, patch grub.cfg for serial console
  │  stat -c %s image.raw > image.raw.size
  ▼
Measure Job (ubuntu:22.04)
  │  cat /data/image.raw.size → parsed as int64
  ▼
image.raw stored in PVC
status.phase = Ready
```

### Cloud Hypervisor Invocation

**Disk boot:**
```
cloud-hypervisor \
  --kernel /usr/share/kubeswift-firmware/hypervisor-fw \
  --api-socket path=<runtime-dir>/ch.sock \
  --memory size=2048M \
  --cpus boot=2 \
  --disk path=<image.raw> path=<seed.iso> \
  --serial socket=<runtime-dir>/serial.sock \
  --console off \
  --net tap=tap0
```

**Kernel boot:**
```
cloud-hypervisor \
  --kernel <localPath>/bzImage \
  --initramfs <localPath>/rootfs.cpio.gz \
  --cmdline "console=ttyS0 root=/dev/ram0 rdinit=/init" \
  --api-socket path=<runtime-dir>/ch.sock \
  --memory size=2048M \
  --cpus boot=2 \
  --serial socket=<runtime-dir>/serial.sock \
  --console off \
  --net tap=tap0
```

**GPU boot (Cloud Hypervisor — Tier 1 PCIe GPUs only):**
```
cloud-hypervisor \
  --kernel /usr/share/kubeswift-firmware/hypervisor-fw \
  --api-socket path=<runtime-dir>/ch.sock \
  --memory size=32768M,hugepages=on,hugepage_size=1G \
  --cpus boot=16 \
  --disk path=<image.raw> path=<seed.iso> \
  --serial socket=<runtime-dir>/serial.sock \
  --console off \
  --net tap=tap0 \
  --device path=/sys/bus/pci/devices/0000:41:00.0/,x_nv_gpudirect_clique=0
```

### QEMU Invocation (Tier 2/3 GPU boot)
```
qemu-system-x86_64 \
  -name guest=<guestId>,debug-threads=on \
  -enable-kvm \
  -machine q35,accel=kvm \
  -cpu host \
  -smp sockets=2,cores=40,threads=1 \
  -m 1920G \
  -mem-path /dev/hugepages \
  -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE.fd \
  -drive if=pflash,format=raw,file=<runtime-dir>/OVMF_VARS.fd \
  -drive file=<image.raw>,format=raw,if=virtio \
  -drive file=<seed.iso>,format=raw,if=virtio \
  -netdev tap,id=net0,ifname=tap0,script=no,downscript=no \
  -device virtio-net-pci,netdev=net0,mac=<mac> \
  -chardev socket,id=serial0,path=<runtime-dir>/serial.sock,server=on,wait=off \
  -serial chardev:serial0 \
  -nographic \
  -object memory-backend-file,id=mem0,size=960G,mem-path=/dev/hugepages,share=on,prealloc=on \
  -object memory-backend-file,id=mem1,size=960G,mem-path=/dev/hugepages,share=on,prealloc=on \
  -numa node,nodeid=0,cpus=0-39,memdev=mem0 \
  -numa node,nodeid=1,cpus=40-79,memdev=mem1 \
  -device pcie-root-port,id=rp0,bus=pcie.0,chassis=1 \
  -device vfio-pci,host=0000:17:00.0,bus=rp0,x-no-mmap=true \
  -device pcie-root-port,id=rp1,bus=pcie.0,chassis=2 \
  -device vfio-pci,host=0000:3d:00.0,bus=rp1,x-no-mmap=true \
  -device pcie-root-port,id=rp2,bus=pcie.0,chassis=3 \
  -device vfio-pci,host=0000:60:00.0,bus=rp2,x-no-mmap=true \
  -device pcie-root-port,id=rp3,bus=pcie.0,chassis=4 \
  -device vfio-pci,host=0000:70:00.0,bus=rp3,x-no-mmap=true \
  -qmp unix:<runtime-dir>/qmp.sock,server=on,wait=off \
  -monitor none
```

Critical:
- `--kernel` with rust-hypervisor-firmware = PVH ELF binary (disk boot, CH only)
- `--kernel` with bzImage = direct Linux kernel (kernel boot, CH only)
- `--firmware` is for OVMF/EDK2 only — never use it in CH
- QEMU uses `-drive if=pflash` for OVMF, not `--kernel`/`--firmware`
- `imageRef` and `kernelRef` are mutually exclusive on SwiftGuest
- `gpuProfileRef` can combine with `imageRef` (disk boot + GPU) but NOT with `kernelRef`
- QEMU `-device vfio-pci` with `x-no-mmap=true` needed for GPUs with >64GB BARs
- Each VFIO device must sit behind its own `pcie-root-port` for Tier 2/3

### Status Reporting Architecture

swiftletd reports status via pod annotations (not SwiftGuest status directly).
The controller reads annotations on reconcile and maps them to SwiftGuest status.

| Annotation | Set by | Maps to |
|---|---|---|
| `kubeswift.io/guest-ip` | lease.rs (DHCP discovery) | status.network.primaryIP |
| `kubeswift.io/guest-interfaces` | lease.rs | status.network.interfaces[] |
| `kubeswift.io/guest-runtime-pid` | report.rs (socket ready) | status.runtime.pid |
| `kubeswift.io/guest-serial-socket` | report.rs (socket ready) | status.console.serialSocket |
| `kubeswift.io/guest-hypervisor` | report.rs | status.runtime.hypervisor |
| `kubeswift.io/gpu-devices` | report.rs (QEMU/CH started) | status.gpu.devices |

GuestRunning condition is patched directly to SwiftGuest status by swiftletd via
kube-rs DynamicObject patch (not via pod annotation).

---

## Bugs Fixed (Historical Reference)

| # | Component | Bug | Fix |
|---|-----------|-----|-----|
| 1 | network-init.sh | eth0 enslaved to br0, broke pod network | Remove eth0 from bridge, add NAT/masquerade |
| 2 | swift-seed | OpenStack ConfigDrive layout instead of NoCloud | Write flat files at ISO root |
| 3 | swiftletd | genisoimage missing -rock/-joliet/-volid flags | Add flags to genisoimage invocation |
| 4 | swift-runtime | genisoimage relative output path | Canonicalize output path |
| 5 | swiftimage/import.go | fdisk fails on GPT disks | Replace with od-based GPT LBA parser |
| 6 | swiftimage/import.go | GRUB patch skipped BOOT partition (ttyS0 already present) | Add unconditional terminal patch block |
| 7 | swiftimage/import.go | GRUB terminal patch order wrong (terminal_input before serial init) | Use awk rewrite for correct ordering |
| 8 | swift-ch-client/config.rs | --firmware used instead of --kernel | Revert to --kernel |
| 9 | config/samples/image.yaml | Ubuntu Noble incompatible with rust-hypervisor-firmware | Switch to Ubuntu Focal |
| 10 | swiftguest controller | No requeue when IP not yet discovered | Add RequeueAfter: 5s when Running + no IP |
| 11 | CRD schema | status.network missing from OpenAPI schema | Regenerate CRD, add to make deploy |
| 12 | smoke test | 2m network timeout too short | Increase to 5m, add --timeout-network flag |
| 13 | Makefile | make deploy didn't apply CRDs | Add kubectl apply -k config/crd + kubectl wait |
| 14 | seed profile | package_update: true caused slow first boot | Remove from sample seed profile |
| 15 | swiftletd/main.rs | on_socket_ready closure used nested tokio runtime | Reuse outer Arc<Runtime> via rt_clone |
| 16 | swiftletd/lease.rs | guest-interfaces annotation double-escaped JSON | Use serde_json::json! instead of format! |
| 17 | swiftletd/report.rs | report_guest_runtime patched SwiftGuest status directly | Patch pod annotations instead |
| 18 | swiftguest/pod.go | RestartPolicy not set, defaulted to Always | Set RestartPolicy: Never unconditionally |
| 19 | swiftguest/controller.go | Controller recreated pod when runPolicy=Stopped | Add stopped guard before pod create logic |
| 20 | rbac.yaml | controller-manager missing pods/log get permission | Add pods/log get rule to ClusterRole |
| 21 | swiftkernel/pull.go | --media-type flag not supported by oras v1.x | Remove flag, pull all layers |
| 22 | swiftkernel OCI artifact | Layer titles had full build path prefix | Repush from artifact directory for clean titles |
| 23 | swiftkernel CRD | nodeStatuses field missing from OpenAPI schema | Regenerate CRD after adding NodeKernelStatus type |
| 24 | rbac.yaml | controller-manager missing kernel.kubeswift.io permissions | Add SwiftKernel rules to ClusterRole |
| 25 | rbac.yaml | controller-manager missing nodes get/list/watch | Add nodes rule for SwiftKernel per-node scheduling |
| 26 | launcher-entrypoint.sh | network_enabled() checked seedPath not network field | Check "network":true directly in intent JSON |
| 27 | faas-minimal linux.config | CONFIG_PCI missing — virtio-net PCI device invisible to guest | Add CONFIG_PCI=y CONFIG_PCI_MSI=y |
| 28 | faas-minimal linux.config | CONFIG_NETDEVICES missing — Kconfig silently dropped VIRTIO_NET | Add CONFIG_NETDEVICES=y CONFIG_NET_CORE=y |
| 29 | launch.rs | kernel boot path hardcoded tap_name: None | Use intent.has_network() for tap_name |
| 30 | Rust commit | launch.rs changes not included in git commit | Add rust files explicitly to git add |
| 31 | swiftgpu controller | Duplicate controller name collision with swiftguest (both watch SwiftGuest) | Add explicit .Named("swiftgpu") to controller builder |
| 32 | swiftletd/report.rs + status.go | Hypervisor annotation never reported — status.runtime.hypervisor hardcoded to "cloud-hypervisor" | Add kubeswift.io/guest-hypervisor annotation in report.rs, read it in status.go |
| 33 | pod.go + security.go | network-init fails with "open: No such file or directory" — missing /dev/net/tun | Add dev-net-tun hostPath volume + mount on network-init and launcher when networking enabled |

---

## Deployment
```bash
make build-images push-images deploy
```

`make deploy` must:
1. Run `controller-gen` to regenerate CRD YAML from Go types
2. `kubectl apply -k config/crd` + wait for Established
3. Deploy controller-manager

**Never let the CRD schema drift from the Go types.** The API server silently drops unknown fields.

After CRD changes, always:
```bash
make generate
cp config/crd/bases/*.yaml charts/kubeswift/crds/
```

### Smoke Test
```bash
make smoke-test
# or with custom timeouts:
./test/smoke/boot-test.sh --timeout-image 15 --timeout-guest 5 --timeout-network 5
```

Success criteria:
- `GuestRunning=True`
- `status.network.primaryIP` populated

### SwiftKernel Quick Test
```bash
# Label a node
kubectl label node <nodename> kubeswift.io/kernel-node=true

# Create kernel artifact (use 6.6.1 — 6.6.0 has no networking)
kubectl apply -f config/samples/swiftkernel-faas.yaml
kubectl get swiftkernel faas-minimal -w  # wait for Ready

# Create guest using kernel boot
kubectl apply -f config/samples/swiftguest-faas.yaml
kubectl get swiftguest faas-test -w  # wait for Running + primaryIP
```

### SwiftGPU Quick Test
```bash
# Label a GPU node
kubectl label node <nodename> kubeswift.io/gpu-node=true

# Apply GPU RBAC (needed for gpu.kubeswift.io permissions)
kubectl apply -f config/manager/controller-manager-rbac.yaml

# Wait for discovery
kubectl get swiftgpunode <nodename> -o yaml  # check GPUs detected

# Create GPU profile (PCIe tier — Cloud Hypervisor)
kubectl apply -f config/samples/swiftgpuprofile-pcie.yaml

# Or for HGX SXM tier (QEMU):
# kubectl apply -f config/samples/swiftgpuprofile-hgx.yaml

# Create GPU guest
kubectl apply -f config/samples/swiftguest-gpu.yaml
kubectl get swiftguest gpu-test -w  # wait for Running + primaryIP

# Verify inside guest
swiftctl ssh gpu-test -- nvidia-smi
```

---

## Roadmap

### Completed (v0.1.0)
- VM boots end-to-end ✓
- Networking works ✓
- IP discovery and status reporting ✓
- swiftctl console/start/stop/restart/debug/ssh/describe/logs ✓
- Smoke test passes ✓
- Rich guest status ✓
- Graceful stop, RestartPolicy=Never, stopped guard ✓
- Image pipeline improvements ✓
- runPolicy: RestartOnFailure | Always with exponential backoff ✓
- Documentation ✓
- Observability: Prometheus metrics ✓

### Completed (SwiftKernel)
- faas-minimal buildroot profile: Linux 6.6.44 + BusyBox musl ✓
- Boot verified on Cloud Hypervisor v51.1 ✓
- OCI packaging: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.1 ✓
- SwiftKernel CRD: Pending | Pulling | Ready | Failed ✓
- Per-node pull via kubeswift.io/kernel-node=true label ✓
- nodeStatuses per-node tracking ✓
- SwiftGuest kernelRef boot path ✓
- RuntimeIntent kernel boot fields ✓
- swiftletd kernel boot: --kernel --initramfs --cmdline --net tap=tap0 ✓
- SwiftGuest phase=Running with kernel boot verified ✓
- DHCP networking: faas-minimal guest gets IP via virtio-net + udhcpc ✓
- status.network.primaryIP populated for kernel boot guests ✓
- Smoke test passes with kernel boot changes ✓

### Completed (SwiftGPU Phases 1-3)
- **Phase 1: QEMU Hypervisor Abstraction in swiftletd**
  - `hypervisor` field on RuntimeIntent: "cloud-hypervisor" (default) or "qemu"
  - New Rust crate: swift-qemu-client (lib.rs, config.rs, qmp.rs)
  - QemuProcess: spawn, QMP lifecycle (powerdown, quit, SIGKILL fallback)
  - QemuConfig: Q35 machine, OVMF firmware, KVM acceleration, virtio-net tap, serial socket
  - QMP client: synchronous Unix socket, capabilities negotiation, system_powerdown, quit
  - Containerfile updated: qemu-system-x86, ovmf, gpu-init.sh included
  - Hypervisor override annotation (kubeswift.io/hypervisor-override) for testing without GPU hardware
- **Phase 2: GPU CRDs and Resource Model**
  - SwiftGPUProfile CRD (api/gpu/v1alpha1/types_gpuprofile.go): tier, count, model, pcieTopology, numaTopology, hugepages, vcpuPinning, fabricManager
  - SwiftGPUNode CRD (api/gpu/v1alpha1/types_gpunode.go): cluster-scoped, status-only, host topology, GPU inventory, NVSwitch, Fabric Manager
  - SwiftGuest extended: spec.gpuProfileRef, status.gpu (GPUStatus), ConditionGPUAllocated
  - RuntimeIntent extended: gpu.devices[], gpu.firmware, gpu.numa, gpu.vcpuPinning, gpu.hugepages, gpu.fabricManagerPartitionId
  - Scheme registration for gpu.kubeswift.io in internal/scheme/
  - RBAC: gpu.kubeswift.io permissions in config/manager/controller-manager-rbac.yaml
  - Sample manifests: swiftgpuprofile-pcie.yaml, swiftgpuprofile-hgx.yaml, swiftguest-gpu.yaml, swiftgpunode-sample.yaml
- **Phase 3: SwiftGPU Controller and GPU Pod Building**
  - SwiftGPU controller (internal/controller/swiftgpu/): watches SwiftGuest, allocates GPUs on SwiftGPUNode
  - Controller named "swiftgpu" explicitly (.Named()) to avoid collision with swiftguest controller
  - NUMA-aware GPU selection: prefers single NUMA node, falls back to cross-NUMA
  - Fabric Manager partition selection for shared mode (findFMPartition)
  - Tier-based hypervisor selection: pcie -> cloud-hypervisor, hgx-shared/hgx-full -> qemu
  - Idempotent allocation: detects existing allocatedTo before re-allocating
  - Finalizer-based deallocation (kubeswift.io/gpu-allocation): frees GPUs and FM partitions on SwiftGuest delete
  - Graceful handling when SwiftGPUNode is gone during deallocation
  - GPU pod builder (internal/controller/swiftguest/gpu.go): BuildGPUDiskBootPod with gpu-init container, /dev/vfio volume, hugepage volume, node selector
  - GPU intent builder: resolves PCIe topology flags, NUMA layout, vCPU pinning from SwiftGPUNode host topology
  - gpu-init.sh: unbind from current driver, bind to vfio-pci, verify binding, activate FM partition via fmpm
  - Comprehensive unit tests: selectGPUs, findFMPartition, countFreeGPUs, idempotent allocation, deallocation, hypervisor selection, cross-NUMA fallback
  - Documentation: docs/gpu-passthrough.md with workflow, examples, troubleshooting

### Completed (Host Runtime Hardening)
- Removed `privileged: true` from all three pod containers (SEC-01, SEC-02, SEC-03)
- **network-init**: drop ALL + NET_ADMIN, NET_RAW — bridge/tap/iptables/dnsmasq
- **gpu-init**: drop ALL + SYS_ADMIN — sysfs writes for VFIO driver binding + fmpm
- **launcher (non-GPU)**: drop ALL + NET_ADMIN, SYS_ADMIN — tap device, KVM ioctls
- **launcher (GPU)**: drop ALL + NET_ADMIN, SYS_ADMIN, SYS_RESOURCE, DAC_OVERRIDE — adds hugepage mlock + VFIO device access
- All containers set `allowPrivilegeEscalation: false`
- Added `/sys/bus/pci` hostPath volume for gpu-init sysfs access without privileged
- PCI BDF validation in gpu-init.sh: regex rejects malformed addresses (SEC-05)
- FM partition ownership validation: `isFMPartitionOwnedBy()` check before pod creation (SEC-06)
- `/dev/vfio` scoping documented: directory mount required because VFIO group files are created during bind (SEC-04)
- Security audit findings SEC-01 through SEC-06 resolved or mitigated
- Unit tests for all security contexts: `security_test.go`

### Completed (dataDiskRef)
- `spec.dataDiskRef` on SwiftGuest: optional secondary SwiftImage reference
- Data disk appears as /dev/vdb inside guest (both CH `--disk` and QEMU `-drive`)
- Works with all boot paths: disk boot, kernel boot, GPU boot
- Resolver validates data disk SwiftImage exists and is Ready
- Pod builders add PVC volume + mount at `/var/lib/kubeswift/disks/data/`
- RuntimeIntent carries `dataDisk` field to swiftletd
- Sample manifests: `swiftimage-datadisk.yaml`, `swiftguest-datadisk.yaml`
- Comprehensive tests: resolver, runtime intent, pod builder, CH args, QEMU args

### Completed (GPU Discovery DaemonSet)
- Discovery binary at `cmd/gpu-discovery/`
- DaemonSet runs on nodes labeled `kubeswift.io/gpu-node=true`
- Discovers GPUs, NUMA topology, NVSwitches, Fabric Manager via sysfs + lspci + lscpu + fmpm
- Merge logic preserves controller-owned allocation fields during status patches
- Separate container image (`images/gpu-discovery/Containerfile`) with pciutils
- Helm chart gate: `gpuDiscovery.enabled` for DaemonSet + RBAC templates
- Validation report template at `docs/validation/discovery-daemonset-validation.md`

### Next Priorities (in order)

**1. Discovery DaemonSet validation on GPU hardware**
- Validate on a node with real NVIDIA GPUs (Hetzner, Equinix, or similar)
- Verify lspci parsing produces correct GPU inventory
- Verify BAR size detection for large-BAR GPUs
- Verify Fabric Manager discovery on HGX hardware (if available)

**2. Tier 1 GPU end-to-end validation**
- Rent a bare-metal server with a PCIe GPU (A100-PCIe, L40S, or RTX class)
- Full flow: label node -> discovery -> create profile (tier=pcie) -> create guest -> nvidia-smi inside guest
- Validate VFIO bind, Cloud Hypervisor --device passthrough, GPU driver init in guest

**3. Additional kernel profiles**
- gpu-workload profile: Linux kernel with NVIDIA driver modules, VFIO support
- vhost-user profile: for vhost-user-net/blk offload scenarios
- Build pipeline at build/kernels/<profile>/

**4. Windows guest support**
- OVMF/UEFI boot path (already implemented for QEMU GPU path)
- VirtIO driver ISO injection (virtio-win)
- Cloudbase-init or unattend.xml for Windows provisioning
- Guest agent for IP reporting (Windows doesn't use cloud-init)

**5. Multi-NIC support**
- SwiftGuest spec supports multiple network interfaces
- Per-NIC tap+bridge setup in network-init
- Use cases: management vs data plane separation, SR-IOV passthrough

**6. SwiftGuestPool**
- New CRD: SwiftGuestPool with desired replica count and guest template
- Controller creates/deletes SwiftGuests to match desired count
- Use case: GPU inference fleets with identical VM configurations

### SwiftGPU Continued (in order)

**7. Tier 2 GPU validation (HGX SXM)**
- Rent H100/H200 HGX bare-metal for validation sprint
- Validate QEMU + pcie-root-port + Fabric Manager partition flow
- Validate NUMA topology and vCPU pinning
- Validate x-no-mmap=true for large-BAR GPUs

**8. GPU Phase 4: Full PCIe Topology — Tier 3 (HGX full passthrough)**
- QEMU launch builds full PCIe hierarchy per NVIDIA reference architecture:
  - PCIe expander buses (one per NUMA node)
  - Root ports under each expander bus
  - PCIe switch upstream/downstream ports under root ports
  - VFIO devices placed on correct downstream ports matching physical topology
- NVSwitch devices passed through to guest alongside GPUs
- Fabric Manager runs inside guest (not on host)
- Full 8-GPU passthrough with NVLink all-to-all connectivity
- Host topology autodiscovery: lstopo/lspci parsing to generate PCIe map
- NCCL topology file injection (optional optimization for custom topologies)
- Target: single-tenant 8-GPU HGX VMs
- Verify: all 8 GPUs visible, nvidia-smi topo shows NVLink, NCCL bandwidth test passes
- Deliverable: full HGX passthrough matching NVIDIA reference VM configuration

### Long-term (not yet prioritized)

**9. Live migration**
- VM memory state serialization and transfer
- Shared storage requirement (network-attached PVCs)
- Coordinated IP handoff between nodes
- CH experimental migration support vs QEMU mature migration
- Design doc required before implementation

**10. Snapshots and persistent disks**
- VM disk snapshots for backup/restore
- Integration with CSI VolumeSnapshot (Ceph, Longhorn)
- Local storage snapshot strategy

**11. SR-IOV / Multus networking**
- Hardware NIC passthrough via SR-IOV VFs
- Multus CNI integration for multiple network attachments
- Complements multi-NIC support

**12. vGPU (mediated device) support**
- NVIDIA GRID vGPU for fractional GPU sharing
- Mediated device passthrough via mdev
- Different use case from full passthrough (lighter workloads)

**13. GPU health monitoring and automatic failover**
- Monitor GPU health via nvidia-smi or NVML inside discovery DaemonSet
- Detect Xid errors, ECC failures, thermal throttling
- Mark unhealthy GPUs as unallocatable
- Optional: migrate guest to different GPU on failure

---

## SwiftKernel Build Notes

### faas-minimal profile
- Location: `build/kernels/faas-minimal/`
- Buildroot version: 2024.02.6
- Linux version: 6.6.44
- Userspace: BusyBox 1.36.1, statically linked against musl
- Build: `cd build/kernels/faas-minimal && make setup && make build`
- Outputs: `output/images/bzImage` (~4.8MB), `output/images/rootfs.cpio.gz` (~498KB)
- Boot verified: cloud-hypervisor v51.1 on node miles with full DHCP networking

### Critical kernel config requirements
These must all be present or networking will not work:
```
CONFIG_PCI=y              # PCI bus — virtio-net is a PCI device
CONFIG_PCI_MSI=y          # PCI MSI interrupts
CONFIG_NETDEVICES=y       # Required parent for VIRTIO_NET
CONFIG_NET_CORE=y         # Required parent for VIRTIO_NET
CONFIG_VIRTIO_NET=y       # Virtio network driver
CONFIG_VIRTIO_PCI=y       # Virtio PCI transport
CONFIG_INET=y             # IPv4 stack
CONFIG_IP_PNP=y           # IP autoconfiguration
CONFIG_IP_PNP_DHCP=y      # DHCP support
```
Without CONFIG_PCI, the virtio-net PCI device is invisible to the guest.
Without CONFIG_NETDEVICES, Kconfig silently drops CONFIG_VIRTIO_NET.

### OCI packaging
- Always push from artifact directory for clean layer titles:
```bash
  cd build/kernels/faas-minimal/output/images
  oras push ghcr.io/projectbeskar/kubeswift/kernels/faas:<version> \
    --artifact-type application/vnd.kubeswift.kernel.v1 \
    --annotation "org.opencontainers.image.description=KubeSwift faas-minimal kernel 6.6.44" \
    bzImage:application/vnd.kubeswift.kernel.binary \
    rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary
```
- Use a new version tag when bzImage changes (kernel config changes = new tag)
- Current production tag: `6.6.1` (has networking)
- Do NOT use `6.6.0` — that tag has broken networking (missing CONFIG_PCI)

---

## Design Principles

When contributing, always follow these:

1. **Minimalism** — avoid unnecessary complexity, deps, abstraction layers
2. **Cloud Hypervisor first** — CH is the default runtime; QEMU only when hardware requires it
3. **Raw disk at runtime** — qcow2 is input only; runtime always uses raw
4. **Kubernetes-native** — everything observable via kubectl; status fields must be accurate
5. **Strong operability** — operators must be able to discover IP, connect console, SSH, inspect status
6. **No silent failures** — status fields must reflect real system state; never drop errors silently
7. **Verified fixes only** — no speculative patches; diagnose with real cluster output first
8. **Distributed by design** — no single-node assumptions; per-node artifact management via labels
9. **Hardware-aware** — GPU workloads require correct PCIe topology, NUMA affinity, and driver alignment

---

## AI Assistant Instructions

When helping develop KubeSwift:

- Read this document and the session transcript before starting any work
- Check `/mnt/transcripts/journal.txt` for previous session summaries
- Prefer minimal changes — one bug fix at a time, verified with real output
- Always ask for actual cluster output before suggesting fixes
- Never assume a fix worked without seeing logs confirming it
- All pod containers currently run privileged: true — this is intentional during development. Do not attempt to harden security contexts until the feature surface is stable.
- When writing Cursor prompts: be explicit about what NOT to change
- CRD changes always require `make generate` + copy to charts/kubeswift/crds/ + redeploy
- The working guest OS (disk boot) is Ubuntu Focal — do not suggest Noble
- rust-hypervisor-firmware uses `--kernel`, not `--firmware`
- The GRUB terminal patch order is: serial init → terminal_input → terminal_output
- swiftletd reports status via pod annotations, not direct SwiftGuest status patches
- RestartPolicy on launcher pods is always Never — controller owns VM lifecycle
- imageRef and kernelRef are mutually exclusive on SwiftGuest
- gpuProfileRef can combine with imageRef but NOT with kernelRef
- SwiftKernel node opt-in label: kubeswift.io/kernel-node=true
- SwiftGPU node opt-in label: kubeswift.io/gpu-node=true
- Kernel artifact localPath is deterministic: /var/lib/kubeswift/kernels/<namespace>-<name>/
- ORAS version in pull jobs: ghcr.io/oras-project/oras:v1.3.1
- Kernel boot pods have network-init init container (same as disk boot)
- launcher-entrypoint.sh starts dnsmasq when "network":true in intent JSON
- faas-minimal requires CONFIG_PCI + CONFIG_NETDEVICES for virtio-net to work
- Current production kernel tag is 6.6.1 — do NOT reference 6.6.0
- When kernel config changes, bump the OCI tag (6.6.0 → 6.6.1 → etc.)
- After any /init change: rebuild initramfs, repush OCI, delete+recreate SwiftKernel
- **GPU: Tier 1 PCIe GPUs use Cloud Hypervisor with x_nv_gpudirect_clique**
- **GPU: Tier 2/3 HGX SXM GPUs require QEMU with pcie-root-port per device**
- **GPU: CUDA refuses to init on flat PCI topology for B200/H200 SXM class GPUs**
- **GPU: Fabric Manager version on host must exactly match nvidia-open driver in guest**
- **GPU: Large BARs (>64GB) need x-no-mmap=true on QEMU to avoid boot stalls**
- **GPU: NVSwitch passthrough only for Tier 3 full passthrough — not for shared mode**
- **GPU: gpu-init container handles VFIO bind + FM partition activate before swiftletd starts**
- **GPU: QEMU path uses QMP (unix socket) for monitoring, not HTTP API like CH**
- **GPU: SwiftGPU controller name is "swiftgpu" (explicit .Named() to avoid collision with swiftguest controller)**
- **GPU: RBAC for gpu.kubeswift.io must be applied separately: kubectl apply -f config/manager/controller-manager-rbac.yaml**
- **GPU: Allocation is idempotent — if GPUs already marked allocatedTo this guest, they are returned without re-allocating**
- **GPU: Deallocation uses kubeswift.io/gpu-allocation finalizer on SwiftGuest**
- **GPU: Hypervisor override annotation kubeswift.io/hypervisor-override allows testing QEMU path without GPU hardware**
- **GPU: swift-qemu-client QMP is synchronous (std::os::unix::net::UnixStream), not async tokio**
- **GPU: SwiftGPUProfile has no `firmware` field — firmware is auto-selected based on tier (hypervisor-fw for CH, ovmf for QEMU)**
- **GPU: gpuProfileRef uses corev1.LocalObjectReference (same as imageRef/kernelRef), not a custom ObjectReference**
- **Security: NO container uses privileged: true — all use drop ALL + specific capabilities**
- **Security: network-init capabilities: NET_ADMIN, NET_RAW — do NOT add SYS_ADMIN**
- **Security: gpu-init capabilities: SYS_ADMIN — needs /sys/bus/pci hostPath volume (sysfs-pci)**
- **Security: launcher (non-GPU) capabilities: NET_ADMIN, SYS_ADMIN**
- **Security: launcher (GPU) capabilities: NET_ADMIN, SYS_ADMIN, SYS_RESOURCE, DAC_OVERRIDE**
- **Security: gpu-init.sh validates PCI BDF format before sysfs writes — do NOT remove validation**
- **Security: FM partition ownership checked via isFMPartitionOwnedBy() before pod creation**
- **Security: All containers set allowPrivilegeEscalation: false — do NOT revert to privileged: true**
