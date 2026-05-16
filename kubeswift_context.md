# KubeSwift Project Context
> This document is the canonical context anchor for AI-assisted KubeSwift development.
> It should be read at the start of every new session before any work begins.
> Last updated: April 29, 2026 — Live Migration Phase 1 shipped; Snapshot Phases 0/1/2 operator-validated

---

## What is KubeSwift

KubeSwift is a Kubernetes-native virtual machine runtime built on Cloud Hypervisor (with QEMU as a secondary runtime for GPU workloads).
It allows Kubernetes users to run virtual machines as first-class Kubernetes workloads,
using custom resources and controllers.

KubeSwift is **not** a container sandbox (not Kata Containers).
It is a VM platform where virtual machines are first-class Kubernetes workloads.

It is similar in spirit to KubeVirt but with a much simpler architecture:
- Runtime: Cloud Hypervisor (default) + QEMU (GPU workloads requiring PCIe topology)
- Firmware: CLOUDHV.fd (EDK2/OVMF UEFI firmware for Cloud Hypervisor, disk boot), direct bzImage (kernel boot), OVMF (QEMU/GPU boot)
- Distribution: OCI-native (Helm chart + container images)
- Goal: minimal architecture, strong operability, fast iteration

Repository: `https://github.com/projectbeskar/kubeswift` (private)
Images: `ghcr.io/projectbeskar/kubeswift/` (public packages)

---

## Current State (v0.2.0+ with SwiftKernel + Networking + SwiftGPU + Snapshots Phases 0/1/2 + Live Migration Phase 1)

