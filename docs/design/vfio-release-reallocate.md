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

`SwiftGPUNode.status` gains a `vfioReady` condition (or boolean), set by the
gpu-discovery DaemonSet (already privileged, already per-GPU-node):
- gpu-discovery checks `/sys/bus/pci/drivers/vfio-pci` exists (module loaded)
  and, if missing, **`modprobe vfio-pci`** (it has the privilege; gpu-init
  does not — minimal caps). Surfaces the result on `SwiftGPUNode.status`.
- `ReserveOnNode` and the GPU target pre-flight **refuse a node that is not
  vfioReady**, turning a silent gpu-init `Init:Error` into a clear, early
  rejection.
- Operators should also load `vfio-pci` persistently
  (`/etc/modules-load.d/vfio.conf`); the modprobe is a safety net, documented
  as such.

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

- **Same-node `boba->boba` (real GTX 1080):** a degenerate offline migration
  that release->reacquires the real GPU exercises ReserveOnNode / ReleaseFromNode
  / the cutover status stamp / gpu-init rebind end-to-end. The spike already
  proved the raw choreography; the PR-level test confirms the *orchestrated*
  sequence.
- **Cross-node (mocked):** unit/envtest with a second mocked `SwiftGPUNode`
  for target-selection, pre-flight, reservation-holds-against-other-guests,
  and the failure/restore path.
- Ship explicitly labeled **"cross-node GPU migration not hardware-validated
  (needs a 2nd GPU node)."**

## 10. Open questions (resolve during implementation)

1. **Reservation leak on guest-delete-mid-migration.** If the guest is deleted
   while a target reservation is held (status.GPU=S), `deallocateGPUs` frees
   only S; T's reservation leaks. Fix: the SwiftMigration finalizer/cleanup
   `ReleaseFromNode(T)` on abort; OR a periodic reconcile that GCs reservations
   whose owning migration is gone. Lean toward the former (explicit).
2. **vfioReady representation.** A `SwiftGPUNode.status.conditions[vfioReady]`
   vs a boolean `status.vfioReady`. Conditions are the kube-idiomatic choice;
   confirm with the discovery DaemonSet's existing status shape.
3. **modprobe in gpu-discovery: opt-in?** Auto-modprobe vs only-report. Lean
   auto (it is the safety net) but gate behind a discovery flag if operators
   want host config to be authoritative.
4. **Same-node migration semantics.** `boba->boba` is degenerate (source ==
   target). The migration webhook today rejects same-node; the validation path
   needs a test-only allowance OR the design treats `boba->boba` as a
   release+reacquire test harness rather than a real SwiftMigration. Decide
   before PR-validation.
5. **Reservation timeout.** A reservation held by a wedged migration blocks the
   target GPUs for other guests. Bound by `spec.timeout` (the migration's
   runaway gate) + the abort-release in (1).

## 11. Implementation plan (PRs)

1. **PR 1** — this design doc + the `SwiftGPUNode.status` vfioReady surface +
   gpu-discovery `vfio-pci` check/modprobe + a `GPUNodeHasCapacity` pre-flight
   helper (no migration wiring yet). Cluster-validate vfioReady on boba.
2. **PR 2** — `ReserveOnNode` / `ReleaseFromNode` primitives + `deallocateGPUs`
   refactor to `ReleaseFromNode`. Unit-tested (mock SwiftGPUNodes), including
   reservation-holds-against-other-guests.
3. **PR 3** — migration controller offline-GPU sequence (Validating pre-flight,
   Preparing reserve-before-stop, cutover release+status-stamp, failure
   release) + the precedence-rule change + lift the webhook VFIO-offline
   rejection. Unit-tested.
4. **PR 4** — drain controller: VFIO guests under `drainPolicy: Migrate` now
   get a marker + an offline migration (remove the PR-4a "VFIO denied" path
   for Migrate; keep LiveMigrate+VFIO blocked). GPU target selection uses
   `GPUNodeHasCapacity`.
5. **PR 5** — cluster walkthrough (same-node `boba->boba` release->reacquire
   via the orchestrated path; mocked cross-node in tests) + operator runbook
   (GPU drain + the vfio-pci prerequisite) + design-doc updates.
