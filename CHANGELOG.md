# Changelog

All notable changes to KubeSwift are documented here.

---

## [Unreleased] — April 3, 2026

### Added

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
