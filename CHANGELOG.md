# Changelog

All notable changes to KubeSwift are documented here.

---

## [v0.3.0] — 2026-06-09

Consolidates everything since v0.1.0 (the v0.2.0-rc.1 tag from April was never
promoted and is superseded by this release). Roughly 500 commits across six
major feature arcs, each shipped with on-cluster validation walkthroughs.

### Highlights

- **VM snapshots, end to end (Phases 0–6)** — disk-only CSI snapshots,
  local memory snapshots, S3/object-storage export with zstd compression,
  boot-as-clone (`spec.cloneFromSnapshot`), cron scheduling with keep-N
  retention, Prometheus metrics + Grafana dashboard.
- **Live migration, end to end (Phases 1–5)** — offline migration for any
  guest, live migration with sub-3s observed downtime, mTLS-secured
  migration transport, `kubectl drain` integration, offline GPU
  evacuation, metrics + retention.
- **Windows guest support v1** — `osType: windows` on Cloud Hypervisor.
- **vhost-user devices** — virtiofs shared filesystems, vhost-user-net /
  -blk / generic devices (operator-provided backends).
- **Multi-node L2 networking foundation** — primary-on-NAD (experimental)
  and a corrected migration IP-preservation gate; multi-NIC support
  actually works now (three latent bugs fixed; smoke test passes 5/5).
- **Cloud Hypervisor v51.1 → v52.0** — fixes the Windows viostor bugcheck,
  resets guests in place on reboot, unlocks core-scheduling and
  restore/snapshot improvements.

### Added

**Snapshots & restore (`snapshot.kubeswift.io`)**
- SwiftSnapshot + SwiftRestore CRDs and controllers: Tier A CSI
  VolumeSnapshot (disk-only), Tier B local hostPath memory snapshots
  (CH pause/snapshot/resume), Tier C S3-compatible export/import
  (MinIO/AWS/RGW) with checksummed manifests and zstd-compressed memory
  ranges (`spec.backend.s3.compression`).
- `SwiftGuest.spec.cloneFromSnapshot`: boot N guests as clones of one
  memory snapshot (pool-templatable; per-clone hypervisor MAC; CH v52
  auto-resume + on-demand/userfaultfd memory restore).
- `SwiftImage.spec.cloneStrategy: copy|snapshot` for ≥3× faster pool scaling.
- SwiftSnapshotSchedule CRD: cron-created snapshots with `keepLast` pruning,
  reference-aware GC, `spec.ttl`, `spec.deletionPolicy: Delete|Retain` with
  prefix-scoped S3 purge.
- `swiftctl snapshot|restore|schedule` command groups; snapshot metrics,
  byte gauges, Grafana dashboard (`config/grafana/kubeswift-snapshots.json`).

**Live migration (`migration.kubeswift.io`)**
- SwiftMigration CRD + controller: offline mode (direct PVC reuse,
  ~25–70s downtime depending on CSI driver) and live mode (CH pre-copy,
  ~2–3s observed downtime, kernel-boot and RWX+Block disk-boot).
- mTLS migration transport: per-node cert-manager-issued identities,
  SAN-pinned stunnel sidecars (~1% overhead); plaintext path requires an
  explicit unsafe acknowledgement.
- `kubectl drain` integration: eviction webhook + drain controller
  auto-migrate guests per `spec.migration.drainPolicy`
  (Migrate|LiveMigrate|Block); universal per-guest `maxUnavailable: 0`
  PDB as the hard floor; VFIO/GPU guests evacuate offline via the GPU
  release-and-reallocate primitive (reserve-before-stop atomicity).
- Auto mode resolution (live when eligible, else offline), per-guest
  `spec.migration.enabled` pinning, `spec.allowIPChange` opt-in,
  `spec.timeout` (default 30m) and `spec.ttl` retention,
  `status.observedDowntime` / `status.transferProgress` /
  `status.observedTransferDuration`, typed `FailureReasonCode` taxonomy,
  migration metrics + Grafana dashboard.
