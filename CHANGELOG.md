# Changelog

All notable changes to KubeSwift are documented here.

---

## [Unreleased] - 2026-04-05

### Changed (Cloud Hypervisor v51.1 -> v52.0 — platform-wide)
- Bumped the Cloud Hypervisor static binary in the `swiftletd` image from v51.1 to
  v52.0 (`images/swiftletd/Containerfile` `CH_VERSION`). v51.1's virtio-blk has a
  bug that bugchecks Windows' viostor driver (`0xD1 DRIVER_IRQL_NOT_LESS_OR_EQUAL`)
  in a reboot loop; v52.0 fixes it and boots Windows cleanly and stably (spike:
  `docs/design/windows-guest-support-spike.md` §4.1). This unblocks the CH-first
  path for upcoming Windows guest support.
- The `CLOUDHV.fd` firmware is **unchanged** (`ch-13b4963ec4`) — the spike-validated
  pairing with the v52.0 binary. `CurrentHypervisorVersion` is read from CH at
  runtime (vm.info), so it tracks the new version automatically; no code constant
  to change.
- Platform-wide change (the VMM for Linux guests too): a Linux-guest regression pass
  (smoke-test + the snapshot/migration suites) lands with the redeploy.

### Fixed (Phase 3a downtime metrics — W27 follow-up)
- W27a: `status.observedDowntime` previously measured two adjacent `metav1.Now()` calls in the same reconcile invocation, producing sub-millisecond nonsense (34-114µs across all 17 PR #46 + E12 walkthrough runs). Now anchored on new `status.cutoverStep2DispatchedAt` (stamped by `cutoverStep2` on Delete dispatch); reflects the operator-visible cutover-to-resume window. Defensive nil-check leaves the field unset rather than reporting a wrong value if the timestamp is missing.
- W27b: `status.observedPauseWindow` plumbing was half-implemented — swiftletd-on-src wrote the `kubeswift.io/migration-pause-window-ms` annotation correctly but the controller had zero readers, leaving the field permanently nil. Now stamped at `substateSrcCompleted` (W1 gate observation), mirroring the snapshot controller's parallel pattern. Defensive parse-failure handling.
- New status field `cutoverStep2DispatchedAt *metav1.Time` on SwiftMigration: operator-visible audit data (`kubectl get smig -o wide`) and authoritative anchor for `observedDowntime`. Phase 1 offline mode does not populate this field.
- Closes Tracked Follow-up #7 in kubeswift_context.md.

### Added
- Comprehensive networking operations guide for physical networks, VLANs, bonds, and isolated networks
- VMware ESXi and Proxmox VE concept mapping for platform migration
- SwiftGuestPool CRD and controller for VM fleet management (ReplicaSet semantics)
- kubectl scale support for SwiftGuestPool via scale subresource
- Stable naming, failed VM replacement, cascade deletion
- Rolling updates for SwiftGuestPool (RollingUpdate/Recreate strategy, maxUnavailable/maxSurge)
- Topology spread for SwiftGuestPool (spreadPolicy shorthand, topologySpreadConstraints)
- PVC per replica for SwiftGuestPool (volumeClaimTemplates, StatefulSet-like persistent storage)
- dataDiskRefs on SwiftGuest for multiple data disk references (SwiftImage or PVC)
- TopologySpreadConstraints on SwiftGuest (flows to launcher pod)
- Multi-vendor GPU discovery (AMD, Intel, NVIDIA) -- class-based PCI detection
- Tier 1 GPU passthrough validated on real hardware (GeForce GTX 1080)
- GPU discovery DaemonSet validated on Hetzner bare-metal
- IOMMU group peer auto-binding in gpu-init.sh (consumer NVIDIA GPUs)
- Root disk resize pipeline: qemu-img resize + sgdisk -e during import
- SwiftGuestClass default bumped to 4Gi RAM
- GTX 1080 sample manifests (swiftgpuprofile-gtx1080.yaml, swiftguest-gpu-gtx1080.yaml)

### Fixed
- gpu-init: mount host /sys at /host/sys to avoid container sysfs shadow (bug 35)
- gpu-init: use DirectoryOrCreate for /dev/vfio hostPath (bug 36)
- gpu-init: replace Unicode em dashes with ASCII (bug 37)
- gpu-init: use readlink without -f for host sysfs symlinks (bug 40)
- Containerfile: use explicit /bin/sh interpreter in ENTRYPOINT (bug 38)
- Init containers: use explicit interpreter in command array (bug 39)
- launch.rs: read intent.gpu.devices for CH --device and QEMU -device args (bug 41)
- Pod builder: add 512MiB memory overhead to container limits (bug 42)
- RBAC: add pods/log permission to controller-manager ClusterRole (bug 43)
- Makefile: default IMAGE_TAG to sha-$(git rev-parse --short HEAD) (bug 44)
- Import pipeline: resize image.raw to rootDisk.size (bug 45)
- Import pipeline: fix GPT backup header with sgdisk -e after resize (bug 46)

---

## [Unreleased] — April 3, 2026

### Added

**Firmware migration: CLOUDHV.fd replaces rust-hypervisor-firmware**
- Replaced rust-hypervisor-firmware with CLOUDHV.fd (Cloud Hypervisor EDK2/OVMF UEFI firmware)
- All modern Linux distributions now supported: Ubuntu 22.04+, Rocky 9, Fedora, Debian 12
- Ubuntu Focal 20.04 is no longer required — Ubuntu Noble 24.04 is the primary guest OS
- CLOUDHV.fd is pinned to release ch-13b4963ec4 with SHA256 verification
- GPU Tier 1 (CH path) firmware changed from "hypervisor-fw" to "cloudhv"
- QEMU path firmware (OVMF_CODE.fd / OVMF_VARS.fd) unchanged

**dataDiskRef on SwiftGuest**
- New optional field `spec.dataDiskRef` on SwiftGuest: references a SwiftImage as a secondary data disk
- Data disk appears as /dev/vdb inside the guest (both Cloud Hypervisor and QEMU paths)
- Works with all three boot paths: disk boot, kernel boot, GPU boot
- Resolver validates the referenced SwiftImage exists and is in Ready state
- Pod builders (disk, kernel, GPU) add PVC volume + mount at `/var/lib/kubeswift/disks/data/`
- RuntimeIntent carries `dataDisk` field; swiftletd passes as additional `--disk` (CH) or `-drive` (QEMU)
- Sample manifests: `swiftimage-datadisk.yaml`, `swiftguest-datadisk.yaml`
- Tests: resolver (success, missing, not ready, backward compat), runtime intent (disk boot, kernel boot, roundtrip), pod builder (disk boot, GPU boot, no data disk), CH args, QEMU args

**GPU Discovery DaemonSet**
- Discovery binary at `cmd/gpu-discovery/` auto-discovers GPUs, NUMA topology, NVSwitches, Fabric Manager
- DaemonSet runs on nodes labeled `kubeswift.io/gpu-node=true` with 60s re-discovery cycle
- Merge logic preserves controller-owned allocation fields during SwiftGPUNode status patches
- Separate container image with pciutils for lspci
- Helm chart: `gpuDiscovery.enabled` gate for DaemonSet + RBAC templates
- Validation report template at `docs/validation/discovery-daemonset-validation.md`

**Roadmap update**
- Comprehensive roadmap through 13 priority items including GPU hardware validation, Windows guests, multi-NIC, SwiftGuestPool, live migration, vGPU

---

## [Unreleased] — SwiftGPU Phases 1-3 Complete (April 2, 2026)

### Added

**SwiftGPU Phase 1: QEMU Hypervisor Abstraction**
- New Rust crate `swift-qemu-client` (`rust/swift-qemu-client/`) with QemuProcess, QemuConfig, and QmpClient
- QEMU disk boot via Q35 machine type, OVMF firmware, KVM acceleration
- QMP lifecycle management: capabilities negotiation, system_powerdown, quit, SIGKILL fallback
- `hypervisor` field on RuntimeIntent: selects "cloud-hypervisor" (default) or "qemu"
- `kubeswift.io/hypervisor-override` annotation on SwiftGuest for testing QEMU path without GPU hardware
- swiftletd Containerfile updated to include `qemu-system-x86`, `ovmf`, and `gpu-init.sh`

**SwiftGPU Phase 2: GPU CRDs and Resource Model**
- SwiftGPUProfile CRD (`api/gpu/v1alpha1/types_gpuprofile.go`): tier, count, model, pcieTopology, numaTopology, hugepages, vcpuPinning, fabricManager
- SwiftGPUNode CRD (`api/gpu/v1alpha1/types_gpunode.go`): cluster-scoped, status-only GPU inventory with host topology, NVSwitch, and Fabric Manager state
- SwiftGuest extended with `spec.gpuProfileRef` and `status.gpu` (GPUStatus with devices, partitionId, numaNodes, hypervisor, nodeName)
- `ConditionGPUAllocated` condition on SwiftGuest
- RuntimeIntent GPU extensions: devices[], firmware, numa, vcpuPinning, hugepages, fabricManagerPartitionId, nvSwitches
- Scheme registration for `gpu.kubeswift.io` API group in `internal/scheme/scheme.go`
- RBAC rules for `gpu.kubeswift.io` in `config/manager/controller-manager-rbac.yaml`
- Sample manifests: `swiftgpuprofile-pcie.yaml`, `swiftgpuprofile-hgx.yaml`, `swiftguest-gpu.yaml`, `swiftgpunode-sample.yaml`

**SwiftGPU Phase 3: Controller, Allocation, and GPU Pod Building**
- SwiftGPU controller (`internal/controller/swiftgpu/`): reconciles SwiftGuest objects with gpuProfileRef
- Controller registered with explicit `.Named("swiftgpu")` to avoid collision with SwiftGuest controller
- NUMA-aware GPU selection: prefers single NUMA node, falls back to cross-NUMA allocation
- Fabric Manager partition selection for shared NVSwitch mode (Tier 2)
- Tier-based hypervisor resolution: `pcie` -> cloud-hypervisor, `hgx-shared`/`hgx-full` -> qemu
- Idempotent allocation: detects existing `allocatedTo` on GPUs before re-allocating
- Finalizer-based deallocation (`kubeswift.io/gpu-allocation`): frees GPUs and FM partitions on SwiftGuest deletion
- Graceful deallocation when SwiftGPUNode has been deleted
- GPU pod builder (`internal/controller/swiftguest/gpu.go`): BuildGPUDiskBootPod with gpu-init container, /dev/vfio volume, hugepage volume, node pinning
- GPU intent builder: resolves PCIe topology flags, NUMA layout, vCPU pinning map from SwiftGPUNode host topology
- `gpu-init.sh` init container script: unbinds GPUs from current driver, binds to vfio-pci, verifies binding, activates FM partition via `fmpm`
- SwiftGPUNode watch: re-enqueues unallocated GPU guests when node capacity changes
- Documentation: `docs/gpu-passthrough.md` with prerequisites, tier reference, workflow, examples, troubleshooting

**Stabilization**
- Comprehensive unit tests for GPU allocation logic: selectGPUs (single, same-NUMA, cross-NUMA, insufficient, model filter), findFMPartition, countFreeGPUs, numaSetToSlice
- Controller integration tests: allocate success, no capacity, profile not found, already allocated, idempotent recovery, deallocation, node gone, finalizer lifecycle, hypervisor selection (PCIe vs HGX)
- Security audit notes in `docs/security-audit.md`

### Fixed

- Controller name collision: SwiftGPU and SwiftGuest controllers both watch SwiftGuest; resolved with explicit `.Named("swiftgpu")` (Bug 31)

---

## [v0.1.0] — SwiftKernel + Networking (March 2026)

### Added

- SwiftImage import: HTTP source, qcow2-to-raw conversion, GRUB serial console patching
- SwiftGuest lifecycle: launcher pod creation, VM boot, status reporting via pod annotations
- Networking: tap+bridge+dnsmasq, guest IP discovery, status.network.primaryIP
- swiftctl CLI: console, start, stop, restart, debug, ssh, describe, logs
- SwiftSeedProfile: NoCloud cloud-init for user-data, SSH keys, network config
- RunPolicy: Running, Stopped, RestartOnFailure, Always with exponential backoff
- Observability: Prometheus metrics (boot time, running count, failure count, import time)
- SwiftKernel: per-node OCI artifact pull, kernel boot path (bzImage + initramfs)
- faas-minimal kernel profile: Linux 6.6.44 + BusyBox musl via buildroot
- SwiftKernel networking: DHCP IP via virtio-net on kernel boot guests
- Smoke test: end-to-end boot verification

### Known Issues

See the Bugs Fixed table in [kubeswift_context.md](kubeswift_context.md) for the full history of bugs 1-30.
