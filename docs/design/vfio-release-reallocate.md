# VFIO/GPU Release-and-Reallocate — Design

> Status: design-locked-pending-review (2026-06-02). The Phase 4 drain
> follow-on sub-phase (TFU #27): offline migration for VFIO/GPU guests, so
> they can be evacuated off a node on drain.
>
> Decisions resolved: **migration controller orchestrates** (SwiftGPU exposes
> reserve/release primitives); validated **spike-first** on the real GTX 1080
> ([`vfio-release-reallocate-spike.md`](vfio-release-reallocate-spike.md) —
> the dealloc->realloc choreography PASSES same-node).

## 1. Goal & non-goals

**Goal.** A VFIO/GPU guest (gpuProfileRef, or any sriov interface) can be
**offline-migrated** between nodes: its GPUs are released on the source node
and reallocated on the target node, the guest is recreated on the target
acquiring the same PVC, and the SwiftMigration / drain controller drives it.
This lifts the unconditional VFIO migration rejection **for offline mode**.

**Non-goals.**
- **No live migration of VFIO guests** — Cloud Hypervisor cannot live-migrate
  a guest with a VFIO device (the source device state cannot be transferred).
  VFIO is offline-only; `drainPolicy: LiveMigrate` on a VFIO guest still
  blocks the drain.
- **No new transport** — reuses the Phase 1 offline path (direct PVC reuse).
- **No GPU live-migration research, no SR-IOV NIC handling beyond rejection
  parity** (sriov NICs are also VFIO; same offline path applies, but NIC
  reattach on the target is out of scope for this sub-phase — GPU first).

## 2. Spike grounding (what we know from hardware)

- The dealloc->realloc choreography works: delete guest -> finalizer frees the
  GPU (~3s) -> a fresh guest reacquires the same device and boots.
- The GPU stays vfio-pci-bound across the release->reacquire window (CH closes
  the device on exit; gpu-init is idempotent). So the realloc'd pod's gpu-init
  is a fast no-op on the bind.
- **Node vfio-readiness is a hard prerequisite** — `vfio-pci` must be loaded on
  the target node, else gpu-init fails. This MUST be part of GPU target
  selection, not discovered at gpu-init time.
- One GPU node on the validation cluster: **cross-node cannot be
  hardware-validated**. Same-node `boba->boba` exercises the choreography;
  cross-node target-selection + FM handoff are unit/envtest with a mocked 2nd
  `SwiftGPUNode`.

## 3. The blocker today

Two mechanisms prevent moving a GPU guest:

1. **Auto-pick allocation.** `findAndAllocate` (allocate.go) grabs the *first*
   `SwiftGPUNode` with matching free capacity and sets
   `status.GPU.NodeName`. No way to request a *specific* node.
2. **Precedence rule.** The GPU pod builder
   ([`gpu.go`](../../internal/controller/swiftguest/gpu.go)) hard-**rejects**
   `spec.NodeName != status.GPU.NodeName`. `status.GPU.NodeName` is the source
   of truth for where the pod runs.

Plus the SwiftMigration validating webhook unconditionally rejects all VFIO
migration.

## 4. Architecture — migration orchestrates, SwiftGPU primitives

The SwiftMigration controller already owns the offline stop/recreate sequence
and the drive-forward-post-cutover / restore-pre-cutover failure model. The
**reserve-target-before-stopping-source** atomicity must live where that
sequencing already is — so the migration controller orchestrates, and SwiftGPU
exposes two new primitives it calls.

### 4.1 New SwiftGPU primitives (exported from `internal/controller/swiftgpu`)

```
// ReserveOnNode marks the matching free GPUs (+ FM partition for shared mode)
// on a SPECIFIC node as allocatedTo the guest, WITHOUT touching
// status.GPU. Returns the selected devices / NUMA / partition for the caller
// to stamp into status at cutover. Fails if the node lacks free matching GPUs
// or is not vfio-ready. Idempotent (re-reserve returns the existing reservation).
func ReserveOnNode(ctx, c, guest, profile, nodeName) (devices, numaNodes, partitionID, error)

// ReleaseFromNode clears the guest's allocatedTo (+ FM partition) on a
// SPECIFIC node's SwiftGPUNode. Idempotent; no-op if nothing is allocated
// there. (deallocateGPUs becomes a thin wrapper: ReleaseFromNode(status.GPU.NodeName).)
func ReleaseFromNode(ctx, c, guest, nodeName) error
```

**Why no CRD change for the reservation:** the reservation reuses the existing
`GPUDevice.AllocatedTo`. During the window the guest legitimately holds GPUs
on **both** the source (status.GPU.NodeName=S, the running pod) and the target
(reserved). This double-hold is benign:
- The pod builder + SwiftGPU controller key on `status.GPU` (still S) — the
  source pod is unaffected; the SwiftGPU controller only allocates when
  `status.GPU` is nil, so it stays idle.
- `findAndAllocate` for *other* guests sees the target GPUs as `AllocatedTo`
  this guest — so no one else grabs them. The reservation holds.

### 4.2 Node vfio-readiness

`SwiftGPUNode.status` gains a `vfioReady` boolean (PR 1, shipped):
- gpu-discovery **detects and reports** vfio-readiness — a read-only check
  that `/sys/bus/pci/drivers/vfio-pci` exists (module loaded). It does **NOT**
  load the module: the gpu-discovery DaemonSet runs with `privileged: false`,
  `drop: ALL`, and a read-only `/sys` (verified — an earlier design draft
  wrongly assumed it was privileged), so it has neither `CAP_SYS_MODULE` nor a
  writable sysfs / `/lib/modules`.
- **Loading `vfio-pci` is a host responsibility** — `/etc/modules-load.d/
  vfio.conf` (persistent) or `modprobe vfio-pci` (transient). Documented as a
  GPU-node prerequisite. A future opt-in privileged loader (a separate
  minimal, dedicated DaemonSet, NOT gpu-discovery) could automate it if
  operators want it; out of scope for this sub-phase.
- `ReserveOnNode` and the GPU target pre-flight **refuse a node that is not
  vfioReady**, turning a silent gpu-init `Init:Error` into a clear, early
  rejection — this is the value the surface delivers regardless of who loads
  the module.

### 4.3 GPU target pre-flight (a GPU analogue of `NodeHasCapacity`)

Exported `GPUNodeHasCapacity(ctx, c, nodeName, profile) error`: the target
`SwiftGPUNode` is `vfioReady` AND has `>= profile.Count` free GPUs matching
`model` / `tier` / (NUMA where the profile requires it) / an available FM
partition for shared mode. Used in:
- the **drain controller** target selection (so a VFIO guest only targets a
  node that can actually host it — the GPU analogue of the existing
  `NodeHasCapacity` filter); and
- the SwiftMigration **Validating** phase (authoritative gate).

## 5. The offline GPU migration sequence

Driven by the migration controller for a VFIO guest in offline mode. Reuses
the Phase 1 offline phases, inserting GPU steps.

| Phase | Non-GPU (Phase 1) | + GPU steps |
|---|---|---|
| **Validating** | node Ready/schedulable, CPU/mem capacity | **+ `GPUNodeHasCapacity(T)`** (vfioReady + free matching GPUs). Resolve mode=offline (VFIO never live). |
| **Preparing** | patch runPolicy=Stopped + nodeName=T, Delete src pod, wait pod gone + PV detached | **FIRST `ReserveOnNode(T)`** (reserve target GPUs while the source still runs). THEN the existing stop. Reserve-before-stop is the atomicity guarantee. |
| **StopAndCopy** (cutover) | patch spec.nodeName=T, runPolicy=Running | **`ReleaseFromNode(S)`**, then stamp `status.GPU` = {NodeName:T, Devices:T-devices, NUMANodes, PartitionID} (from the reservation), then the existing spec patch. The SwiftGuest controller recreates the pod on T; gpu-init binds T's (already-reserved) GPUs. |
| **Resuming** | wait GuestRunning on T | unchanged (gpu-init + CH boot with T's GPUs). |
| **Failed (pre-cutover)** | restore source (runPolicy=Running on S) | **+ `ReleaseFromNode(T)`** (drop the reservation), source restarts on S with S's GPUs intact. |

Ordering invariant (the spike's lesson): **reserve T before stopping S; release
S only at cutover; release T on any pre-cutover failure.** A failed reserve
never stops the source, so the source is never stranded GPU-less.

### 5.1 status.GPU ownership

The migration controller patches `status.GPU` at cutover. Safe because: the
SwiftGPU controller only *writes* `status.GPU` when it is nil (allocation),
and `deallocateGPUs` *reads* `status.GPU.NodeName` (=T post-cutover) on guest
delete — both consistent with the migration controller's commit. A
`migration-in-progress`-style guard is not needed for GPU (the SwiftGPU
controller is structurally idle while `status.GPU` is non-nil), but the
migration's failure/cleanup path MUST `ReleaseFromNode` both nodes if the
guest is deleted mid-migration (else the target reservation leaks — open
question §10).

## 6. Precedence-rule change (gpu.go)

Today: reject `spec.NodeName != status.GPU.NodeName`. After: the orchestrated
cutover patches **both** `spec.NodeName=T` and `status.GPU.NodeName=T`
together, so at pod-build time they agree. The rule stays as a **consistency
assertion** ("they must agree when the pod is built") — it just no longer
implies "GPU guests can never move", because the migration commits both
atomically. **LOAD-BEARING (W26-class):** a future refactor must not weaken
this to "trust spec.NodeName alone" — `status.GPU.NodeName` must remain the
binding source for which GPUs gpu-init binds.

## 7. Webhook change

Lift the unconditional VFIO rejection
([`validator.go`](../../internal/webhook/swiftmigration/validator.go)) **for
offline mode only**:
- `mode=offline` (or `auto` resolving to offline) + VFIO -> **allow** (subject
  to the GPU target pre-flight in Validating).
- `mode=live` + VFIO -> **still reject** (CH cannot live-migrate VFIO).
The drain controller already resolves VFIO `drainPolicy: Migrate` to offline;
`LiveMigrate` + VFIO stays blocked at the eviction webhook (no change there).

## 8. FM partition handoff (Tier 2/3) — design only, unvalidatable here

For shared-partition GPUs (tier hgx-shared): the source FM partition is
deactivated and a target partition activated. `ReserveOnNode` selects a free
FM partition on T (existing `findFMPartition`); the gpu-init on T activates it
(existing `GPU_PARTITION_ID` path); `ReleaseFromNode(S)` frees the source
partition. **No HGX hardware** — this path is unit/envtest only with mocked FM
status, shipped explicitly labeled. Tier 1 (the validatable case) has no FM.

## 9. Validation strategy (one GPU node)

**Actual approach used (PR 5 walkthrough, 2026-06-03).** Same-node `boba->boba`
was NOT viable — the SwiftMigration webhook rejects same-node migration
(`validator.go`: "same-node migration is meaningless", open question §10.4). So
the orchestration was validated against a **mock second SwiftGPUNode**: a real
GTX-1080 guest runs on boba, a fake `SwiftGPUNode/miles` (vfioReady, one free
GPU at a **non-existent PCI `0000:ff:00.0`** so the dst gpu-init fails
harmlessly without touching any real miles device) is the migration target, and
`kubectl drain boba` (scoped to the guest pod) drives the whole chain on the
real apiserver. The dst cannot *boot* (no real GPU on miles) — that is the
**validation ceiling on a single GPU node** — but every control-plane step is
exercised end-to-end, and the migration must reach an HONEST terminal state
(Failed: "destination guest failed to boot"), never a false success.

**Validated end-to-end on the cluster** (image sha-fafc2c9): eviction webhook
marks the GPU guest -> drain controller selects the GPU target via
`GPUNodeHasCapacity` -> migration resolves `auto->offline` -> Preparing reserves
the target (benign double-hold: both nodes `allocatedTo` the guest) -> cutover
releases the source + stamps `status.GPU=target` -> Resuming detects the dst
init failure -> **Failed** with `destination guest failed to boot on "miles":
init container "gpu-init" exited 1`. Final state consistent: `status.GPU=miles`,
boba freed, no `status.GPU` flip-back. `node/boba drained` (exit 0).

**Cross-node (mocked):** unit/envtest with a second mocked `SwiftGPUNode` for
target-selection, pre-flight, reservation-holds-against-other-guests, the
reserve/cutover/release primitives, and the dst-pod-state Resuming gate.

Ship explicitly labeled **"cross-node GPU migration not hardware-validated
(needs a 2nd real GPU node)"** — the dst *boot* + a `Completed` migration
require two real GPU nodes.

### 9.1 Bugs the walkthrough surfaced (all fixed; the W5 pattern)

Three real multi-controller / lifecycle bugs that all unit tests missed (fake
clients, no concurrent controllers, dst always "boots") — each fix revealed the
next (finding-behind-a-finding):

| # | Bug | Fix (PR) |
|---|---|---|
| **W-GPU-1** | The live SwiftGPU controller re-stamps `status.GPU` on every reconcile via `findAndAllocate`, whose first-pass returned the FIRST allocated node. During the reserve double-hold it re-stamped the source, racing the migration. (The §5.1 "SwiftGPU is idle while status.GPU is non-nil" assumption was FALSE.) | `findAndAllocate` prefers the node `status.GPU` already references (#100) |
| **W-GPU-2** | Offline StopAndCopy checked `pod.Spec.NodeName != target` — but GPU pods pin via a `kubernetes.io/hostname` nodeSelector, so the scheduler fills `spec.NodeName` a moment later. The eager check false-failed "atomicity invariant violated". | Empty `spec.NodeName` = "not yet scheduled" (requeue), unless a nodeSelector pins it away from the target (#101) |
| **W-GPU-3** | Offline Resuming concluded `Completed` off `GuestRunning=True` + IP, which SURVIVE the cutover pod swap (stale source values). A dst that never boots produced a **false success**. Latent in Phase-1 offline migration generally. | Gate completion on the dst pod's real state: fail on a terminal init failure, require the launcher Ready before trusting GuestRunning/IP (#102) |

## 10. Open questions (resolve during implementation)

1. **Reservation leak on guest-delete-mid-migration — RESOLVED.** If the guest
   was deleted while a target reservation was held (status.GPU=S),
   `deallocateGPUs` freed only S and T's reservation leaked (a GPU stranded
   AllocatedTo a now-deleted guest). Fixed: `deallocateGPUs` now lists **all**
   SwiftGPUNodes and `ReleaseFromNode`s each (idempotent), so it frees both the
   source allocation and any held target reservation — robust even if the
   SwiftMigration object is also gone. Regression test: double-hold
   (source+target) delete frees both nodes.
2. **vfioReady representation.** A `SwiftGPUNode.status.conditions[vfioReady]`
   vs a boolean `status.vfioReady`. Conditions are the kube-idiomatic choice;
   confirm with the discovery DaemonSet's existing status shape.
3. **modprobe — RESOLVED (PR 1): only-report.** gpu-discovery is minimal-cap
   (drop ALL, read-only /sys) and cannot modprobe; it detects + reports
   vfioReady. Loading `vfio-pci` is a host responsibility. A future opt-in
   privileged loader (separate DaemonSet) is the path if auto-load is wanted.
4. **Same-node migration semantics.** `boba->boba` is degenerate (source ==
   target). The migration webhook today rejects same-node; the validation path
   needs a test-only allowance OR the design treats `boba->boba` as a
   release+reacquire test harness rather than a real SwiftMigration. Decide
   before PR-validation.
5. **Reservation timeout.** A reservation held by a wedged migration blocks the
   target GPUs for other guests. Bound by `spec.timeout` (the migration's
   runaway gate) + the abort-release in (1).

## 11. Implementation plan (PRs) — SHIPPED

1. **PR 1 (#96) — DONE.** `SwiftGPUNode.status.vfioReady` surface + gpu-discovery
   read-only vfio-pci detection (modprobe descoped — minimal-cap DaemonSet,
   §4.2) + `GPUNodeHasCapacity` pre-flight. Cluster-validated `boba vfioReady=true`.
2. **PR 2 (#97) — DONE.** `ReserveOnNode` / `ReleaseFromNode` primitives +
   `deallocateGPUs` refactor. Unit-tested incl. reservation-holds-against-others.
3. **PR 3 (#98) — DONE.** Migration offline-GPU sequence (pre-flight,
   reserve-before-stop, cutover release+stamp, failure release) + precedence-rule
   reframe + webhook VFIO-offline lift (SR-IOV stays rejected).
4. **PR 4 (#99) — DONE.** Drain controller GPU wiring (VFIO `Migrate` -> offline
   migration; SR-IOV stays manual; GPU target selection via `GPUNodeHasCapacity`).
5. **PR 5 (this doc + #100/#101/#102) — DONE.** Cluster walkthrough
   (mock-2nd-GPU-node + drain boba; §9) + the three bug fixes it surfaced
   (W-GPU-1/2/3) + operator runbook (`docs/migration/phase-4.md` GPU drain + the
   vfio-pci prerequisite) + this design-doc validation update.

**Remaining (tracked, not blocking):** open questions §10.1 (reservation leak on
guest-delete-mid-migration) and §10.5 (reservation timeout) are not yet
exercised; cross-node dst *boot* (`Completed`) needs a second real GPU node.