### What works end-to-end
- SwiftImage import: downloads qcow2, converts to raw, patches GRUB for serial console, qemu-img resize + sgdisk -e for proper disk sizing
- SwiftGuest lifecycle: creates launcher pod, boots VM, reports status
- Networking: tap+bridge+dnsmasq DHCP, guest gets IP, IP propagated to status
- Multi-NIC support: primary + secondary NICs via Multus NADs
- SR-IOV NIC passthrough via VFIO with mixed bridge+sriov in same guest
- `swiftctl console`: connects to serial socket, interactive console works
- `swiftctl start/stop/restart/debug`: implemented and working
- `swiftctl ssh <guest>`: SSH into guest via launcher pod with --user and --identity flags
- `swiftctl describe <guest>`: rich human-readable status
- `swiftctl logs <guest>`: tail swiftletd launcher logs
- `swiftctl snapshot` and `swiftctl restore`: snapshot/restore CLI subcommands (Phase 1/2)
- `swiftctl migrate <guest> --to <node>`: SwiftMigration CLI (Phase 1 of live migration)
- `swiftctl migration list/describe/cancel`: migration management
- Rich guest status: runtime.pid, runtime.hypervisor, console.serialSocket, network.interfaces[], gpu.devices, migration state
- Graceful stop via SIGTERM + 30s fallback to pod delete
- RestartPolicy=Never on launcher pod
- Controller stopped guard — no pod recreation when runPolicy=Stopped
- Image pipeline: sourceFormat, preparedFormat, size measurement, qemu-img resize + sgdisk -e
- Smoke test: passes 4/5 scenarios end-to-end (`make smoke-test` — multi-nic flake on Longhorn unrelated to KubeSwift code)
- Observability: Prometheus metrics
- SwiftKernel: per-node OCI artifact pull, kernel boot path, verified on CH v51.1
- SwiftKernel networking: faas-minimal gets DHCP IP via virtio-net, status.network.primaryIP populated
- SwiftGPU controller: GPU allocation, deallocation, finalizer-based cleanup
- SwiftGPUProfile and SwiftGPUNode CRDs (api/gpu/v1alpha1/)
- GPU pod building: gpu-init container, VFIO volumes, hugepage volumes, node pinning
- QEMU hypervisor path in swiftletd: swift-qemu-client crate, QMP lifecycle, OVMF boot
- Hypervisor override annotation (kubeswift.io/hypervisor-override) for testing QEMU without GPU hardware
- Tier-based hypervisor selection: pcie -> Cloud Hypervisor, hgx-shared/hgx-full -> QEMU
- NUMA-aware GPU allocation, Fabric Manager partition selection, GPU finalizer-based deallocation
- GPU Discovery DaemonSet (cmd/gpu-discovery/) auto-discovers GPUs/NUMA/NVSwitches/FM via sysfs/lspci/lscpu/fmpm
- Image resize pipeline: qemu-img resize + sgdisk -e GPT fix during import
- Cloud-init growpart handles partition + filesystem expansion on first guest boot
- Tier 1 GPU passthrough: GTX 1080 via Cloud Hypervisor --device on Hetzner bare-metal
- IOMMU group peer binding: auto-detects and binds companion devices (HD Audio) to vfio-pci
- **CSI VolumeSnapshot-based VM disk snapshots (Tier A) — disk-only backup/restore**
- **Local hostPath memory snapshots (Tier B) — in-place restore + clone restore with hypervisor-layer MAC isolation**
- **SwiftImage cloneStrategy: copy|snapshot for fast pool scaling**
- **Snapshot CLI: swiftctl snapshot create/list/describe/delete and swiftctl restore create/list/describe/delete**
- **SwiftGuestPool with rolling updates, topology spread, PVC per replica**
- **Snapshot operator walkthrough: 8 scenarios documented, sample manifests, findings catalog**
- **CI runs e2e tests on path-touch trigger (snapshot PR #22 wiring)**
- **SwiftMigration CRD with offline migration controller (Phase 1 of live migration)**
- **SwiftMigration validation webhook with per-operation discipline (PR #26)**
- **Direct PVC reuse for offline migration — Approach A from spike, no snapshot+restore overhead**
- **VFIO/SR-IOV migration: explicit Phase 1 webhook rejection (cross-node not supported until Phase 4+)**
- **Mode auto-selection logic: VFIO → offline; non-VFIO → offline (Phase 1 only ships offline)**
- **SwiftGuest.spec.nodeName field with direct pod.Spec.NodeName binding**
- **SwiftGuest.spec.migration block (enabled, preferredMode) for per-guest migration policy**

### Known working configuration
- Guest OS (disk boot): Ubuntu Noble (24.04) cloud image — all modern distributions supported
- Guest OS (kernel boot): faas-minimal — Linux 6.6.44 + BusyBox musl
- Firmware (disk boot): CLOUDHV.fd loaded via `--kernel` flag (NOT `--firmware`)
- Cloud Hypervisor: v51.1
- Seed format: NoCloud flat layout
- DHCP range: 10.244.125.10–20 on br0 (10.244.125.1)
- ORAS CLI: v1.3.1
- GPU (Tier 1): tier=pcie, Cloud Hypervisor, x_nv_gpudirect_clique=0
- GPU (Tier 2): tier=hgx-shared, QEMU, pcie-root-port per device, OVMF, 1Gi hugepages
- Default SwiftGuestClass: 4Gi RAM
- Snapshots Phase 1: CSI VolumeSnapshot via Longhorn, Retain or Delete deletion policies
- Snapshots Phase 2: local hostPath at /var/lib/kubeswift/snapshots/, ~2.8s/GiB pause window on Longhorn
- Live Migration Phase 1: direct PVC reuse, ~70s downtime on Longhorn (32s shutdown + 13s detach + 25s boot), ~25s on true CoW drivers (boot-bound)

### Deployed images (latest)
- `ghcr.io/projectbeskar/kubeswift/controller-manager:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/swiftletd:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/gpu-discovery:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/snapshot-stager:sha-<latest>`
- `ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.1`

---

## Architecture

### High-Level
```
User
 │
 │ kubectl / helm / swiftctl
 ▼
Kubernetes API Server
 │  CRDs
 ▼
KubeSwift Controllers (Go, controller-runtime)
 │  create launcher pod
 ▼
SwiftGuest Pod
 ├─ init container: network-init (bridge/tap/dnsmasq setup)
 ├─ init container: gpu-init (VFIO bind, partition activate) — GPU only
 ├─ init container: snapshot-stager (clone restore only)
 └─ launcher container: swiftletd (Rust)
        ▼
     Cloud Hypervisor v51.1 (default)
        │  disk boot:   --kernel CLOUDHV.fd --disk image.raw --disk seed.iso --net tap=tap0
        │  kernel boot: --kernel bzImage --initramfs rootfs.cpio.gz --cmdline "..." --net tap=tap0
        │  restore:     --restore source_url=file://<snapshot-path>
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
  imageRef:
  kernelRef:
  kernelCmdline:
  guestClassRef:
  seedProfileRef:
  runPolicy: Running | Stopped | RestartOnFailure | Always
  gpuProfileRef:
  nodeName:                         # NEW (Phase 1 live migration)
  migration:                        # NEW (Phase 1 live migration)
    enabled: true                   # default true
    preferredMode: auto | live | offline  # default auto
  interfaces:
  - name: mgmt
  - name: data
    type: bridge | sriov
    networkRef: ...
    resourceName: ...
  dataDiskRef:
status:
  phase: Pending | Scheduling | Running | Failed | Stopped
  conditions: [Resolved, PodScheduled, GuestRunning, GPUAllocated]
  nodeName:
  podRef:
  runtime:
    pid:
    hypervisor: cloud-hypervisor | qemu
  console:
    serialSocket:
  network:
    primaryIP:
    interfaces: [{name, mac, ip}]
  gpu:
    devices: [...]
    partitionId:
    numaTopology: ...
```

**SwiftImage**, **SwiftKernel**, **SwiftSeedProfile**, **SwiftGuestClass**, **SwiftGPUProfile**, **SwiftGPUNode**, **SwiftGuestPool** — unchanged from v0.1.0+ context (see prior revisions). SwiftImage gained `cloneStrategy: copy|snapshot` and `status.cloneSeed` in snapshot Phase 1.

**SwiftSnapshot** — VM snapshot (Phase 1/2)
```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
spec:
  source:
    swiftGuestRef: {name: my-guest}
  backend:
    type: csi-volume-snapshot | local
    csiVolumeSnapshot:
      volumeSnapshotClassName: longhorn-snapshot
    local:
      hostPath: /var/lib/kubeswift/snapshots/
  includeMemory: false              # Tier B / local backend only
  deletionPolicy: Delete | Retain
status:
  phase: Pending | Capturing | Ready | Failed
  observedPauseWindowMs: 4085       # for memory snapshots
  hypervisorVersion: "cloud-hypervisor v51.1"
```

**SwiftRestore** — snapshot restore operation (Phase 1/2)
```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
spec:
  source:
    swiftSnapshotRef: {name: my-snapshot}
  targetGuest:
    name: my-guest                  # same as source = in-place; different = clone
    overwriteExisting: true         # required for in-place
  identity:
    regenerate: [hostname, machineId, sshHostKeys, macAddresses]
status:
  phase: Pending | Restoring | Resuming | Ready | Failed
```

**SwiftMigration** — VM migration operation (Phase 1 of live migration)
```yaml
apiVersion: migration.kubeswift.io/v1alpha1
spec:
  source:
    swiftGuestRef: {name: my-guest}
  target:
    nodeName: target-node           # exclusive with nodeSelector (Phase 4+)
  mode: auto | live | offline       # Phase 1: auto picks offline; live is webhook-rejected
  allowIPChange: true               # required for cross-node on default networking
  timeout: 1h
  reason: "node maintenance"
status:
  phase: Pending | Validating | Preparing | StopAndCopy | Resuming | Completed | Failed | Cancelled
  mode: offline                     # actual mode picked
  conditions: [Ready, Compatible, IPWillChange]
  observedDowntime: 42.413s
  phaseDetail: "waiting for VolumeAttachment GC"
```

### Repository Structure
```
api/
  image/v1alpha1/, kernel/v1alpha1/, seed/v1alpha1/, swift/v1alpha1/, gpu/v1alpha1/
  snapshot/v1alpha1/                NEW — SwiftSnapshot, SwiftRestore (Phase 1/2)
  migration/v1alpha1/                NEW — SwiftMigration (Phase 1 of live migration)
cmd/
  swiftctl/, controller-manager/, webhook-server/, gpu-discovery/
  snapshot-stager/                   NEW — clone restore staging init container binary
internal/
  controller/swiftguest/, swiftimage/, swiftkernel/, swiftgpu/, swiftguestpool/
  controller/swiftsnapshot/          NEW (Phase 1/2)
  controller/swiftrestore/           NEW (Phase 1/2)
  controller/swiftmigration/         NEW (Phase 1 of live migration)
  webhook/swiftmigration/            NEW (per-operation discipline)
  snapshot/configjson/               NEW — config.json patcher for clone restore
  runtimeintent/, resolved/, seed/, scheme/
rust/
  swiftletd/, swift-runtime/
  swift-ch-client/                   extended with snapshot/restore actions
  swift-qemu-client/, swift-seed/
images/
  gpu-discovery/, swiftletd/         (swiftletd now includes qemu-system-x86_64 + OVMF)
  snapshot-stager/                   NEW
config/
  crd/bases/                         (now includes snapshot.kubeswift.io and migration.kubeswift.io)
  samples/                           (incl. snapshots, snapshots-walkthrough, local-snapshots, migration)
test/
  smoke/, gpu/
  snapshot/                          NEW — round-trip, clone-identity tests
  migration/                         NEW — cross-node round-trip + VFIO rejection
docs/
  design/                            (snapshots.md, live-migration.md, gitops-flux.md, *spike-results*.md)
  snapshots/                         (incl. operator-walkthrough.md, walkthrough-findings.md)
  migration/                         NEW (overview, offline-migration, networking-requirements, troubleshooting)
  networking/                        (operations-guide, ovn-kubernetes, sriov)
```

### Networking Model

(Unchanged from v0.1.0+ — tap+bridge+dnsmasq for primary NIC, Multus for secondary, SR-IOV via VFIO. eth0 is NOT enslaved to br0; br0 has its own IP as gateway for guests.)

### Cloud Hypervisor Invocations

(Disk boot, kernel boot, GPU boot unchanged. New restore mode for Phase 2 snapshots:)

```
cloud-hypervisor \
  --api-socket path=<runtime-dir>/ch.sock \
  --restore source_url=file://<snapshot-path> \
  --serial socket=<runtime-dir>/serial.sock \
  --console off
```

After `--restore`, the controller-driven `vm.resume` action via the annotation surface transitions the VM to running.

### Status Reporting Architecture

| Annotation | Set by | Maps to |
|---|---|---|
| `kubeswift.io/guest-ip` | lease.rs | status.network.primaryIP |
| `kubeswift.io/guest-interfaces` | lease.rs | status.network.interfaces[] |
| `kubeswift.io/guest-runtime-pid` | report.rs | status.runtime.pid |
| `kubeswift.io/guest-serial-socket` | report.rs | status.console.serialSocket |
| `kubeswift.io/guest-hypervisor` | report.rs | status.runtime.hypervisor |
| `kubeswift.io/gpu-devices` | report.rs | status.gpu.devices |
| `kubeswift.io/snapshot-action` | controller (snapshot/restore) | swiftletd action loop trigger |
| `kubeswift.io/snapshot-action-id-mirror` | swiftletd | controller observes action completion |
| `kubeswift.io/migration-in-progress` | SwiftMigration controller | idempotency source-of-truth for re-entrant reconciles |
| `snapshot.kubeswift.io/active-restore` | SwiftRestore controller | swiftletd starts in restore-receive mode |

---

## Snapshots Phases 0/1/2 — SHIPPED

### Phase 0 (Spike) — completed
Validated CH pause/snapshot/resume on real cluster (~2.8s/GiB pause window on Longhorn-backed disk). Five findings reconciled into design doc:
1. CH v51.1 SUCCEEDS at snapshot of VFIO VM but RESTORE fails — design Constraint #1 corrected: VFIO blocks at restore, not snapshot
2. Longhorn does full-copy not CoW (~10s/GiB background copy)
3. Longhorn refuses larger-target clones — `clone-grow-init` init container
4. Cross-namespace dataSourceRef silently provisions empty PVC on k0s 1.34 — same-namespace constraint enforced
5. Finalizer load-bearing for CoW drivers, defensive for full-copy

### Phase 1 (CSI VolumeSnapshot disk-only) — SHIPPED
SwiftSnapshot + SwiftRestore CRDs (csi-volume-snapshot backend), SwiftImage cloneStrategy: copy|snapshot for fast pool scaling, validation webhooks, swiftctl snapshot/restore subcommands, e2e tests, ≥3× pool scaling speedup.

### Phase 2 (Tier B local memory snapshots) — SHIPPED
Memory snapshot capture and restore via Cloud Hypervisor pause/snapshot/resume. Tier B local hostPath backend at `/var/lib/kubeswift/snapshots/`. swiftletd action loop handles pause/snapshot/resume via CH HTTP API with sentinel-guarded destination wipe-and-recreate. snapshot-stager init container for clone restores.

**In-place restore validated**: tmpfs sentinel survives kill+restore cycle.
**Clone restore validated**: both clones reach Ready with deterministic per-clone hypervisor MAC, unique runtime_dir paths, deterministic seed.iso rebuild.

#### Known limitation: identity regeneration on clone resume

CH `--restore` resumes captured guest state byte-for-byte — **this is not a fresh boot**. Cloud-init does not re-run. As a result:

| Identity field | After clone resume |
|---|---|
| /etc/machine-id | Inherited from source |
| /etc/ssh/ssh_host_*_key* | Inherited from source |
| hostname | Inherited from source |
| Guest-visible eth0 MAC | Inherited from source (cached in virtio-net driver state) |
| Hypervisor config.net[0].mac | Rewritten per clone (visible to bridge fdb, but not to guest) |
| Pod network namespace | Per-clone (Kubernetes-isolated, prevents cross-clone L2 collision) |

Operators must either reboot each clone after first resume (cloud-init bootcmd then fires normally) or manually regenerate identity. Identity regen without reboot is targeted for a future phase via in-guest agent over vsock.

#### Snapshot bug-fix history (Phases 0-2 implementation)

| # | Component | Bug | PR |
|---|-----------|-----|-----|
| 11 | swiftsnapshot/local.go | Cleanup destination handling | #11 |
| 12 | swiftletd/action.rs | mkdir on snapshot directory | #12 |
| 13 | swiftletd/action.rs | Mount handling for snapshot dir | #13 |
| 14 | swiftsnapshot/local.go | action-id changed across status patches | #14 |
| 15 | swiftguest pod builder + swiftletd/main.rs | seed.iso missing on restore-receive | #15 |
| 16 | configjson + stager | Patcher targeted wrong layout | #16 |
| 17 | configjson | appendCloneMarker crashed on cmdline: null | #17 |
| 18 | swiftrestore/local.go | TargetConflict against own freshly-created SwiftGuest | #18 |

---

## Snapshot Operator Walkthrough — COMPLETED via 3 PRs

After Phases 0/1/2 shipped, performed an operator-perspective validation exercise.

### PR #21 — Tier A data-loss fix (silent bug since SwiftRestore was first added)
`EnsureRootDiskClone` in `internal/controller/swiftguest/rootdisk.go` checked `IsControlledBy` BEFORE `RestoreSeededLabel`, deleting SwiftRestore-seeded PVCs as orphans. Tier A restore silently produced fresh boot from SwiftImage instead of restoring snapshotted disk content. Bug existed since commit 4e055a6.

**Operators following csi-snapshots.md would have unrecoverable data loss** in real disaster recovery scenarios. Three-line reorder fix plus regression test.

### PR #22 — CI wiring + e2e audit (closing systemic gap)
The Phase 1 e2e test for snapshot restore WOULD have caught the Tier A bug — it explicitly asserts restore-seeded=true label and dataSource.kind=VolumeSnapshot. But CI never ran it. CI ran only `go test ./...` and `cargo test`.

PR #22 added: verify-e2e-scripts per PR, e2e-on-cluster.yaml workflow (path-touch trigger), Make targets for every script, audit of every e2e file's CI coverage status.

### PR #23 — Operator walkthrough doc + 8-scenario findings + in-place fixes
Eight scenarios exercised: disk-only snapshot/restore (Tier A), SwiftImage with cloneStrategy=snapshot, SwiftGuestPool scaling, pool rolling update, memory snapshot in-place restore, memory snapshot clone restore, pool templated from memory snapshot (gap documented), failure modes audit.

9 findings categorized:
- F1 (silent data-loss in Tier A) — fixed in PR #21
- F2-F4 — fixed in PR #23 in-place
- F5-F8 — follow-up tracked
- F9 (latent bug) — separate triage

Sample manifests in `config/samples/snapshots-walkthrough/`. Findings inventory in `docs/snapshots/walkthrough-findings.md`. Tutorial doc in `docs/snapshots/operator-walkthrough.md`.

#### Most operationally significant findings
- **F1**: Tier A silently producing fresh boots (caught and fixed)
- **F7**: cloneStrategy: snapshot slower than copy at single-guest scale on Longhorn with significant resize delta — TRACKED FOLLOW-UP
- **F2**: RBAC RoleBinding subject namespace must be patched after `kubectl apply -k config/rbac -n <ns>` (smoke test does this; operator docs didn't mention it)
- **Scenario 6**: confirmed empirically that all four guest-observable identity signals collide between source and clone (resume-vs-boot)

The pattern: "e2e exists, never runs in CI, bugs accumulate that the e2e would catch" — PR #22's nightly cluster-e2e workflow exists to break this.

---

## Live Migration Phase 1 — SHIPPED via PR #24, #25, #26

Phase 1 ships SwiftMigration CRD and controller for **offline migration only**. Independently shippable; immediate value for safe VM movement between Kubernetes nodes — especially for VFIO/SR-IOV workloads that cannot live-migrate.

### Three baked-in design decisions
1. **Storage path**: direct PVC reuse (Approach A from spike) — stop source SwiftGuest fully, recreate target SwiftGuest pinned to target node acquiring same PVC. NOT snapshot+restore.
2. **Drain integration deferred to Phase 4**: Phase 1 ships SwiftMigration CRD + controller only; no eviction webhook
3. **Sub-agent engagement**: matches snapshot prompts (architect at start, qa for tests, tech-writer for docs, security for RBAC)

### Phase 1 spike findings (`docs/design/live-migration-phase-1-spike.md`)

**Q1 — Cross-node PVC reuse on Longhorn (RWO): PASS.** ~70s end-to-end downtime (32s pod-gone + 13s detach + 25s boot). Sentinel survived intact, machine-id stable.

**Q2 — Schedulability check: manual capacity check.** Server dry-run is useless (skips scheduler entirely). Real-pod-probe leaves debris. Manual check (read node.status.allocatable, list pods, sum requests, compute headroom) is sub-second and zero-side-effect.

**D3 — PVC ownerRef: Approach A confirmed.** SwiftGuest CR identity preserved across migration (same UID throughout). PVC ownerRef stays valid. No migration-seeded label needed.

**Two new findings shaped Phase 1 implementation:**
- SwiftGuest needed `spec.nodeName` field (disk-boot pods were unpinned)
- Preparing phase must explicitly `Delete(pod)`, NOT just patch `runPolicy: Stopped` (stop guard is reactive only — left pod running 164s+ in spike)

### PR #24 — SwiftMigration CRD and controller (initial implementation)

49 files changed, +7107/-23 lines. New API group migration.kubeswift.io/v1alpha1. State machine: Pending → Validating → Preparing → StopAndCopy → Resuming → Completed/Failed/Cancelled. SwiftGuest extensions (spec.migration block, spec.nodeName field). Pod builder uses direct pod.Spec.NodeName binding.

Validation webhook with eight rejection rules + three input bounds. 74 tests across three packages. Sub-agent gates cleared.

**Headline validation post-merge**: Migration miles→boba. Sentinel md5 cd28575af1c5c8c438c3b00f9c18add0 matched pre/post-migration. observedDowntime=42.413s matches spike. VFIO rejection fired correctly.

### PR #25 — Fix terminal-state handling (Bug A + Bug B)

**Bug A (HIGH)**: Stuck finalizer when source SwiftGuest deleted before SwiftMigration. removeFinalizer patch hit validation webhook running on UPDATE; source-not-found rejection prevented finalizer removal. Operator workaround required: recreate stub SwiftGuest with same name. Not production-acceptable.

**Bug B (MEDIUM)**: Reconcile loop on terminal-phase SwiftMigrations. Completed SwiftMigrations kept reconciling, attempts at UPDATE re-ran validation against current cluster state and failed. Retry storms with growing backoff every minute.

7 new tests including hardened patchCountingClient. PR description flagged "treat terminal states as terminal" pattern.

### PR #26 — Subsume A/B/C under per-operation discipline (architectural refactor)

While #25 was in flight, third bug surfaced: ensureFinalizer rejection mid-flight when source guest deleted (same family as Bug A). Rather than ship three per-bug guards, refactored webhook to per-operation discipline:
- **ValidateCreate**: full validation (shape rules + cluster-state rules)
- **ValidateUpdate**: shape-only (no cluster-state checks since spec is immutable)
- **ValidateDelete**: pass-through (no validation)

Test renaming from "Bug A/B/C" to `_NoClusterState` (broader rule). Controller test `TestReconcile_InFlight_GuestDisappeared_DrivesToFailed` proves end-to-end coverage.

**Pattern flagged for future phases (PR #26 description)**:
> Validation logic that fires on every operation needs to consider whether each operation is one where validation adds value vs. blocks legitimate work. Bug A: webhook designed for CREATE/UPDATE applied to DELETE without intent. Bug B: controller designed for active transitions reconciled terminal states without explicit exclusion. Phase 2/3/4 implementations should explicitly enumerate which operations validation fires on, and document the rationale for each. Default-to-everything is the bug pattern; default-to-explicit is the discipline.

### Live Migration Phase 1 architecture decisions captured in code

| Decision | Origin | Where |
|---|---|---|
| Direct PVC reuse (Approach A), not snapshot+restore | Spike Q1 | StopAndCopy phase + design-doc D1 correction |
| Single combined client.MergeFrom for runPolicy + nodeName | Architect Q1 | stopandcopy.go |
| runPolicy=Stopped patch BEFORE Delete(pod) (combined with annotation) | Architect Q3 | preparing.go |
| Dual-poll: Pod NotFound + no VolumeAttachment for the per-guest PV | Architect Q3 + spike timing | preparing.go isPVCStillAttached |
| Annotation-as-source-of-truth idempotency marker | Architect Risk 3 | kubeswift.io/migration-in-progress |
| Drive-forward post-cutover, restore source pre-cutover | Architect Risk 2 | failure.go cleanupSourceGuest |
| Manual capacity check, not server dry-run / real-pod-probe | Spike Q2 | validating.go checkNodeCapacity |
| GPU cross-node migration unconditional rejection in Phase 1 | Architect + security | Webhook + pod-builder |
| Direct pod.Spec.NodeName binding, not selector | Architect Q2 | pod.go applyNodeName |
| Operator-opt-in for IP change via spec.allowIPChange | Architect Q4 + spike Q1a | Webhook + Validating phase |
| Per-operation validation discipline | PR #26 architectural refactor | webhook/swiftmigration/validator.go |

### Live Migration Phase 1 performance baseline

| Sub-step | Longhorn (RWO full-copy) | Rook Ceph RBD / EBS (RWO CoW) |
|---|---|---|
| Validating | <1s | <1s |
| Preparing: pod gone | ~32s | ~32s |
| Preparing: VolumeAttachment GC'd | +13s | <1s |
| StopAndCopy: spec patch + scheduling | <2s | <2s |
| StopAndCopy: PV reattach on target | ~5s | <1s |
| Resuming: VM cold-boot | ~17s | ~17s |
| **Total observable downtime** | **~70s** | **~25s** |

### Live Migration Phase 1 operator-immediate value

Two workload classes get full value day-one and forever:
1. **VFIO/SR-IOV workloads** (Tier 1/2/3 GPU, SR-IOV NIC passthrough) — these can NEVER live-migrate due to upstream Cloud Hypervisor constraint #2251. Offline migration is the only migration mode they will ever have.
2. **Non-VFIO workloads where tens-of-seconds downtime is acceptable** — most operator-initiated rebalancing, manual maintenance, hardware refreshes.

Phase 1 is also the foundation Phases 2–5 build on.

---

## Tracked Follow-ups

### 1. Network architecture requirements design doc (deferred from live migration Phase 1 conversation)

When Phase 2 of live migration ships (or sooner if needed), produce:
- Promotion of Constraint 6 in `docs/design/live-migration.md` to a proper "Network Architecture Requirements" section
- New design doc at `docs/design/network-architecture-requirements.md` capturing the broader framework

Framework should establish:
- Node-local vs multi-node networking choice
- Capabilities requiring multi-node L2: live migration with IP preservation, offline migration with IP preservation, multi-tenancy with cross-node isolation, telco/NFV, stateful services with external clients
- Three multi-node L2 options:
  - Multus + macvlan/bridge on shared physical NIC
  - OVN-Kubernetes layer-2 secondary network
  - OVN-Kubernetes user-defined networks (UDN)
- Cross-references from existing per-feature networking docs

This affects future Phase 3 of live migration (network requirements), Phase 4 (drain integration assumptions), any future multi-tenancy work, and operator deployment planning.

### 2. Operator-flow validation pattern in test infrastructure

Three data points suggest testing strategy has a gap:
- Snapshot Phase 1 Tier A bug (PR #21) — silent data loss undetected by all unit tests
- Live migration Phase 1 finalizer bug (PR #25 Bug A) — surfaced in 30-min headline validation
- Live migration Phase 1 reconcile-loop bug (PR #25 Bug B) — surfaced in same validation

Worth structural treatment after live migration roadmap settles. Question: should KubeSwift bake operator-flow validation into test infrastructure rather than relying on post-hoc walkthroughs?

### 3. Snapshot walkthrough finding F7

`cloneStrategy: snapshot` slower than `copy` at single-guest scale on Longhorn with significant resize delta. Three categories of explanation possible:
1. Benchmark methodology issue (snapshot timing includes SwiftImage snapshot creation that copy doesn't pay)
2. Resize delta dominates at small image sizes
3. Implementation issue

Walkthrough findings doc has the timing data needed. ~15 min of analysis to distinguish. If 1 or 2: docs update. If 3: real perf bug.

### 4. Mini-walkthrough between phases vs batched at end

Pattern decision validated: do mini-walkthroughs between phases for live migration. Headline validation post-Phase-1 caught Bugs A+B+C in 30-60 min.

### 5. Source-PVC-deletion behavior

What happens when SwiftImage's source PVC is deleted while a snapshot of it has bound clones. Deferred from Phase 0 spike for cluster safety. Phase 2 e2e covered some of this but not all. Should validate before Phase 3.

### 6. Storage RWX+Block runtime path (W9 — SHIPPED via PR #35)

W9 shipped via PR #35 (2026-04-30). Cluster mini-walkthrough validated
the end-to-end Block runtime path on Longhorn Migratable storage
(`parameters.migratable: "true"`). RWX+Block SwiftGuest boots, cloud-init
growpart + resize2fs succeed (`df` reports ~37G of 40G), Block PVC
persists across pod recreation, RWO+Filesystem regression unaffected.
See "PR #35 walkthrough findings" subsection above.

Two follow-ups from the walkthrough:

- **W10 — CH `Request check failed: ... ReadOnly` WARN at sector 0 during
  early boot.** Two warnings during boot, then never again; growpart
  succeeds. Non-blocking; investigate as a CH v51.1 quirk in the early-
  boot virtio-blk request validation path. Document for operators.
- **W11 / W9.x — `cloneStrategy=snapshot` + `volumeMode: Block` fails at
  PVC provisioning.** CSI external-snapshotter requires the
  `snapshot.storage.kubernetes.io/allow-volume-mode-change: "true"`
  annotation on the source VolumeSnapshotContent when destination
  volumeMode differs from source. The SwiftImage controller's
  cloneSeed-creation path needs to set this annotation. Small fix;
  separate follow-up issue per W9 prompt's binding language.

For historical reference (the original W9 scoping doc still describes
the gap as it existed before PR #35):

Three components need updates, plus three open scoping questions the
walkthrough did not exercise. Detail in
[`docs/design/storage-rwx-block-runtime.md`](docs/design/storage-rwx-block-runtime.md).

Components:
- Copy Job (`internal/controller/swiftguest/rootdisk.go::createCloneJob`):
  use `volumeDevices` + raw-device write (`dd`/`qemu-img convert` to
  `/dev/dst-block`) for Block destinations
- Launcher pod builder
  (`internal/controller/swiftguest/pod.go`): switch root-disk volume to
  `volumeDevices` for Block destinations; pass the device path via
  RuntimeIntent
- swiftletd (Rust): accept block device path in
  `runtimeintent::DiskIntent` and pass as `--disk path=/dev/...` to
  Cloud Hypervisor

Open scoping questions (must be answered as part of W9, not blockers):
(a) does the qcow2→raw SwiftImage import pipeline work against
Block-mode destination PVCs (`qemu-img convert -O raw` to `/dev/...`
inside the import Job)?
(b) does cloud-init `growpart` work on a Block-mode root disk inside the
guest, or does the Block path produce a partition that cloud-init's
filesystem-resize step can't extend?
(c) does the existing `qemu-img resize` + `sgdisk -e` GPT-fix step work
against Block targets, or does Block change the resize/sgdisk semantics
(qemu-img resize on a block device is typically a no-op since size is
fixed at provision time; sgdisk -e operates byte-level so should still
work)?

This is not a PR #32 regression. Same shape as Phase 1 → Phase 2 → PR
#32: each shipped layer reveals what the next needs. The W5 pattern
(spike scenarios under-constrain the design) restated yet again — this
time at the API/runtime boundary.

### 7. Phase 3a downtime-metrics observability — SHIPPED via W27 follow-up PR

**Status: SHIPPED + cluster-validated.** W27a + W27b both fixed in
PR #55 (merged). PR #56 documents empirical cluster validation; PR
#56 commit D additionally clarifies field semantics in CRD docstrings.

`status.observedDowntime` now anchors on a new
`status.cutoverStep2DispatchedAt` timestamp (stamped at src pod
Delete dispatch); for live mode this measures the operator-visible
cluster downtime window (cutover dispatch → GuestRunning=True), not
sub-millisecond reconcile-clock noise. `status.observedPauseWindow`
is read from the swiftletd-written
`kubeswift.io/migration-pause-window-ms` annotation at
`substateSrcCompleted` — measures the wall-clock elapsed of the
entire `vm.send-migration` RPC (pre-copy iterations + final
stop-and-copy + finalize), NOT the vCPU stop-the-world sub-phase
(buried inside CH internals).

Empirical cluster baseline (image `sha-b730536`, default node-local
networking, 4Gi guests):
- Kernel-boot: observedDowntime=1.75s, observedPauseWindow=38.17s
- Disk-boot RWX+Block: observedDowntime=1.96s, observedPauseWindow=38.19s

### W28 candidate (split from W27): capture actual vCPU stop-the-world

Currently `observedPauseWindow` measures the entire send-migration
RPC, dominated by pre-copy iterations where the vCPU is still
running. The actual vCPU stop-the-world (CH's final stop-and-copy
sub-phase, typically hundreds of milliseconds for the empirical
4Gi-guest cases above) is the operator-relevant "guest frozen"
metric and is not separately surfaced today.

Three plausible paths to capture it:

1. **Future CH versions may grow per-phase timing** on the
   `vm.send-migration` HTTP response body (CH per-phase telemetry
   is on the upstream roadmap; tracked there, not here).
2. **`swift-ch-client` could probe `vm.info` around the stop-and-copy
   boundary** inside the send-migration RPC — requires interleaving
   reads with the synchronous send call, which itself is constrained
   by the W12 swift-ch-client async refactor (Phase 3b prerequisite).
3. **External observer via Tracked Follow-up #1** (multi-node L2
   enablement: Multus + macvlan / OVN-K layer-2 / UDN) — ping the
   guest from a third-node sibling pod with 50ms intervals and count
   consecutive lost pings × 50ms. This is the cleanest empirical
   measurement but blocked on the cross-node L2 prerequisite.

W28 is non-blocking for Phase 3a or Phase 3b shipping. Phase 3b
design conversation can begin without W28 resolved; W28 lands
opportunistically when one of the three paths becomes feasible.

### W27 cluster validation operational finding

First validation run hit a **stale-CRD-silent-strip failure mode**:
cluster CRD didn't have the new `cutoverStep2DispatchedAt` field;
apiserver silently stripped it from controller status patches; field
appeared empty in cluster despite the controller image having the
W27a stamp code. Documented in `docs/migration/phase-3a.md` operational
note. Pattern applies to any new status field across releases —
operators upgrading via custom pipelines must `kubectl apply -f
config/crd/bases/...` (or use `make deploy` / `helm upgrade` which
already do this); detection is `kubectl explain swiftmigration.status`
after upgrade.

Original W27 audit notes preserved below for context (the diagnosis
that drove the fix).

E12 cluster validation surfaced **two metrics-observability bugs** in
Phase 3a's `status.observedDowntime` and `status.observedPauseWindow`
fields. Correctness of migrations is unaffected — these are
operator-visible metrics surfaces only — but both are paper-shipped
and undeliverable as documented. Disposition: **W27 follow-up PR**
after PR 2 (swiftctl resolveLauncherPodName) and PR #54 walkthrough
log update merge. **Not a Phase 3b prerequisite** — Phase 3b design
conversation can begin in parallel; W27 lands as a small hotfix.

**W27a — observedDowntime measurement broken (sub-millisecond values
across all walkthroughs).**

Code site:
[`internal/controller/swiftmigration/resuming_live.go:130-198`](internal/controller/swiftmigration/resuming_live.go).
`ResumingStartedAt` is stamped at line 131 on the first reconcile that
enters Resuming phase; `now := metav1.Now()` at line 194 fires later
in the SAME reconcile invocation when the completion path fires
(GuestRunning=True observed without an intervening apiserver event
because the dst-side condition write may have already landed before
this reconcile started). The two `metav1.Now()` calls are
microseconds apart on real clusters, producing 34–114µs durations
across all 9 PR #46 + 8 E12 walkthrough runs. The existing comment at
lines 186–193 explicitly says "B3 may refine to a step-2 anchor if it
stamps a separate timestamp; until then this is the best
approximation" — design knew this was wrong, deferred without
tracking.

Fix shape: stamp `status.CutoverStep2DispatchedAt` in
[`cutover.go::cutoverStep2`](internal/controller/swiftmigration/cutover.go)
when the src pod Delete dispatches; compute
`observedDowntime = guestRunningObservedAt - cutoverStep2DispatchedAt`
at Completed. Wire-anchor: cutoverStep2 is the actual cutover commit
point (vCPU pause begins on src CH at this moment); GuestRunning=True
on the SwiftGuest is the actual resume signal (vCPU pause ends on dst
CH). The window between these is the operator-visible downtime — the
~38–48s pre-copy + cutover total observed in cluster logs.

W27a does not affect migration correctness. Affects: operators
reading `kubectl get smig -o wide` for downtime, dashboards consuming
the field, capacity-planning analyses. The CRD field exists with
correct documentation; the implementation under-delivers.

**W27b — observedPauseWindow plumbing half-implemented.**

Code surface inventory:

| Side | Status |
|---|---|
| swiftletd writes `kubeswift.io/migration-pause-window-ms` annotation on src pod via `write_migration_status` ([`rust/swiftletd/src/action.rs:1383-1407`](rust/swiftletd/src/action.rs)) | ✓ implemented |
| swiftletd computes `pause_window_ms = elapsed_ms` at `dispatch_migration_send_complete` (line 701) and `dispatch_migration_send_complete` clean-exit (line 809) | ✓ implemented |
| Go controller: ANY read of `kubeswift.io/migration-pause-window-ms` from src pod annotations | **✗ ZERO consumers** |
| Go controller: ANY assignment to `status.ObservedPauseWindow` field | **✗ ZERO assignments** |

Comment at
[`resuming_live.go:75–79`](internal/controller/swiftmigration/resuming_live.go)
+ `:199–203` says "ObservedPauseWindow is populated by B3 from src
pod's migration-status-detail annotation during StopAndCopy. B2.3
leaves it as-is — preserves whatever B3 wrote, leaves nil if B3
hasn't run yet (forward-compat hedge per the prompt's rule 5)." B3
landed but never implemented the read-and-stamp path. Field is
permanently nil in cluster.

By contrast: `internal/controller/swiftsnapshot/local.go:249-251`
DOES read the snapshot-pause-window-ms annotation and stamp
`status.ObservedPauseWindowMs`. That's the parallel pattern Phase 3a
needs; it just wasn't ported.

Fix shape: in stopandcopy_live.go (or the cutover entry point), read
the src pod's `kubeswift.io/migration-pause-window-ms` annotation
when present and stamp `status.ObservedPauseWindow` (parsing
milliseconds → metav1.Duration). Mirror the snapshot pattern at
[`local.go:249-252`](internal/controller/swiftsnapshot/local.go). ~10
LOC + a unit test.

W27a + W27b together: a single follow-up PR cleanly addresses both.
W27a fixes the dispatch-to-resume wall-clock window; W27b fixes the
swiftletd-reported vCPU-pause-only window. Both fields then carry
their documented semantics.

**Note on shipping qualification** (operator's framing): "Phase 3a
shipped: correctness validated; downtime metrics observability
deferred to W27 follow-up" is the honest version of the shipping
claim. PR #54 description updated to state this explicitly.

### 8. swiftctl debug /proc scan misses CH process (W2-1 — surfaced by PR 2 walkthrough 2026-05-05)

**Symptom**: `swiftctl debug <guest>` output includes
`--- Cloud Hypervisor command line (from /proc) --- No
cloud-hypervisor process found (check launcher logs for spawn
args)`, while `swiftctl describe` simultaneously reports
`Runtime: Hypervisor: cloud-hypervisor PID: <pid>`. The two
diagnostics contradict each other on the same guest in the same
moment.

**Hypothesis**: swiftletd's `/proc` scan inside the launcher
container appears to miss the CH process despite swiftletd
successfully reporting the PID via pod annotations. Likely a /proc
visibility issue from inside the launcher container — PID
namespace, /proc mount, or shell loop boundary in
`internal/cli/debug.go`. Pre-existing in swiftctl debug, not a PR 2
regression.

**Severity: LOW** (diagnostic contradiction, not operational
failure). Disposition: triage when an operator hits it OR alongside
other swiftctl polish work. ~10-30 min investigation; fix surface
likely small (one shell command in debug.go).

### 9. offline→delete→live migration sequence has a settling window (W2-2 — surfaced by PR 2 walkthrough 2026-05-05)

**Symptom**: chaining `swiftctl migrate <guest> --to <node>` (mode
defaults to offline) → `kubectl delete swiftmigration <name>` → apply
a live-mode SwiftMigration in <5s sequence, the live migration's
Validating-time `status.SourcePodUID` capture races against the
offline migration's `cleanupSourceGuest` pod-recreate cycle (the
offline cancellation path restored runPolicy=Running, the SwiftGuest
controller created a new pod, the live mig captured podRef.UID
mid-recreate, and Preparing-live's UID-check observed mismatch
against the steady-state pod). Live mig fast-fails t+3s with
`failureReason=SourcePodReplaced`.

Steady-state retry ~30s later (once the new pod settles back to
Running) succeeds cleanly. Pre-existing race; not a controller bug
— SourcePodUID lock is W26's by-design behavior, functioning
correctly. The race is between the offline mig's cleanup path and
the live mig's validation, not between two live migs.

**Severity: LOW** (user-induced; rare in practice — operators don't
typically chain offline→delete→live in the same second).
Disposition: document a "rapid mode-switch caveat" in
`docs/migration/phase-3a.md` operator runbook IF operators raise it
empirically; otherwise leave at tracked-follow-up status. No code
change needed.

### 10. CPU-feature mismatch (Phase 3b spike Q3 reframe)

Phase 2 spike's F12 noted that CPU-feature mismatch (heterogeneous
microarchs across nodes) is the realistic production failure mode
for live migration, not version skew. Phase 3b spike Q3 confirmed
this is **NOT addressed in Phase 3b** — spike cluster is hardware-
uniform; testing requires deliberate CPU-flag masking and the
mitigation is procedural anyway.

**Mitigation today (Phase 3b ships without code changes for this):**
operator runbook discipline. Verify `lscpu` flag uniformity across
nodes that participate in live migration. CH v51.1 uses `host` CPU
model by default (passes through host flags as-is); migration
succeeds when source and destination expose identical flag sets and
fails at guest-feature-load time when they don't.

**Tracked for Phase 3c or operational-polish phase**: add a
`swiftctl migrate --check <guest> --to <node>` pre-flight that
compares source and destination node CPU flags before issuing the
SwiftMigration. Surface mismatches as a warning, not a hard
rejection (operator may have a reason to proceed). Mirrors Phase
1's target-node-Ready-check ergonomic pattern.

### 11. observedPauseWindow CRD field-name decision (Phase 3b spike Q2 leftover)

The current name `observedPauseWindow` suggests "vCPU pause
window" — the duration the guest is frozen. W27 (commit D, PR #56)
clarified the field semantics in CRD docstrings: it actually
measures the *full* `vm.send-migration` RPC duration (pre-copy
iterations + final stop-and-copy + finalize), most of which is
NOT vCPU-paused.

The Phase 3b spike Q2 results made the naming gap concrete: a
baseline migration shows `pauseWindow=38.20s` while the guest
stayed responsive throughout — no operator looking at
"pauseWindow=38s" intuits "guest was responsive for most of those
38s; only the last sub-second was actual vCPU-pause."

**Phase 3b design conversation should consider renaming** to
`transferDuration` or `migrationRpcDuration` — and reserve
`observedPauseWindow` for the actual vCPU-stop-the-world metric
when W28 lands. **Decision deferred to Phase 3b design
conversation**. CRD field renames are breaking changes; the
cost/benefit needs explicit weighing against the operator-clarity
benefit. Doing it before Phase 3b GA is cheaper than after.

### 12. `newDstPod` clone-src behavior is LOAD-BEARING for version discipline (Phase 3b spike Q3 lesson)

Phase 3a's
[`internal/controller/swiftmigration/dst_pod.go::newDstPod`](internal/controller/swiftmigration/dst_pod.go)
constructs the destination pod by `srcPod.DeepCopy()` of the source
pod's spec. This implementation choice provides a load-bearing
property the original Phase 2 design did not acknowledge:
**version-skew is structurally prevented at the controller layer**
because dst image is atomically inherited from src. There is no
controller code path that produces heterogeneous src/dst.

**Future-proof by stating the constraint explicitly in the Phase
3b design doc**: a future refactor that re-resolves the dst pod
from SwiftGuest spec (which would sound cleaner architecturally —
"single source of truth, no clone-and-mutate") would silently
re-introduce the version-skew surface that Phase 2 Decision 3 was
originally meant to address.

Same structural pattern as Phase 3a's W26 lesson: an apparently
clean refactor regresses a load-bearing property because the
property was never explicitly named. The newDstPod docstring
(lines 108-130) describes the clone-src approach in code; Phase 3b
design doc must state explicitly that version-skew-prevention is
one of the properties the clone-src approach delivers, so future
maintainers see the constraint before refactoring. Don't rely on
commit archaeology.

**Disposition:** documentation only — no code change. Surfaces in
Phase 3b design doc; cross-reference from `newDstPod` docstring if
language is added there too.

---

## Phase 2 Decisions Resolved (live migration)

Phase 2 spike completed 2026-04-29. Findings doc: `docs/design/live-migration-phase-2-spike.md`. All four pending decisions resolved with empirical evidence on the deployed cluster (miles + boba, CH v51.1).

1. **swiftletd control surface — RESOLVED: annotation-driven**, mirroring snapshot Phase 2's `kubeswift.io/<resource>-action-id-mirror` pattern. 8-action set: prepare-destination (pod-level), start-receive, start-send, report-progress, report-complete, report-failed, cancel (= dst-kill, NOT a CH API call), wait-keepalive. Annotation churn ~8 patches per ~30 s migration — trivially within surface throughput. No action requires synchronous request/response. Spike doc Resolved Decision 1 + Q3.

2. **mTLS posture — RESOLVED: plaintext TCP for Phase 2 with security-gating**. Phase 2 manual demonstration only; mTLS lands in Phase 3. **Required gates before Phase 2 ships**: `docs/design/THREAT-MODEL.md` callout + `kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation gate on swiftletd action acceptance. Phase 3 mTLS handoff must compose with the S1 annotation-trust-boundary mitigation (URLs from SwiftMigration CR, NOT pod annotations) — neither subsumes the other. Spike doc Resolved Decision 2 + S2.

3. **Same-CH-version constraint — RESOLVED: bidirectional v50/v51 minor compatibility**, but Phase 2 spec defaults to **exact-image-tag match** with `spec.allowVersionSkew=true` opt-in escape hatch (analogous to Phase 1's `spec.allowIPChange`). Detection at the Kubernetes layer (controller-level image-tag comparison), NOT at the CH wire level (no CH-level capability handshake exists). The realistic production failure mode is **CPU-feature mismatch** (heterogeneous microarchs), not version mismatch — F12 in spike doc. Q4b (full sweep across major versions, patch-only skew) descoped per architect time-cap. Spike doc Resolved Decision 3 + Q4 + F12.

4. **Pre-copy convergence — RESOLVED: 5-iter cap is the convergence gate**. CH v51.1 hardcodes pre-copy to 5 iterations; high-dirty-rate workloads do NOT converge in the spec sense — they emerge with stop-and-copy ≈ one iteration window of dirty pages. Phase 2 spec encodes `spec.maxPauseWindow` (operator chooses acceptable vCPU-paused window — workloads exceeding it rejected at admission via dirty-rate estimation) and `spec.timeout` (controller-level total-migration cap). **Realistic Phase 2 numbers**: vCPU-paused window 0.5–5 s for typical workloads; operator-visible BEACON gap 20–40 s (pre-copy iterations contribute). Spike doc Resolved Decision 4 + Q2 + F6 + F7.

### Phase 2 must-have-before-ship checklist

Status updated as PRs land; PR-A merged 2026-04-29 (swift-ch-client foundations + W2 cleanup + THREAT-MODEL.md), PR-B merged 2026-04-29 (action-loop refactor + migration ActionKinds + ack gate + W1 dispatch-side gate + sanitizer), PR-C in flight (receiver-mode launch branch + manual demo + cluster walkthrough).

- [x] **Threat-model gating** — `docs/design/THREAT-MODEL.md` shipped in PR-A + `kubeswift.io/migration-phase2-unsafe-plaintext: ack` annotation gate enforced inside `decide()` in PR-B (S2).
- [ ] **swiftletd reads URL inputs from SwiftMigration CR**, not from pod annotations (S1; ties to OQ6). Phase 2 manual path reads from operator-set annotations (acceptable per design §8.2.3 since operator IS the writer); Phase 3 deletes the annotation key entirely (§8.2.5 deprecation contract). PR-B tags every annotation-URL read with `// SECURITY-S1` for the Phase 3 grep check.
- [x] **swiftletd CH spawn `rm -f` API socket file before invoking CH** — `rm_stale_api_socket` in `rust/swift-ch-client/src/spawn.rs` (PR-A; covers spawn_ch, spawn_ch_restore, spawn_ch_receive). W2 walkthrough finding.
- [ ] **Resuming-phase guard pattern** — controller's Resuming phase MUST gate `phase=Completed` on actual destination guest state. **Controller-side work is Phase 3.** PR-B implements the dispatch-side W1 gate (vm_info probe post-call; w1_violation category) which is what swiftletd reports up to the controller.
- [ ] **Controller-level CPU-feature pre-flight check** in SwiftMigration validating webhook (OQ1; mitigates F12/S3). **Phase 3 work** — PR-B's sanitizer collapses raw CH errors into category tokens (`cpu_incompat`, etc.) defensively, but the real pre-flight check is in the SwiftMigration webhook.

### Phase 2 design open questions surfaced by the spike

(NOT in original four; require explicit treatment before swiftletd extension begins.)

1. Heterogeneous CPU microarch policy (controller pre-flight CPU-feature check, mirroring Phase 1's target node Ready check).
2. Destination listener timeout strategy (~30 s default, exposed as `spec.destinationTimeout`).
3. observedDowntime → split into `observedPauseWindow` + `observedTotalMigrationTime` in Phase 3 status reporting.
4. Progress-reporting mechanism: poll-`info`-API (recommended) vs tail-`--log-file` (Phase 3+ improvement).
5. Source-crash recovery model (no retry-same-destination after source crash; provision fresh dest).
6. Migration channel auth for Phase 3: sidecar mTLS vs first-party CH support; trust-anchor model. **Compose with S1.**
7. Audit logging policy (Kubernetes Events on each migration phase transition; operator-identity binding for opt-in flags).

### Spike-walkthrough operational findings

- **W1 — Walkthrough script self-narrated success on actual failure.** First run failed; script printed "no contradiction" because conclusion wasn't gated on observed state. Phase 2 controller's Resuming phase must avoid this same pattern.
- **W2 — Stale-state cleanup is the persistent operational hazard.** CH leaves API socket file on SIGKILL; next CH instance fails with "Address in use". swiftletd launcher entrypoint must `rm -f` the socket file before starting CH.

### Phase 2 walkthrough findings (post-PR-C cluster validation)

After PR-C (#29) merged + redeployed, attempting the manual demo in a fresh `mig-walkthrough` namespace surfaced TWO bugs that combined to silently break SwiftGuest IP discovery in any non-`default` namespace. Documented here because both bugs are pre-Phase-2 latent (snapshot Phases 0-2 + Phase 1 migration also affected on multi-namespace clusters), but Phase 2 walkthrough is what finally forced the architectural fix.

- **W3 — Per-namespace `swiftletd-reporter` RoleBinding required manual application.** Latent re-surface of snapshot walkthrough finding F2 (Scenario 1 setup, 6 days prior). `config/rbac/swiftletd-rolebinding.yaml` hardcoded `subjects[0].namespace: default`; operators were expected to `kubectl apply -k config/rbac -n <ns>` followed by a `kubectl patch` on every new namespace. Without the patch, swiftletd's pod-annotation writes hit 403 Forbidden, no `kubeswift.io/guest-ip` annotation got written, and the SwiftGuest's `status.network.primaryIP` stayed empty forever. **Fix: controller-driven auto-bind.** Promoted Role → ClusterRole (`kubeswift-swiftletd-reporter`) shipped via `make deploy` / Helm; SwiftGuest controller calls `EnsureSwiftletdRBAC` at the top of every Reconcile to idempotently create the per-namespace RoleBinding bound to the namespace's `default` ServiceAccount. Operators no longer apply per-namespace RBAC manually. Two post-hoc walkthroughs in 6 days hitting the same bug was the dispositive signal — the architectural fix shipped on the second occurrence.

- **W4 — Lease poller exited permanently after first patch failure.** Compounded W3: even when the RBAC arrived later in the pod's lifetime (operator manually applied the binding mid-flight), the lease poller had already terminated. `rust/swiftletd/src/lease.rs::spawn_lease_poller` had an unconditional `return` after the first `patch_pod_annotation` attempt regardless of result. **Fix: only `return` on patch SUCCESS.** Transient errors (kube client unavailable, RBAC gap, apiserver flap) now leave the poller alive for retry on the next 2s tick, bounded by the existing 4-minute MAX_ATTEMPTS cap. Same-shape bug as the snapshot poller's earlier handling; the lease poller was simply the only one left with the broken pattern.

- **W5 — Two post-hoc walkthroughs hit the same bug.** Snapshot walkthrough F2 documented W3's symptom but the disposition was "fix-in-walkthrough-PR" (the operator-walkthrough doc and the smoke test got the manual-apply incantation), NOT the architectural fix. Phase 2 walkthrough re-surfaced the same bug. **Pattern observation:** when an operator-experience finding is closed by "document the workaround" rather than "fix the root cause", the same finding will re-surface in the NEXT post-hoc validation. Worth applying to the Tracked Follow-up #2 ("operator-flow validation pattern in test infrastructure") — the walkthrough pattern should resolve findings architecturally on first occurrence, not on second.

W3 and W4 are fixed in PR #30 (`fix/swiftletd-rbac-auto-bind`). The original Phase 2 walkthrough was paused after surfacing these findings; it resumed after PR #30 merged + redeployed.

### Phase 3a Decisions Resolved (live migration)

Phase 3a spike completed 2026-05-01. Findings doc:
[`docs/design/live-migration-phase-3a-spike.md`](docs/design/live-migration-phase-3a-spike.md).
All four spike questions resolved with empirical evidence on the
deployed cluster (miles + boba, kernel-boot 4Gi guest, CH v51.1).

1. **Q1 — Controller orchestration**: state machine drives the four
   transitions (Validating → Preparing → StopAndCopy → Resuming →
   Completed) entirely via the Phase 2 annotation surface. Send-id
   derivation `<SwiftMigration>:send:<attempt-counter>` for idempotent
   retry across leader-handover (F1.8). Dst pod ownerRef:
   **conditional decision** — option 2 (SwiftGuest owns dst, with
   `migration-role` label) recommended IF Phase 3a makes SwiftGuest
   controller migration-aware. If rejected, options 1 (SwiftMigration
   owns dst) / 3 (no ownerRef + explicit cleanup) reopen. Spike
   doc F1.5.

2. **Q2 — Controller observation**: informer push latency ≤25ms on
   spike cluster (avg 20ms, max 24ms over 10 trials). Annotation
   schema sufficient for full state machine; no new annotations
   required. **Controller-manager observes both pods via informer
   alone — no cross-pod TCP connectivity needed.** This closes
   off the controller→swiftletd command channel as a Phase 3b
   design surface (F2.4); only swiftletd↔swiftletd needs Phase 3b
   mTLS hardening.

3. **Q3 — W1 gate-on-observed-state**: enforcement is in
   swiftletd-on-src's `vm.send-migration` dispatch (Phase 2 PR-B's
   W1 dispatch-side gate). Controller observation reduces to
   "informer event for src `migration-status=complete`" (F1.2).
   F1.9-vs-F4 contradiction RESOLVED: F1.9 (≥60s) captured silent-
   network failure mode; F4 (~3-5s) captured peer-abort failure
   mode; q3v3 surfaced a third (blackhole, ≥127s kernel TCP
   retransmit). All three handled by `spec.timeout` default 5min
   (F3.5).

4. **Q4 — K8s-initiated termination + node failure**: dst termination
   (any cause) → src writes `failed` cleanly (F4.1); src termination
   → no terminal status, controller detects via src pod UID change
   (F4.2). FailureReason enum: Cancelled / PodTerminated /
   SourcePodReplaced / Timeout / Other (F4.3). NO PDB on dst pod;
   Phase 4 webhook handles drain-mid-migration (F4.4). True node
   failure ≈ Q3-v3 blackhole; same coping path via `spec.timeout`
   (F4.5).

### Phase 3a PR 1 cluster validation (2026-05-03)

PR 1 merged + cluster-validated functional. Walkthrough surfaced
**nine findings (W13-W21)** across four image iterations.

**Four BLOCKING bugs fixed mid-walkthrough via hotfix PRs:**
- W13 ([PR #43](https://github.com/projectbeskar/kubeswift/pull/43)):
  controller didn't patch src pod with phase2-ack annotation
- W14 ([PR #43](https://github.com/projectbeskar/kubeswift/pull/43)):
  `deriveSubstate` didn't recognize `migration-status=rejected` as
  terminal
- W15 ([PR #44](https://github.com/projectbeskar/kubeswift/pull/44)):
  UID-check used `canonicalPodName` which resolves to dst post-step-1
- W16 ([PR #45](https://github.com/projectbeskar/kubeswift/pull/45)):
  swiftletd receiver-mode never flipped GuestRunning=True
  post-receive

**Five non-blocking findings** for follow-up PR:
- W17 (MEDIUM): pre-cutover Failed migration leaves dst pod running
- W18 (HIGH): failureReason classification can't distinguish dst-K8s-
  termination from generic src failures (maps to Other not
  PodTerminated)
- W19 (LOW): docs/migration/phase-3a.md W12 narrative out of date
- W20 (MEDIUM): cancel D1 fast-path doesn't fire while
  receive_migration blocks the action loop (Phase 3b candidate)
- W21 (HIGH): `SwiftMigrationConditionPodRefSwapped` never written
  → CancelIgnored gate broken; potential data-loss in narrow Resuming
  window. Same root cause as W15.

Pattern: four consecutive finding-behind-a-finding events. Each
BLOCKING bug at a different code path hid the next. The W5 lesson
restated for the fifth time in the project's history.

Walkthrough log:
[`docs/migration/phase-3a-cluster-validation.md`](docs/migration/phase-3a-cluster-validation.md).

**Cluster validation status: Phase 3a PR 1 mode=live live migration
functional** for **both in-scope workload classes** on default
node-local networking:
- **Kernel-boot** (`spec.kernelRef`): PR #46 walkthrough (10 scenarios)
  + post-W26 chain validation (2 runs, sourcePodRef confirmed).
- **RWX+Block disk-boot** (`spec.imageRef` + RWX+Block storage): E12
  walkthrough 2026-05-04 (S1 3-run + chain run 2 boba→miles + S2
  reconcile-recovery + S5 force-delete + S7 cancel-pre-cutover; all
  PASS post-W26).

### W26 — chain-migration BLOCKING bug surfaced by E12, fixed via PR #53

E12 surfaced **W26**: Phase 3a's three live-mode src-pod lookup sites
([`stopandcopy_live.go:184`](internal/controller/swiftmigration/stopandcopy_live.go),
[`cutover.go:167`](internal/controller/swiftmigration/cutover.go),
[`preparing_live.go:98,124`](internal/controller/swiftmigration/preparing_live.go))
derived src pod identity from cluster state — literal `guest.Name`
(W15 fix in two of them) or `canonicalPodName` (the third). Both
derivations break for back-to-back migrations: after a prior
migration's cutover, `SwiftGuest.status.podRef.Name` points at the
prior dst pod (= the new migration's src), not `guest.Name`. Literal
lookup hit NotFound → false-fired SourcePodReplaced; the naive
canonicalPodName-everywhere alternative would post-cutoverStep1
resolve to **THIS** migration's dst pod, and `cutoverStep2` would
delete the migrated guest (silent data destruction).

**Fix** (PR #53): stamp `status.SourcePodRef.Name = srcPod.Name` at
Validating-live (mirroring existing `SourcePodUID` lock-in); three
live-mode src lookups use `srcPodLookupName(mig, guest)` helper that
returns `SourcePodRef.Name` when set, falls back to
`canonicalPodNameForGuest`. Race-immune AND chain-safe.

**Workload-class-independent.** Same controller code runs for both
kernel-boot and disk-boot. Disk-boot E12 surfaced it because the
"or sequential miles→boba→miles→boba" S1 path naturally exercised
chain migrations; PR #46's three-run determinism gate ran on
non-chained runs (different validation question — timing-race
elimination per W22 lesson, not chain correctness).

**Phase 1 offline unaffected** — Approach A reuses `guest.Name` as
post-migration pod name, so literal-`guest.Name` lookups remain
correct there. W26 fix is live-mode-only.

W5 pattern restated for the **sixth** time. Future Phase 3a/3b
validation should include explicit chain-migration scenarios alongside
three-run determinism gates.

### Phase 3a PR 1 implementation status (Group B + Group C complete)

PR 1 (`feat/phase-3a-controller-core`) ships the SwiftMigration
controller core for `mode=live`. Implemented across 11 commits in
two groups:

**Group B — controller core (10 commits, B0 → B3.3):**
B0 (`a0e1526`) selectiveFailingClient + reconcile-recovery test
infra; B0.5 (`2d6f2dd`) shouldCheckSourcePodUID + isPostCutover
helpers; B1 (`0790711`) webhook MinLiveTimeout 60s; B2.1
(`9090b60`) Validating-live + auto-mode resolution; B2.2
(`337d900`) Preparing-live + dst pod construction; B2.3
(`7fb7cb7`) Resuming-live + ResumingStartedAt; B2.4 (`b110b29`)
cancel handler for spec.cancelRequested=true; B3.1 (`350a79e`)
StopAndCopy-live 6-substate state machine; B3.2 (`420b075`)
3-step cutover with retry-in-place; B3.3 (`16bf529`) failure-
detail classifier + reconcile-recovery tests covering §4.7.

**Group C — controller-runtime integration + operator docs:**
src-pod label patch at StopAndCopy entry (architect F-3) makes
src observable via the same labeled-pod watch as dst;
podToMigrations enhanced with label-based path
(`kubeswift.io/migration` label) covering both src and dst pods;
30s SyncPeriod registered as defense-in-depth (NOT primary
observation per §5.5); operator-facing reference at
`docs/migration/phase-3a.md` with W12 cancellation guidance,
post-migration pod name change behavior, and F2.4 architectural
simplification.

RBAC: B0.5 audit closure verified; no drift between
config/manager and charts/kubeswift Helm chart at semantic
verb-set level.

**Pending before PR 1 opens:** cluster integration testing
across the 10 scenarios from the original PR 1 prompt
(end-to-end on miles + boba; W12 cancellation path validation;
forced-failure mode coverage). Cluster integration is a separate
session.

### Phase 3a must-have-before-ship checklist

- [ ] **B0 — br0/Calico CIDR collision fix** ([PR #39](https://github.com/projectbeskar/kubeswift/pull/39),
  in-flight ahead of Phase 3a implementation). Launcher pod's
  hardcoded `10.244.125.0/24` br0 subnet collides with Calico per-
  node pod CIDRs on some clusters; cross-node TCP from miles-pod to
  boba-pod silently fails because dst pod's br0 (linkdown stub)
  shadows Calico's eth0 route for replies. Fix moves br0 to
  `192.168.99.0/24` (RFC1918 reservation). Affects all future
  kubeswift cross-pod-TCP workflows, not just Phase 3a. Spike doc
  B0 section.

- [ ] **swiftletd auto-write `failed` on abnormal listener exit**
  (F3.2). Without this, controller relies entirely on `spec.timeout`
  to escape stuck-at-running scenarios where dst CH listener died
  without writing terminal status. Phase 3a controller can ship
  without this if `spec.timeout` is the floor; cleaner with it.

- [ ] **swiftletd cancel handler implementation** (F3.4 / Phase 2
  PR-B's placeholder). Phase 3a's Cancel mechanism issues
  `migration-action: cancel` via annotation FIRST, falls back to
  pod-deletion only if cancel-handler times out. Phase 2 PR-B
  shipped a placeholder; Phase 3a needs the real implementation.

- [ ] **Controller-side `status.failureReason` enum** (F4.3) with
  values: Cancelled / PodTerminated / SourcePodReplaced / Timeout /
  Other. Distinguishes the failure modes operators see in `kubectl
  describe swiftmigration`.

- [ ] **`spec.timeout` default 5m** (F3.5) — empirically grounded
  in Q3-v3 blackhole behavior (kernel TCP retransmit ~127s default).

### Phase 3a design open questions surfaced by the spike

These are NOT spike questions; they're decision points Phase 3a
design must address.

1. **SwiftGuest controller migration-awareness** (F1.5 conditional).
   Phase 3a's first design decision. If yes → dst pod ownerRef =
   SwiftGuest with `migration-role` label. If no → reopen
   ownerRef options 1 (SwiftMigration owns) or 3 (no ownerRef +
   explicit cleanup) with additional empirical validation outside
   the spike.

2. **dst-side `migration-status=running` ambiguity** (F1.1). The
   same value fires at receive-accept-time AND at terminal-success
   on dst. F1.2's recommendation (gate Completed on src-side
   `complete`) routes around it. **Phase 3a may also request
   swiftletd-side rename of the dst-side terminal value** (e.g.,
   `complete` instead of `running`) — cleaner state-machine
   semantics, but not blocking for Phase 3a.

3. **Multi-migration concurrency**. Default recommendation:
   serialize per-source-node (refuse new SwiftMigration whose
   source is a node with an in-flight SwiftMigration). Spike doc's
   "Open questions for Phase 3a design".

4. **Progress visibility (F2.5)** — already filed as Phase 5
   backlog item above. Operators watching a 38s SwiftMigration with
   no progress visibility will surface it as a usability gap during
   first production rollouts.

## Phase 3b Spike — COMPLETE (Decisions Resolved)

Phase 3b spike completed 2026-05-08. Findings doc:
[`docs/design/live-migration-phase-3b-spike.md`](docs/design/live-migration-phase-3b-spike.md).
All four Phase 2 pending-decision items from the Phase 3a close-
out are answered. Branch `spike/phase-3b-q1-q4` retained for
reference; **NOT for merge** per spike contract. Phase 3b design
conversation begins in a separate session anchored on this
findings doc.

1. **swiftletd control surface for migration actions — SHIPPED via
   spike (Q1).** PASS conditional: annotation-driven pattern holds
   for Phase 3b's expected use (state-machine transitions, 4-6
   patches per migration). Per-iteration progress reporting
   rejected by Phase 3b design **independent of CH limitations** —
   even if a future CH version exposes per-iteration timing,
   annotation surface is wrong tool for streaming-progress data
   (apiserver-bounded ~540ms median, 50 iterations × 540ms ≈ 27s
   pure overhead vs 38s data-transfer body). Progress visibility,
   if ever needed, routes through a separate streaming channel
   (swiftletd HTTP status endpoint, upstream CH telemetry, or
   external network observer). Supersedes Phase 2 Resolved
   Decision 1.

2. **mTLS posture — UNCHANGED.** Phase 2's plaintext-TCP-with-
   security-gating + `kubeswift.io/migration-phase2-unsafe-
   plaintext: ack` posture remains. Phase 3b inherits without
   modification. mTLS handoff is Phase 3c+ work; spike did not
   exercise this surface.

3. **Same-CH-version constraint — SUPERSEDED via spike (Q3).**
   Phase 3a's
   [`internal/controller/swiftmigration/dst_pod.go::newDstPod`](internal/controller/swiftmigration/dst_pod.go)
   constructs the destination pod via `srcPod.DeepCopy()` —
   cloning the source pod's spec including launcher image. This
   structurally guarantees match-tag at pod construction; there is
   NO controller code path that produces a heterogeneous src/dst.
   **Phase 2 Decision 3 retired**: webhook match-tag rule is
   redundant (implementation already enforces atomically), and
   `spec.allowVersionSkew=true` opt-in escape hatch is dropped
   from Phase 3b API surface (no controller code path consumes
   it). Cluster-empirical: cross-version v50.2 ↔ v51.1 deployment
   could not be produced through the controller; both spike runs
   ran homogeneous v50.2 ↔ v50.2 by structural construction.
   Document `newDstPod` clone-src as a LOAD-BEARING architectural
   property in Phase 3b design doc to future-proof against
   refactor regression (W26 lesson pattern: a future "let's
   re-resolve dst from SwiftGuest spec — cleaner" refactor
   would silently re-introduce the skew surface).

4. **Pre-copy convergence test surface — SHIPPED via spike (Q2).**
   PASS through 50%-of-RAM dirtied (4 workers × 512M continuous
   `rand-set` on a 4Gi guest). Termination behavior is
   **iteration-cap-bounded, NOT classical algorithmic
   convergence** — CH v51.1 hardcodes pre-copy to 5 iterations
   then unconditionally enters final stop-and-copy regardless of
   dirty rate. **This is a CH-version dependency** that Phase 3b
   inherits: future CH versions making the cap configurable or
   replacing it with classical dirty-rate-vs-bandwidth detection
   would change Phase 3b's webhook policy ("no admission gate on
   dirty rate" remains correct for CH v51.x) and operator-visible
   downtime characteristics. Empirical numbers (4Gi guest, 4
   migrations): pauseWindow scales 1.0× → 1.18× → 1.79× → 2.29×
   across baseline → LOW (1×64M) → MED (2×256M) → HIGH (4×512M)
   stress-ng intensities; observedDowntime stays bounded at
   ~2-3s across ALL workloads. Supersedes Phase 2 Resolved
   Decision 4.

### Phase 3b spike — additional finding outside the four

**Pod-network TCP plumbing (Q4) — PASS.** Default Calico VXLAN at
MTU 1450 saturates the underlying NIC (~902 Mbit/s on the spike
cluster's Hetzner gigabit interconnect) with low retransmissions,
no MTU sensitivity, and symmetric direction-pair behavior. CH
live-migration data path achieves **~95% of raw TCP bandwidth**
(Q2 inferred 107.2 MB/s ÷ Q4 measured 112.75 MB/s = 0.951);
orchestration overhead is ~5%. **No dedicated migration network
needed for Phase 3b** — default pod network is sufficient.
Operator sizing formula for live-migratable guests: expected
pauseWindow ≈ `(guest_RAM × 1.05) / pod_network_bandwidth`.

### Phase 2 walkthrough resumption (post-PR-#30 redeploy)

After PR #30 merged + redeployed, the walkthrough resumed in a fresh `mig-walkthrough` namespace. Two more findings surfaced (W6, W7); one (W7) was a follow-up to PR #30 fixed inline; one (W6) is a design contradiction in PR-C requiring disposition before further Phase 2 work.

- **W6 — Phase 2 manual demo cannot complete on RWO-only storage; design doc §5.1.2 contradicts live-migration.md Constraint 4.** PR-C's `live-migration-phase-2.md` §5.1.2 said "RWO is required" and "RWX is rejected" for the destination-receive pod template. In practice the destination pod hits `FailedAttachVolume: Multi-Attach error` because the source pod still has the same RWO PVC mounted on `miles`. The §5.1.2 text conflates the F2-split-brain risk (which RWO does mitigate) with the live-migration disk-handoff requirement (which RWO does NOT solve without Phase 3's RWO-handoff choreography per `live-migration.md` Constraint 4). The Phase 2 spike's Q1 evidence was kernel-boot/initramfs-only — it never exercised the disk-handoff scenario. **Disposition:** Phase 2 manual demo on disk-boot guests requires either (a) a kernel-boot variant of the demo template that omits the PVC mount, (b) RWX storage, or (c) Phase 3 controller integration with the RWO-handoff choreography. Recommend (a) for any further Phase 2 wire-protocol demonstrations on the current cluster (Longhorn-RWO); defer (c) to Phase 3 design work. Detail in [`docs/design/live-migration-phase-2-walkthrough.md`](docs/design/live-migration-phase-2-walkthrough.md).

- **W7 — controller-runtime cached client requires `list,watch` on RoleBindings.** PR #30's grant of just `get,create` was insufficient — every Reconcile in a workload namespace logged `Failed to watch *v1.RoleBinding: rolebindings.rbac.authorization.k8s.io is forbidden: User "system:serviceaccount:kubeswift-system:controller-manager" cannot list resource "rolebindings"`. The cache layer never synced, so `EnsureSwiftletdRBAC`'s `Get` blocked indefinitely; SwiftGuest pods never got created. Same controller-runtime architectural property affects every namespace-scoped resource the controller reads via the cached client. **Fixed in commit `e794471` (direct push to main):** verbs extended to `get, list, watch, create` in both `config/manager/controller-manager-rbac.yaml` and the Helm chart. Cluster ClusterRole patched live + controller restarted; SwiftGuest reconciled successfully thereafter. **Pattern observation:** this regression escaped the unit tests in PR #30's `rbac_test.go` because they use the fake client (no informer cache); a real-cluster smoke test would have caught it. Adds weight to Tracked Follow-up #2 (operator-flow validation pattern in test infrastructure) — fake-client tests verify control-flow but not RBAC sufficiency.

W6 is the **third** post-hoc walkthrough to surface a finding the spike did not catch (after F2 in snapshot walkthrough, W3 in Phase 2 walkthrough). The W5 pattern restated: spike findings under-constrain the design when they validate a simplified scenario; the broader operator scenario reveals contradictions. The Phase 2 spike's kernel-boot guest sidestepped disk handoff; the operator walkthrough's Ubuntu Noble disk-boot guest exercised it.

What the post-resumption walkthrough DID validate end-to-end:
- W3 + W4 fixes shipped cleanly (auto-bind + lease retry-on-failure both observable in fresh namespace).
- swiftletd image `sha-6fa2394` carries PR-A + PR-B + PR-C + the env-var-race fix.
- `make migration-phase2-manual` orchestration scripts (source.sh + destination.sh up to apply) correctly extract metadata + render dst pod template.

What it did NOT validate (blocked on W6):
- Receiver-mode launch branch (`run_ch_receive`) actually running in production.
- Cross-node `send-migration` wire protocol on a real disk-boot guest.
- Sentinel md5 survival post-migration.
- Timing measurements (vCPU pause window, BEACON gap, total downtime).

Pre-migration sentinel md5 captured anyway for any future re-run on this same source pod: `88d94a051ea2db180606535a4125784d` (sentinel `SPIKE-PHASE2-WT-1777503996`, written via serial console).

### PR #32 walkthrough findings (post-merge cluster validation)

After PR #32 (storage access mode CRD) merged + redeployed, the cluster
validation exercise in `default` namespace surfaced two findings (W8, W9).
The framing applies the now-recurring pattern: **each shipped layer reveals
what the next layer needs**. Phase 1 (offline migration) revealed Phase 2's
need (live migration plumbing). Phase 2 walkthrough revealed PR #32's need
(API surface for storage access mode). PR #32 walkthrough now reveals W9
(runtime-path support for Block volumeMode). W9 is **not a PR #32
regression** — PR #32's stated scope was the API-surface unblock, and
every piece of that surface is validated. W9 is the next phase the
unblock makes addressable.

- **W8 — controller-runtime cached client requires `list,watch` on
  StorageClass.** PR #32's `checkStorageReady()` calls `r.Get` on
  StorageClass to verify the Longhorn migratable parameter. Same shape as
  W7 (rolebindings) and Phase 2 walkthrough W3 (RBAC gap): adding a
  cached-client `r.Get` on a new resource type without granting
  `list,watch` starves the reconcile queue ("Failed to watch
  *v1.StorageClass" loop, no SwiftGuest reconciles fire). Unit tests
  passed because fake-client doesn't use informers. **Fixed in commit
  `8f5265e` on the PR #32 branch:** verbs extended to `get, list, watch`
  in both `config/manager/controller-manager-rbac.yaml` and the Helm
  chart. **Recurring lesson** (W7 + W8 are the same lesson): when adding
  a `r.Get` on a new resource type from inside the controller-runtime
  cached client, grant `list,watch` alongside `get`. Adds further weight
  to Tracked Follow-up #2 (operator-flow validation pattern in test
  infrastructure) — fake-client tests verify control-flow but not RBAC
  sufficiency.

- **W9 — runtime-path gap: Copy Job + launcher pod + swiftletd do not
  yet support `volumeMode: Block` destinations.** With PR #32 shipped,
  applying a SwiftGuest with `spec.storage.{accessMode: ReadWriteMany,
  volumeMode: Block, storageClassName: longhorn-migratable}` resolves
  correctly, populates `status.storage`, surfaces `StorageReady=True`,
  and creates the per-guest clone PVC bound at 40Gi RWX on the migratable
  Longhorn class. The gap surfaces **at the rootdisk Copy Job step**:
  kubelet refuses with `volume dst has volumeMode Block, but is specified
  in volumeMounts`. The Copy Job in
  `internal/controller/swiftguest/rootdisk.go::createCloneJob` mounts the
  destination as a filesystem path (`volumeMounts: /dst`) and runs
  `cp /src/image.raw /dst/image.raw` — which only works on Filesystem-mode
  PVCs. Block-mode PVCs need `volumeDevices` + raw-device write
  (`dd`/`qemu-img convert` to `/dev/dst-block`). The launcher pod and
  swiftletd have the analogous gap further along the path: both currently
  mount the root PVC as a filesystem path and pass `--disk
  path=/var/lib/.../image.raw` to Cloud Hypervisor; for Block they would
  need `volumeDevices` and `--disk path=/dev/...`. **Disposition: defer
  to a follow-up PR** scoped as "Storage RWX+Block runtime path."
  Detail and scoping questions in
  [`docs/design/storage-rwx-block-runtime.md`](docs/design/storage-rwx-block-runtime.md).
  PR #32 ships and is complete on its API-surface scope; the runtime-path
  follow-up uses the same surface.

W8 + W9 are the **fourth and fifth** post-hoc walkthroughs in 9 days to
surface a finding the spike-and-tests cycle did not catch (after snapshot
F2, Phase 2 W3, Phase 2 W6). The W5 pattern continues to restate itself:
unit tests with fake clients verify control-flow shape but not RBAC
sufficiency or kubelet-mount-side semantics; spike scenarios validate
simplified inputs and miss the operator's full-feature target shape. This
is now durable signal for Tracked Follow-up #2 — the operator-flow
validation pattern needs to land as part of the test infrastructure, not
as the next phase's after-the-fact discovery.

### PR #35 (W9 runtime path) walkthrough findings — 2026-04-30

PR #35 shipped W9 in three components: Copy Job + launcher pod
builder + restore-receive launcher + clone-grow-init Block path
(controller-side) + Rust opacity contract (swiftletd / swift-ch-client
docs + tests verifying disk_path passes opaquely to `--disk path=`).
Cluster mini-walkthrough on `default` namespace with `longhorn-migratable`
StorageClass (Longhorn `parameters.migratable: "true"`). Two findings:

- **W10 — CH `Request check failed: ... ReadOnly` WARN at sector 0
  during early boot of Block-mode guests; non-blocking.** The launcher
  log shows 2x WARNs at ~18s and ~23s into boot, both writes to sector 0
  (likely a bootloader / GPT scan write). CH's `vm.info` reports
  `readonly: false` for the disk, the device is `O_RDWR | O_NONBLOCK |
  O_CLOEXEC` per `/proc/$pid/fdinfo`, the launcher container can write
  to `/dev/kubeswift-root` directly with `dd`, and the `growpart` +
  `resize2fs` chain in cloud-init ultimately succeeds (verified by
  `df -h /` reporting 37G of 40G after first-boot, dmesg showing
  `EXT4-fs (vda1): resized filesystem from 655099 to 10223355 blocks`).
  After the two boot-time WARNs, no further ReadOnly warnings for
  the lifetime of the guest. **Disposition: noisy boot-time diagnostic,
  no functional impact.** Worth investigating in CH source to understand
  why `Request::check()` returns `Error::ReadOnly` on a disk whose
  config says `readonly:false` — likely a CH v51.1 quirk in the early-
  boot virtio-blk request validation path. Document for operators so
  they don't mistake the WARN for a real failure; revisit if a future
  CH version surfaces it as a hard error.

- **W11 (= W9.x) — `cloneStrategy=snapshot` + `volumeMode: Block` fails
  at PVC provisioning.** The CSI external-snapshotter refuses to clone
  a Filesystem-mode source VolumeSnapshot (the SwiftImage's clone-seed,
  taken from a `longhorn` Filesystem PVC) into a Block-mode destination
  PVC (the SwiftGuest's clone PVC on `longhorn-migratable`):
  > `error getting handle for DataSource Type VolumeSnapshot ... requested volume modifies the mode of the source volume but does not have permission to do so. snapshot.storage.kubernetes.io/allow-volume-mode-change annotation is not present on snapshotcontent ...`

  Per W9 prompt's binding language ("Only if it does NOT work does it
  become W9.x with a separate follow-up issue. The 'OR' in the W9
  prompt was deliberate"), this becomes **W9.x — separate follow-up**.
  Fix surface is small: the SwiftImage controller's snapshot-creation
  path (where it generates the cloneSeed VolumeSnapshot for
  `cloneStrategy: snapshot`) needs to set the
  `snapshot.storage.kubernetes.io/allow-volume-mode-change: "true"`
  annotation on the resulting VolumeSnapshotContent. **Operator
  workaround until W9.x ships:** for RWX+Block guests, use
  `cloneStrategy: copy` (the default — exercised end-to-end in this
  walkthrough). Snapshot-strategy clones remain available for
  Filesystem-mode guests (the existing path).

What the walkthrough VALIDATED end-to-end on cluster (W9 acceptance):

| | Result |
|---|---|
| RWX+Block SwiftGuest reaches Phase=Running | ✓ ~2m18s clone Job + ~30s boot |
| `status.network.primaryIP` populated | ✓ via DHCP+annotation pipeline |
| Pod manifest: VolumeDevices=[{root-disk, /dev/kubeswift-root}] | ✓ |
| Pod manifest: no root-disk VolumeMount on Block | ✓ |
| Console login (kubeswift/kubeswift) | ✓ |
| `swiftctl ssh -i <key> rwx-test` | ✓ (operator-confirmed) |
| `df -h /` reports ~37G of 40G | ✓ — growpart + resize2fs on Block work |
| Block PVC persistence across pod recreate | ✓ same PVC UID, guest reboots cleanly |
| RWO+Filesystem regression (`rwo-test` + smoke-test `sample`) | ✓ both Phase=Running with default RWO+Filesystem |
| Pre-W9 manifest with no `spec.storage` block | ✓ resolves to legacy RWO+Filesystem |
| Scoping (a): SwiftImage import PVC stays Filesystem | ✓ `RWO Filesystem longhorn` |
| Scoping (c): sgdisk-on-Block via clone-grow-init | Deferred — exercised only on snapshot path which is W9.x-blocked |
| `cloneStrategy=snapshot` + Block | ❌ → W9.x (CSI annotation gap) |

**Pattern for the project (W5 restated yet again):** the cluster
walkthrough caught W10 + W11 that the unit-test cycle could not — a
CH-runtime-noise WARN that fake-client tests can't see, and a CSI
inter-driver behaviour that doesn't reach Go test surface. Adds yet
more weight to Tracked Follow-up #2 (operator-flow validation pattern
in test infrastructure).

---

## Bugs Fixed (Recent — Snapshot and Migration Phases)

(Bugs 1-46 from v0.1.0+ unchanged; see prior context doc revisions.)

| # | Component | Bug | PR |
|---|-----------|-----|-----|
| 47-53 | Snapshot Phases 0-2 | (See "Snapshot bug-fix history" table above — PRs #11-#18) | #11-18 |
| 54 | swiftguest/rootdisk.go | EnsureRootDiskClone deleted SwiftRestore-seeded PVCs as orphans (silent data loss in Tier A) | PR #21 |
| 55 | CI workflow | e2e tests existed but never ran in CI | PR #22 |
| 56 | swiftmigration/webhook | Stuck finalizer when source SwiftGuest deleted before SwiftMigration | PR #25 |
| 57 | swiftmigration/controller | Reconcile loop on terminal-phase SwiftMigrations | PR #25 |
| 58 | swiftmigration/webhook | Per-operation discipline refactor (subsumes A/B/C as architectural rule) | PR #26 |
| 59 | swiftguest/rbac.go (new) | swiftletd RBAC was per-namespace Role + manually-applied RoleBinding; silently broke IP discovery in non-default namespaces. Promoted to ClusterRole + controller-driven auto-bind. (Re-surface of snapshot walkthrough F2; W3 in Phase 2 walkthrough.) | PR #30 |
| 60 | rust/swiftletd/src/lease.rs | Lease poller `return`-ed unconditionally after first patch attempt; transient 403 (W3 RBAC gap) killed the poller permanently. Only `return` on patch success now; retry on transient errors. (W4 in Phase 2 walkthrough.) | PR #30 |
| 61 | api/swift/v1alpha1 + controller + webhook | Storage access mode CRD: SwiftGuestClass.spec.storage and SwiftGuest.spec.storage select accessMode/volumeMode/storageClassName for controller-created PVCs. CRD admission rejects RWX+Filesystem (Filesystem RWX is not live-migration-capable). SwiftMigration webhook gains forward-compat live-mode storage gate (recompute from spec, NOT read status — write-back-race avoidance). Defaults preserve current behaviour (RWO+Filesystem). Resolves W6 design contradiction at the API surface; storage architecture review owns the deeper questions (CSI driver matrix, F2 split-brain on RWX). | PR #32 |
| 62 | rbac (controller-manager ClusterRole) | StorageClass `list,watch` verbs missing — controller-runtime's cached client opens an informer on every GETable resource; PR #32's `checkStorageReady`'s `r.Get` on StorageClass triggered "Failed to watch" loop, starving SwiftGuest reconcile queue. Fake-client unit tests passed (no informer). Same shape as W7 (rolebindings). (W8 in PR #32 walkthrough.) | PR #32 |
| 63 | rootdisk Copy Job + launcher pod builder + clone-grow-init + restore-receive launcher + RuntimeIntent producer + rust opacity contract | Block volumeMode runtime path: Copy Job branches to `volumeDevices` + `qemu-img convert + sgdisk -e` (no cp, no resize) for Block destinations; launcher pod uses VolumeDevices at `/dev/kubeswift-root`; clone-grow-init runs sgdisk -e against device path on Block (skips qemu-img resize as no-op); RuntimeIntent.RootDisk.Path resolves to device path for Block guests; rust crates verified suffix-free via Q2 grep audit. End-to-end cluster validation: RWX+Block guest boots, growpart succeeds, df reports ~37G of 40G, PVC persistence across pod recreate verified. Two findings (W10 noisy boot WARN non-blocking; W11=W9.x cloneStrategy=snapshot+Block fails at CSI provisioning, deferred). | PR #35 |
| 64 | swiftmigration controller (validating_live + stopandcopy_live + cutover + preparing_live) | Phase 3a back-to-back live migrations false-fired SourcePodReplaced (and carried a latent guest-destruction vector at cutoverStep2). Three live-mode src-pod lookup sites derived src pod from cluster state; both literal-guest.Name (W15 fix) and canonicalPodName broke for chain migrations. Fix: stamp status.SourcePodRef.Name at Validating-live (mirrors existing SourcePodUID lock-in); use srcPodLookupName helper at all sites. Race-immune AND chain-safe. Workload-class-independent — same code runs for kernel-boot and disk-boot. (W26 in E12 disk-boot validation 2026-05-04.) | PR #53 |
| 65 | swiftmigration controller (resuming_live + cutover + stopandcopy_live) | Phase 3a downtime metrics broken/half-wired. (W27a) status.observedDowntime measured two adjacent metav1.Now() calls in the same reconcile invocation, producing 34-114µs across all 17 walkthrough runs vs a real cutover window of ~38-48s. Fix: new status.cutoverStep2DispatchedAt timestamp stamped at cutoverStep2 Delete dispatch; observedDowntime computed against it at Resuming completion. (W27b) status.observedPauseWindow plumbing half-implemented — swiftletd wrote kubeswift.io/migration-pause-window-ms annotation correctly but controller had zero readers. Fix: stampObservedPauseWindow helper reads annotation at substateSrcCompleted (W1 gate observation), mirrors snapshot controller's pattern. Both fields now carry their documented semantics. Defensive nil/parse handling on both. (W27 follow-up to E12 walkthrough.) | PR #55 |
| 66 | swiftctl (internal/cli/guest.go GuestResolver.ResolvePod) | swiftctl pod resolution had two foot-guns surfacing during Phase 3a live migration cutover and chain migration. **Foot-gun 1**: when status.podRef was set but the named pod returned NotFound (cutover transient: podRef just patched to dst-suffix but dst pod not yet created, OR src deleted before podRef patched), ResolvePod errored out instead of falling through to the label-selector path. **Foot-gun 2**: when the label selector returned multiple labeled pods (chain-migration transient: M1 src still Terminating + M2 dst Running, both labeled `swift.kubeswift.io/guest=<name>`), `list.Items[0]` was non-deterministic — apiserver might return Terminating-first. Fix: NotFound on PodRef.Get falls through to the label-selector path; multi-pod selector results stable-sorted by (non-Terminating > Running > newest CreationTimestamp); all-Terminating fallback returns newest with stderr warning. Function signatures unchanged. Cluster-validated: chain-migration dual-labeled-Running state captured at t+16s of M2; race probe ~290 calls during M3 hit zero "not found" errors; W2 walkthrough recorded clean state. (W2 walkthrough findings W2-1 + W2-2 are non-PR-2 issues filed as Tracked Follow-ups #8 + #9.) | PR #57 |

---

## Deployment

```bash
make build-images push-images deploy
```

`make deploy` must:
1. Run controller-gen to regenerate CRD YAML
2. `kubectl apply -k config/crd` + wait for Established
3. Deploy controller-manager

**Never let CRD schema drift from Go types.** API server silently drops unknown fields.

After CRD changes:
```bash
make generate
cp config/crd/bases/*.yaml charts/kubeswift/crds/
```

### Smoke Test
```bash
make smoke-test
```

Multi-nic scenario currently flakes due to Longhorn volume attach issue unrelated to KubeSwift code. Other 4 scenarios pass cleanly.

### CI Workflow (per PR #22)
- `verify-e2e-scripts` runs lint check on every PR
- `e2e-on-cluster.yaml` workflow runs e2e tests on path-touch trigger for `internal/controller/{swiftrestore,swiftguest,swiftmigration}/**`, `api/**`, and `rust/**`

---

## Roadmap

### Completed (v0.1.0+)
VM lifecycle, networking, IP discovery, status reporting, swiftctl commands, smoke test, observability, runPolicy modes, image pipeline. SwiftKernel + per-node OCI artifact pull. SwiftGPU Phases 1-3 (Tier 1 validated). Host runtime hardening. dataDiskRef. GPU Discovery DaemonSet. Multi-NIC. OVN-K integration guide. SR-IOV NIC passthrough. SwiftGuestPool with rolling updates and PVC per replica.

### Completed (Snapshots Phases 0/1/2 + Operator Walkthrough)
See dedicated sections above. Three PRs from walkthrough: #21 Tier A fix, #22 CI wiring, #23 walkthrough doc + findings.

### Completed (Live Migration Phase 1)
See dedicated section above. Three PRs: #24 initial implementation, #25 terminal-state fixes, #26 per-operation discipline refactor.

### Next Priorities (in order)

**1. Live Migration Phase 2 — swiftletd live migration plumbing — SHIPPED across 3 PRs**
- PR-A (#27 — merged): swift-ch-client send_migration / receive_migration / spawn_ch_receive primitives; W2 stale-socket cleanup; THREAT-MODEL.md banner.
- PR-B (#28 — merged): action-loop refactor (KeySet abstraction, per-namespace ActionState); migration ActionKind variants (Send/Receive/Cancel); plaintext-ack gate inside decide(); status-id-paired-write (write_migration_status); W1 dispatch-side gate; detail-string sanitizer; mutual rejection across namespaces. 32 snapshot tests preserved + 22 new migration tests.
- PR-C (in flight): receiver-mode launch branch (RuntimeIntent.migration; KUBESWIFT_MIGRATION_ROLE env var; launch.rs run_ch_receive); namespace-translated terminal-success status (complete/running per design §3.1); manual demo scripts; cluster mini-walkthrough.

Phase 2 deliverable surface complete: operators can manually demonstrate cross-node CH live migration via `make migration-phase2-manual SWIFTGUEST=<name> TARGET_NODE=<node>`. No controller integration (Phase 3); no mTLS (Phase 3); no drain integration (Phase 4).

**2. Storage RWX+Block runtime path (W9 — SHIPPED via PR #35)**
- Copy Job in `rootdisk.go` branches on `rg.Storage.VolumeMode` for
  Block destinations: `volumeDevices` + `qemu-img convert + sgdisk -e`
- Launcher pod builder + clone-grow-init + restore-receive launcher use
  shared `rootDiskMount(rg)` helper; Block guests get VolumeDevices
  at `/dev/kubeswift-root`
- RuntimeIntent.RootDisk.Path resolves to the device path for Block
- Rust opacity contract verified: zero suffix-detection logic in
  `swift-ch-client` or `swiftletd` (Q2 sidebar grep result documented
  in PR #35); doc comments codify the opacity contract
- Cluster validation: RWX+Block guest boots, cloud-init growpart works
  (`df -h /` reports ~37G of 40G), PVC persistence across pod recreate
- Two follow-ups: W10 (CH boot-time ReadOnly WARN, non-blocking) and
  W11=W9.x (cloneStrategy=snapshot + Block, separate follow-up — see
  Tracked Follow-up #6 for details and Tracked Follow-up #7 below)

**3. W9.x — `cloneStrategy=snapshot` + `volumeMode: Block` (small follow-up to PR #35)**
- The SwiftImage controller's cloneSeed VolumeSnapshot needs the
  `snapshot.storage.kubernetes.io/allow-volume-mode-change: "true"`
  annotation when the SwiftImage may be cloned to Block-mode PVCs
  (or unconditionally — the annotation is no-op when destination
  volumeMode matches source)
- ~30 lines of Go change in the SwiftImage controller's snapshot
  creation path; one cluster integration test verifying RWX+Block guest
  boots from a cloneStrategy=snapshot SwiftImage

**4. Live Migration Phase 3 — live mode + mTLS**
- SwiftMigration controller gains live mode
- mTLS sidecar for migration channel
- Pre-copy convergence handling

**4. Live Migration Phase 4 — drain integration via eviction webhook**
- `kubectl drain` triggers migration automatically
- Independent value: drain integration with offline migration alone dramatically improves operator UX
- Could jump sequence if operator demand for safe drain dominates

**5. Live Migration Phase 5 — operational polish**
- Prometheus metrics, dashboards, retention
- **swiftletd progress annotations (Phase 3a spike F2.5).** Phase 2
  design §3 mentioned intermediate `migration-progress` annotation
  values (`precopy` / `stopcopy` / `listening` / `transferring`)
  but Phase 2 PR-B does NOT emit them — operators only see
  `running` (accept) → terminal `complete` / `running` (post-resume).
  Phase 3a spike confirmed this is a correctness-no-op for the
  state machine, but operators watching a 38s SwiftMigration with
  zero progress visibility will surface it as a usability gap
  during Phase 3a's first production rollouts. Implementation:
  swiftletd emits `kubeswift.io/migration-progress` annotation
  during pre-copy iterations + stop-copy + listening states;
  controller surfaces in SwiftMigration `status.phaseDetail`.
  Tracked here so Phase 3a's spike doc is not the canonical
  source.

### Snapshot Roadmap Continuation (deferred behind live migration)

**Snapshot Phase 3 — Tier C (S3 / object storage export)** — cluster-portable snapshots, ~4-5 days
**Snapshot Phase 4 — cloneFromSnapshot ergonomics** — pool template support, ~3-5 days, walkthrough Scenario 7 documented operator demand
**Snapshot Phase 5 — operational polish** — Prometheus metrics, dashboards, retention, ~2-3 days

### Other Roadmap Items Not Progressed
- **Windows guest support** — no design doc, implementable
- **Multi-NIC + SR-IOV hardware validation** — code shipped, hardware not available
- **Tier 2 GPU validation** — needs HGX hardware
- **GitOps documentation phases** — design exists; pure operator value, mostly docs

---

## Hardware Available
- 3-node k0s cluster (frida control-plane, miles + boba workers), Ubuntu 24.04, CH v51.1, Longhorn 22d
- boba has GeForce GTX 1080 (Tier 1 GPU validated)
- No SR-IOV NICs, no HGX, no multi-NIC servers currently

---

## Design Principles

1. **Minimalism** — avoid unnecessary complexity, deps, abstraction layers
2. **Cloud Hypervisor first** — CH is default; QEMU only when hardware requires it
3. **Raw disk at runtime** — qcow2 input only; runtime always raw
4. **Kubernetes-native** — everything observable via kubectl
5. **Strong operability** — operators discover IP, connect console, SSH, inspect status
6. **No silent failures** — status fields reflect real state; never drop errors
7. **Verified fixes only** — no speculative patches; diagnose with real cluster output first
8. **Distributed by design** — no single-node assumptions
9. **Hardware-aware** — GPU workloads need correct PCIe topology, NUMA, driver alignment
10. **Treat terminal states as terminal** (PR #26 lesson) — validation and reconciliation logic must explicitly enumerate which operations they fire on; default-to-everything is the bug pattern, default-to-explicit is the discipline

---

## AI Assistant Instructions

When helping develop KubeSwift:

- Read this document and session transcripts before starting work
- Check `/mnt/transcripts/journal.txt` for previous session summaries
- Prefer minimal changes — one bug fix at a time, verified with real output
- Always ask for actual cluster output before suggesting fixes
- Never assume a fix worked without confirming logs
- All pod containers run with capability-based permissions, not privileged: true
- When writing prompts: be explicit about what NOT to change
- CRD changes require `make generate` + copy to charts/kubeswift/crds/ + redeploy
- Working guest OS (disk boot) is Ubuntu Noble (24.04)
- CLOUDHV.fd is loaded via `--kernel`, not `--firmware`
- swiftletd reports status via pod annotations
- RestartPolicy on launcher pods is always Never
- imageRef and kernelRef are mutually exclusive
- gpuProfileRef can combine with imageRef but NOT with kernelRef
- SwiftKernel node opt-in label: kubeswift.io/kernel-node=true
- SwiftGPU node opt-in label: kubeswift.io/gpu-node=true
- GPU: Tier 1 PCIe uses Cloud Hypervisor; Tier 2/3 HGX SXM requires QEMU with pcie-root-port per device
- SwiftGPU controller name is "swiftgpu" (explicit .Named() to avoid collision)
- Security: NO container uses privileged: true — all use drop ALL + specific capabilities
- gpu-init.sh uses /host/sys (not /sys) for sysfs writes
- All shell scripts in container images must be pure ASCII
- Container ENTRYPOINT and init container commands must use explicit interpreter (/bin/sh or /bin/bash)
- Container memory limits must include LauncherMemoryOverheadMiB (512MiB) above guest RAM
- /dev/vfio hostPath must use DirectoryOrCreate
- Import pipeline must run sgdisk -e after qemu-img resize
- SwiftGuestPool template hash is spec-only — metadata changes don't trigger rollout
- SwiftGuestPool PVCs are owned by the pool, not by individual SwiftGuests
- **Snapshots Phase 2 — clone restore: identity collision is fundamental (resume-vs-boot). Operators reboot or manually regenerate**
- **Snapshots Phase 2 — VFIO + includeMemory rejected at admission (CH cannot RESTORE VFIO state)**
- **Snapshots — config.json patcher handles both wrapped (cfg["config"]) and flat layouts (CH 51.1 uses flat)**
- **Snapshots — Tier A restore must use RestoreSeededLabel check BEFORE IsControlledBy in EnsureRootDiskClone (PR #21 lesson)**
- **CI — e2e tests must be wired into e2e-on-cluster.yaml workflow path-touch triggers (PR #22 lesson)**
- **Live Migration Phase 1 — direct PVC reuse (Approach A) ONLY, NOT snapshot+restore**
- **Live Migration Phase 1 — single combined client.MergeFrom for runPolicy + nodeName (split patches race the SwiftGuest reconciler)**
- **Live Migration Phase 1 — Preparing phase must explicitly Delete(pod), NOT just patch runPolicy: Stopped (stop guard is reactive only)**
- **Live Migration Phase 1 — Preparing phase dual-poll: Pod NotFound AND no VolumeAttachment for the per-guest PV (prevents Multi-Attach errors)**
- **Live Migration Phase 1 — annotation-as-source-of-truth idempotency: kubeswift.io/migration-in-progress on the SwiftGuest**
- **Live Migration Phase 1 — drive-forward post-cutover, restore source pre-cutover**
- **Live Migration Phase 1 — VFIO/SR-IOV cross-node migration unconditionally rejected (Phase 4+ work)**
- **Live Migration Phase 1 — direct pod.Spec.NodeName binding, NOT kubernetes.io/hostname selector**
- **Live Migration Phase 1 — operator-opt-in for IP change via spec.allowIPChange (default networking does not preserve IP cross-node)**
- **Live Migration Phase 1 — webhook uses per-operation discipline (ValidateCreate full / ValidateUpdate shape-only / ValidateDelete pass-through) — see PR #26**
- **Pattern: validation logic that fires on every operation needs to enumerate which operations it fires on. Default-to-everything is the bug pattern; default-to-explicit is the discipline (PR #26 lesson)**
- **Storage access mode (PR #32) — SwiftGuestClass.spec.storage + SwiftGuest.spec.storage select accessMode/volumeMode/storageClassName per-field. Default RWO+Filesystem. RWX+Block is the live-migration-capable combination (KubeVirt model)**
- **Storage access mode — CRD admission HARD rejects RWX+Filesystem via OpenAPI CEL XValidation. Filesystem RWX (Longhorn Generic, NFS-based) is not live-migration-capable; the rejection is at submit time so operators don't discover the gap at drain time**
- **Storage access mode — `liveMigrationCapable` is recomputed from the resolved spec at admission time (SwiftMigration webhook + swiftctl describe), NOT stored in status. Derived facts in status race controller-write-back during cluster restore; recompute eliminates the false-rejection hazard**
- **Storage access mode — Longhorn migratable-parameter check is a STATUS condition (StorageReady), NOT an admission gate. StorageClasses are cluster-admin resources; the controller surfaces the gap and reconciles to ready when fixed**
- **Storage access mode — per-field merge: SwiftGuest.spec.storage overrides SwiftGuestClass.spec.storage one field at a time. Empty/zero fields fall through. *string for storageClassName distinguishes nil ("fall through") from "" ("explicit cluster default") — both currently resolve to empty but the distinction is preserved for forward compat**