- `swiftctl migrate` (with `--check` read-only preflight: target
  readiness/capacity, IP preservation, mode resolution, NFD-based CPU
  feature comparison) and `swiftctl migration list|describe|cancel`.

**Windows guests**
- `osType: windows` on SwiftImage/SwiftGuest: CH disk boot with
  `kvm_hyperv=on`, unprivileged import path, cloudbase-init provisioning
  over the existing NoCloud seed, image-prep tooling
  (`tools/windows-image-prep/`) producing virtio-ready images with
  headless BCD (EMS/SAC serial console).

**vhost-user devices**
- `SwiftGuest.spec.filesystems[]`: virtiofs shared filesystems (hostPath
  or PVC source, readOnly enforcement); swiftletd spawns virtiofsd
  (`--sandbox none`, no added capabilities) — full datapath
  cluster-validated.
- `GuestInterface.type: vhost-user` (+ `socket`, `mac`): virtio-net via an
  operator-provided DPDK/OVS backend.
- `SwiftGuest.spec.vhostUserDevices[]`: vhost-user-blk (SPDK-style) and
  generic vhost-user devices (`--generic-vhost-user`).
- Migration gate: guests with node-local virtio backends are offline-only
  (mirrors VFIO; auto resolves to offline).

**Networking**
- Multi-NIC: secondary interfaces via Multus NADs, SR-IOV VFIO NIC
  passthrough, mixed bridge+sriov guests.
- Multi-node L2 foundation: `GuestInterface.primary` lets the primary NIC
  ride a multi-node NAD (IP-preserving migration); corrected
  IP-preservation gate keyed on the primary interface; primary-on-NAD
  launcher runtime (EXPERIMENTAL — datapath pending validation on a
  multi-node L2 cluster); design + operator docs
  (`docs/design/network-architecture-requirements.md`,
  `docs/networking/multi-node-l2.md`).
- Networking operations guide, OVN-Kubernetes integration guide, ESXi/
  Proxmox concept mapping.

**Storage**
- `spec.storage` on SwiftGuestClass/SwiftGuest: accessMode / volumeMode /
  storageClassName selection with per-field merge; RWX+Block is the
  live-migration-capable combination (RWX+Filesystem rejected at
  admission); full Block-mode runtime path (volumeDevices end to end);
  `cloneStrategy: snapshot` works across volume modes
  (allow-volume-mode-change on the clone seed).

**GPU**
- SwiftGPU Phases 1–3: SwiftGPUProfile/SwiftGPUNode CRDs, allocation
  controller (NUMA-aware, FM partitions, finalizer dealloc), QEMU path
  (Q35/OVMF/QMP) for HGX tiers, GPU discovery DaemonSet (multi-vendor,
  60s cycle), Tier 1 PCIe passthrough validated on hardware (GTX 1080),
  IOMMU-group peer auto-binding, `vfioReady` + capacity pre-flights.

**Fleet & lifecycle**
- SwiftGuestPool: ReplicaSet-style fleets with rolling updates
  (maxUnavailable/maxSurge), topology spread, PVC-per-replica, scale
  subresource, node pre-assignment for snapshot clones.
- `dataDiskRef`/`dataDiskRefs` secondary disks (/dev/vdb) on all boot paths.
- Per-class vCPU core-scheduling (`SwiftGuestClass.spec.coreScheduling:
  off|vm|vcpu`) — SMT side-channel mitigation without disabling SMT.

**Operability & docs**
- GitOps: FluxCD reference repository (`examples/gitops-flux/`, three-layer
  model) + `docs/gitops/` operator docs.
- `swiftctl` grew `ssh`, `describe`, `logs`, robust pod resolution across
  migrations, and the command groups above.
