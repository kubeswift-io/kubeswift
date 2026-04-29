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

---

## Phase 2 Decisions Pending (live migration)

When PR #26 is deployed, Phase 2 of live migration design conversation begins. Pending decisions:

1. **swiftletd control surface for migration actions** — annotation-driven (matches existing patterns including snapshot Phase 2's pattern) vs HTTP. Default recommendation: annotation-driven, but heaviest swiftletd extension yet so worth confirming.

2. **mTLS posture for Phase 2** — Phase 2 is plumbing in isolation, no controller integration, no production migration traffic yet. Manual demonstration uses plaintext TCP. mTLS is Phase 3 territory.

3. **Same-CH-version constraint** — operationally consequential for upgrade workflow. Phase 2 spike must verify against deployed CH version and document upgrade-discipline implications.

4. **Pre-copy convergence test surface** — pre-copy migration's whole shape needs memory-dirtying workload, not static VM. Spike should validate convergence on typical workload before Phase 2 commits to specific timing assumptions.

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

**1. Live Migration Phase 2 — swiftletd live migration plumbing**
- Extend swift-ch-client with send-migration / receive-migration API methods
- Destination "awaiting migration" launcher pod mode
- Annotation-driven control surface for migration actions
- Progress reporting via pod annotations
- Manual demonstration on real cluster (no controller integration yet — that's Phase 3)
- Estimated 7-10 days
- See "Phase 2 Decisions Pending" section above

**2. Live Migration Phase 3 — live mode + mTLS**
- SwiftMigration controller gains live mode
- mTLS sidecar for migration channel
- Pre-copy convergence handling

**3. Live Migration Phase 4 — drain integration via eviction webhook**
- `kubectl drain` triggers migration automatically
- Independent value: drain integration with offline migration alone dramatically improves operator UX
- Could jump sequence if operator demand for safe drain dominates

**4. Live Migration Phase 5 — operational polish**
- Prometheus metrics, dashboards, retention

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