- Controller-driven per-namespace swiftletd RBAC (no manual RoleBinding);
  `make deploy-with-webhook` / `deploy-with-webhook-and-mtls` targets;
  e2e suites wired into CI on path-touch triggers; THREAT-MODEL.md.

### Changed
- **Cloud Hypervisor v51.1 → v52.0** (platform-wide; Linux regression
  passed): fixes Windows viostor `0xD1` bugcheck; guests now **reset in
  place on reboot** (the launcher pod and CH survive — reboots no longer
  churn pods or trigger runPolicy); `CLOUDHV.fd` firmware unchanged
  (`ch-13b4963ec4`). CLOUDHV.fd replaced rust-hypervisor-firmware
  earlier in the cycle (all modern distros bootable; Ubuntu Noble is the
  primary guest OS).
- Guest RAM is now mapped `shared=on` (memfd MAP_SHARED): halves the
  launcher's guest-memory footprint and fixes memory-snapshot OOMs; the
  standard backing for snapshot/migration-capable guests.
- SwiftGuestClass default memory raised to 4Gi; launcher memory limits
  include a 512MiB overhead allowance.
- Root-disk import pipeline: qemu-img resize + sgdisk -e (GPT fix);
  cloud-init growpart expands on first boot.
- `status.observedPauseWindow` renamed to `status.observedTransferDuration`
  (it measures the full transfer RPC, not the vCPU pause).

### Fixed
- **Multi-NIC was silently broken end to end** (three stacked latent bugs):
  the network-init container had no runtime-intent mount (its multi-NIC
  path was unreachable), the launcher image lacked python3 (the intent
  parser), and vhost-user NICs tripped the NIC loop. All fixed; the
  long-flaky multi-nic smoke scenario now passes (smoke suite 5/5).
- **Tier A restore data loss** (silent fresh-boot instead of restored
  disk): `EnsureRootDiskClone` ordering fixed; regression-tested.
- Migration terminal-state handling: per-operation webhook discipline
  (finalizer traps, reconcile storms), chain-migration source-pod
  identity (`status.sourcePodRef`), offline-after-live pod-name trap,
  false-success on destination boot failure, downtime metrics anchored
  on real cutover timestamps.
- vswiftimage webhook: finalizer-removal trap on deletion AND
  pointer-identity spec comparison (metadata-only edits on Ready images
  were falsely rejected) — both fixed with content equality.
- swiftletd: lease poller survives transient RBAC/API failures; stale
  socket cleanup before CH spawn; receiver-mode GuestRunning reporting.
- S3 snapshot upload resume verifies sha256 (not just size); upload Job
  runs with the permissions the root-owned capture artifacts require.
- GPU walkthrough fixes: allocation re-stamp race, premature
  scheduling-atomicity check, reservation leak on guest-delete-mid-migration.
- `swiftctl debug` /proc scan anchors on argv[0] (no self-match);
  numerous gpu-init/container hardening fixes (sysfs shadowing, explicit
  interpreters, ASCII-only scripts).

### Known limitations (v0.3.0-rc.1)
- **Primary-on-NAD runtime is EXPERIMENTAL**: the launcher datapath is
  implemented but unvalidated (the dev cluster has no working multi-node
  L2); validate on an OVN-Kubernetes cluster before relying on
  IP-preserving migration.
- **vhost-user-net/-blk/generic datapaths are asset-gated**: CH wiring is
  cluster-validated, but line-rate operation needs operator-provided
  DPDK/SPDK backends (none on dev infra). virtiofs is fully validated.
- **SR-IOV NIC passthrough and Tier 2/3 HGX GPU support** are code-complete
  but hardware-unvalidated (no SR-IOV NICs / HGX systems available).
- **Cross-node GPU migration destination boot** is not hardware-validated
  (single GPU node); the release/reserve choreography is.
- **Windows in-cluster cloudbase-init provisioning** is untested (no
  Windows license on the dev cluster); every other Windows layer is
  validated.
- All API groups remain **v1alpha1**.

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
