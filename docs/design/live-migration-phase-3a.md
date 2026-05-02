# Live Migration Phase 3a — SwiftMigration Controller `mode: live`

> **Status:** Draft. Sections 1, 2, 4 land first (Day 1, F1.5-independent).
> Sections 3, 5, 6 land Day 2 (Option 2 + (P) settled). Sections 7, 8 land
> Day 3. Sub-agent reviews + final pass Days 4-5. Last updated:
> 2026-05-01.

---

## 0. How to read this document

Phase 3a ships the `SwiftMigration` controller's `mode: live` path. The
controller drives the Phase 2 swiftletd plumbing (PRs #28/#29/#31) across
two pods to move a running guest's CPU + memory state between nodes
without a cold-boot.

This document is the contract for Phase 3a implementation. A reader of
this doc plus the spike doc
([`live-migration-phase-3a-spike.md`](live-migration-phase-3a-spike.md))
should be able to begin implementation without rediscovering any spike
finding or making any architectural decision the design has committed
to.

The doc carries the same 8-section shape as
[`live-migration-phase-2.md`](live-migration-phase-2.md). Phase 1's
[`live-migration.md`](live-migration.md) is the existing offline-
migration controller's behavior that Phase 3a extends — read it first
if you haven't.

### Notation glossary

The doc uses several short codes for cross-references. Reading them
without round-tripping to other docs:

| Code | Meaning | Source |
|---|---|---|
| `F1.5`, `F2.4`, `F4.2`, etc. | Spike finding (numbered F&lt;Q&gt;.&lt;n&gt;) | `live-migration-phase-3a-spike.md` |
| `S1`, `S2`, `S3` | Security finding | Phase 2 spike + `THREAT-MODEL.md` |
| `W3`, `W7`, `W8`, `W11`, etc. | Walkthrough finding | `kubeswift_context.md` |
| `OQ` | Spike open question (not a finding; a deferred decision) | spike doc |
| `G` | Phase 1 spike open question on leader-handover | Phase 1 spike doc |
| `MH-C<N>`, `MH-G<N>`, `MH-W<N>`, `MH-S<N>`, `MH-R<N>`, `MH-N<N>` | Must-have-before-ship checklist item (§8.1) — Controller / SwiftGuest / Webhook / swiftctl / Rust / Pre-existing | this doc §8.1 |
| `D1`, `D2`, `D3` | Phase 3a swiftletd dependency (§7.2) | this doc §7.2 |
| `E1`-`E17` (incl. `E2a`/`E2b`/`E2c`, `E12-walk`) | Cluster-e2e test scenario (§8.2) | this doc §8.2 |
| `B0` | Spike-blocker (br0/Calico CIDR collision; PR #39, merged) | spike doc, this doc §8.1 MH-N1 |

---

## 1. Goal and Non-goals

### Goal

Phase 3a ships the SwiftMigration controller's `mode: live` path for
**two workload classes the spike validated**:

1. **Kernel-boot guests** (`spec.kernelRef`). The Phase 3a spike
   validated end-to-end live migration of a 4Gi kernel-boot
   `faas-minimal` guest cross-node in ~38s (swiftletd-reported
   migration body) on the deployed cluster (miles + boba, CH v51.1,
   Longhorn). No storage handoff required because the guest's root
   filesystem is the initramfs.
2. **RWX+Block disk-boot guests** (`spec.imageRef` with
   `spec.storage.{accessMode: ReadWriteMany, volumeMode: Block,
   storageClassName: longhorn-migratable}`). The runtime path was
   shipped via PR #35 (W9). Live migration over RWX+Block storage
   does not require a controller-side storage handoff: the same
   PVC is mounted RWX on both pods concurrently, and the guest
   transfers state via swiftletd's TCP migration channel rather
   than via the storage layer. F2 split-brain risk is documented
   and handled at the storage-level invariant Phase 3a inherits
   from PR #35; the design treatment of that risk lives in
   Phase 3c.

The Phase 3a controller observes both pods exclusively via the
apiserver/informer surface (F2.4). No cross-pod TCP from the
controller-manager pod is required or used.

The state machine drives the four migration transitions
(Validating → Preparing → StopAndCopy → Resuming → Completed)
**entirely via the Phase 2 annotation surface**. No new annotations
are required; no new CRD fields beyond a small additive set
(Section 6).

### Non-goals

Each of the following is out of scope and shipped (or deferred) by a
separate phase. The design doc references each so future readers
find the right home for follow-up work.

| Out-of-scope item | Home phase | Spike finding |
|---|---|---|
| Migration channel mTLS | Phase 3b | spike §2 + F2.4 |
| `S1` URL-from-CR (deprecate annotation-as-URL-input) | Phase 3b | spike S1 |
| CPU-feature pre-flight check (CPUID compatibility) | Phase 3b | spike F12 (carried from Phase 2 spike) |
| Audit logging schema | Phase 3b | spike OQ7 (Phase 2) |
| F2 split-brain on RWX live migration | Phase 3c | W9.x + PR #35 |
| RWO+disk-boot live migration (storage handoff) | Phase 3c | spike storage-targeting statement |
| Drain-aware migration (kubectl drain → auto-migrate) | Phase 4 | spike F4.4 |
| Pre-copy convergence tuning (`spec.maxPauseWindow` enforcement) | Phase 5 | Phase 2 spike F6/F7 |
| swiftletd progress annotations (`precopy`/`stopcopy`) | Phase 5 | F2.5 |
| F1.1 dst-side terminal-value rename | Future swiftletd cleanup | F1.1 — see §7.3 |

**F1.1 is explicitly NOT a Phase 3a dependency** (§7.3). The controller's
Resuming → Completed gate uses src-side `migration-status=complete`
(F1.2), which is unaffected by F1.1's dst-side ambiguity.

### Phase 3a explicitly INCLUDES

These items are spike findings the controller MUST handle and that
Phase 3a is the right home for:

- F1.5 — dst pod ownership: SwiftGuest owns the destination pod
  from creation (Option 2); SwiftGuest's reconcile resolves the
  canonical pod via `status.podRef.name` with fallback to
  `guest.Name` (the (P) indirection added by Phase 3a). Full
  rationale §3.1.
- F1.6 — plaintext-ack annotation lifecycle: ack annotation
  (`kubeswift.io/migration-phase2-unsafe-plaintext: ack`) is set
  by the controller at dst pod creation time (Phase 2 PR-B's gate
  pattern). Phase 3a inherits without modification.
- F1.8 — `send_id` derivation pattern for idempotent retry across
  controller leader-handover. Design in §2.
- F2.4 — controller observes both pods via informer alone; no cross-
  pod connectivity. Design in §5.
- F4.2 — `src.UID` change as failure-detection signal. Design in §4.
- F4.3 — `status.failureReason` enum (5 values). Schema in §6.
- F4.4 — no PDB on dst pod (Phase 4 webhook handles
  drain-mid-migration). Stated in §3.7.

### Phase 3a operator-visible behavior change

**After a successful live migration, the canonical pod's name no
longer equals the SwiftGuest name.** A pre-migration `kubeswift-faas`
SwiftGuest had a launcher pod also named `kubeswift-faas`. After live
migration, the canonical pod is the destination pod (created during
migration with a different name, e.g., `kubeswift-faas-mig-<id>`),
and `SwiftGuest.status.podRef.name` reflects that name.

Phase 3a updates `swiftctl logs/console/ssh` subcommands to resolve
via `status.podRef.name` (with fallback to `guest.Name`) — closing
the `kubectl logs <guest-name>` foot-gun. Operators using `swiftctl`
see no surface change. Operators using `kubectl` directly must read
`status.podRef.name` from `kubectl describe swiftguest` or use
`-l swift.kubeswift.io/guest=<name>` label selectors.

Phase 1 offline migration is **unaffected** — offline mode reuses the
same pod name (`guest.Name`) post-migration. Only `mode: live` shifts
the canonical pod name.

---

## 2. Controller state machine

### 2.1 Phase enum

Phase 3a reuses the existing `SwiftMigrationPhase` enum from the
Phase 1 CRD without addition or removal:

```go
SwiftMigrationPhasePending     // initial
SwiftMigrationPhaseValidating  // compatibility + capacity checks
SwiftMigrationPhasePreparing   // create destination pod, await Ready
SwiftMigrationPhaseStopAndCopy // issue recv+send, observe migration body, cutover
SwiftMigrationPhaseResuming    // post-cutover; wait for guest health on destination
SwiftMigrationPhaseCompleted   // terminal success
SwiftMigrationPhaseFailed      // terminal failure (with status.failureReason)
SwiftMigrationPhaseCancelled   // terminal cancel
```

The CRD comment on `SwiftMigrationPhase` already noted "forward
compatibility with Phase 3's PreCopy addition." **Phase 3a does NOT
add a PreCopy phase.** Pre-copy iterations are entirely internal to
swiftletd's `vm.send-migration` dispatch; they are not observable as
a controller-level phase. Pre-copy convergence visibility is a
Phase 5 concern (F2.5).

Per-live-mode sub-state is tracked via `status.phaseDetail` (Phase 1
already has this field; Phase 3a adds new value vocabulary — Section
6).

### 2.2 State transition diagram

```
                ┌─────────┐
                │ Pending │
                └────┬────┘
                     │
                     ▼
              ┌────────────┐         (validation fails: mode incompatible,
              │ Validating │────────► node not ready, capacity short,         ─┐
              └─────┬──────┘          per-source-node concurrency violation)  │
                    │                                                          │
                    ▼                                                          │
              ┌──────────┐            (dst pod creation/ready failure,         │
              │ Preparing│──────────► node uncordoned check failed)           ─┤
              └─────┬────┘                                                     │
                    │                                                          │
                    ▼                                                          │
            ┌──────────────┐                                                   ▼
            │ StopAndCopy  │                                              ┌────────┐
            │              │                                              │ Failed │
            │  sub-states: │                                              └────────┘
            │  • recv-issued                                                  ▲
            │  • recv-accepted                                                │
            │  • send-issued                                                  │
            │  • send-in-flight (transfer body)                               │
            │  • src-completed                                                │
            │  • cutover (podRef swap + src delete)                           │
            └──────┬───────┘                                                  │
                   │                                                          │
                   ▼                                                          │
              ┌──────────┐            (dst pod terminated post-cutover,       │
              │ Resuming │──────────► dst guest never reaches Running,       ─┤
              └─────┬────┘            spec.timeout exceeded)                  │
                    │                                                          │
                    ▼                                                          │
              ┌──────────┐                                                    │
              │Completed │                                                    │
              └──────────┘                                                    │
                                                                              │
            (operator sets spec.cancelRequested at any point)            ─────┤
                                                                              │
                                                                              ▼
                                                                        ┌──────────┐
                                                                        │Cancelled │
                                                                        └──────────┘
```

The terminal three (Completed / Failed / Cancelled) are absorbing
states — the controller treats them as no-ops (no further reconcile
work; status subresource is frozen). This is the **per-operation
discipline** PR #26 established for Phase 1 and Phase 3a inherits.

### 2.3 State-by-state contract

For each state: entry conditions, exit conditions, observable events
the controller acts on, idempotency primitive, and the spike finding
that determined the transition logic.

#### Pending → Validating

**Entry**: SwiftMigration created (apiserver Watch event).

**Exit**: First reconcile after Pending → unconditional transition
to Validating. Set `status.startedAt`.

**Idempotency**: phase-stamp guards re-entry; same as Phase 1
pattern.

#### Validating

**Entry conditions**: `phase=Validating`. Status fields populated
from spec by ValidateCreate webhook; controller re-runs cluster-state
checks (defense in depth; cluster state may have shifted since
admission).

**Checks**:

1. **Source SwiftGuest exists and is in `Running` phase.** If
   source is missing or not Running, transition to Failed with
   `failureReason: SourcePodReplaced` (the most likely cause is
   the source guest restarted/migrated since admission).
2. **Destination node is Ready and not cordoned.** Read
   `Node.status.conditions` and `Node.spec.unschedulable` via
   informer. If not ready, transition to Failed with
   `failureReason: Other` and `failureMessage: "destination node
   not ready"`.
3. **Destination node has capacity.** Manual capacity check (the
   Phase 1 spike's resolved decision: read
   `node.status.allocatable`, list pods on that node, sum CPU+
   memory requests, compare with the SwiftGuest's CPU+memory).
   No server dry-run, no real-pod-probe (spike Q2 / Phase 1 spike
   findings).
4. **Migration source-side concurrency**: no other
   SwiftMigration with the same `spec.guestRef` is in a non-
   terminal phase. (This is the per-source-guest mutex; the
   per-source-node mutex below is broader.)
5. **Per-source-node concurrency** (Phase 3a addition; spike's
   "Open questions" closure): no other SwiftMigration with the
   same `status.sourceNode` is in a non-terminal phase. **Reject
   at the validating webhook** (Section 5), not at runtime; the
   webhook check is the cleaner surface. Validating phase is
   defense-in-depth.

**Exit**: All checks pass → transition to Preparing. Any check fails
→ transition to Failed.

**Spike finding**: Phase 1 spike Q2 (capacity check pattern); Phase
3a spike OQ multi-migration concurrency.

#### Preparing

**Entry conditions**: `phase=Preparing`. All Validating checks
passed.

**Actions**:

1. **Create destination launcher pod.** Owned by the SwiftGuest
   (`controllerutil.SetControllerReference(swiftguest, dstPod,
   scheme)`). Named `<guest>-mig-<short-uid>` where `<short-uid>`
   is the first 6 chars of SwiftMigration's UID — deterministic,
   unique per migration, fits Kubernetes' DNS-1123 64-char limit.
   Labeled with:
   - `swift.kubeswift.io/guest=<guest.Name>` (existing label)
   - `kubeswift.io/migration-role=destination` (new for Phase 3a)
   - `kubeswift.io/migration=<swiftmigration.Name>` (new; for
     informer indexing)
2. **Pin to destination node** via `pod.Spec.NodeName=<target>`
   (same pattern as Phase 1 offline). Bypasses scheduler.
3. **Set environment**: `KUBESWIFT_MIGRATION_ROLE=receiver` (so
   swiftletd starts in receiver mode — Phase 2 PR-C).
4. **Set ack annotation** at pod creation:
   `kubeswift.io/migration-phase2-unsafe-plaintext=ack` (F1.6 —
   ack lifecycle is flexible; pod-creation-time is the simplest).
5. **Wait for dst pod Ready** via informer event (`status.phase ==
   Running` && `Ready` condition True). Up to ~60s budget; if not
   Ready in 60s, transition to Failed with
   `failureReason: PodTerminated` and detail "destination pod
   never reached Ready".

**Idempotency**: the dst pod's deterministic name from
SwiftMigration's UID is the per-migration mutex. If the controller
crashes after Create and a new leader resumes Preparing, it observes
the existing dst pod and skips Create. Reconcile-driven, not
in-memory-state driven (G — spike's leader-handover open question).

**Sub-states tracked in `status.phaseDetail`**:
- `"creating destination pod"` — Create dispatched, awaiting
  apiserver ack
- `"waiting for destination pod ready"` — pod exists, awaiting
  Ready

**Exit**: dst pod Ready → transition to StopAndCopy. Failure modes
above → Failed.

**Spike findings**: F1.5 (Option 2 + P), F1.6 (ack lifecycle),
F4.4 (no PDB).

#### StopAndCopy

This is the load-bearing phase. It contains four ordered sub-states
plus the cutover sequence. The single SwiftMigration phase value is
`StopAndCopy` throughout; `status.phaseDetail` distinguishes
sub-states.

**Sub-states**:

| Sub-state | `phaseDetail` | Entry condition | Exit transition |
|---|---|---|---|
| `recv-issued` | `"issuing receive on destination"` | StopAndCopy entered | recv-action annotation written on dst pod with `$RECV_ID` |
| `recv-accepted` | `"destination receiving"` | Informer event: dst pod's `migration-status=running` annotation with matching `migration-status-id=$RECV_ID` (F1.3) | Proceed to send-issued |
| `send-issued` | `"issuing send on source"` | Above | send-action annotation written on src pod with `$SEND_ID` |
| `send-in-flight` | `"transferring guest state"` | send written | Wait for src `migration-status=complete` (success), `failed` (failure), OR `spec.timeout` (timeout) |
| `src-completed` | `"completing cutover"` | Informer event: src pod's `migration-status=complete` annotation with matching `migration-status-id=$SEND_ID` (F1.2) | Proceed to cutover |
| `cutover` | `"cutover: updating canonical pod"` | Above | Patch `SwiftGuest.status.podRef.name = <dst-pod-name>`, then delete src pod, then transition phase to Resuming |

**Idempotency primitives** (F1.8):

- **`$RECV_ID`** = `<swiftmigration.Name>:recv:<status.recvAttempts>`
  where `recvAttempts` starts at 0. Stored in
  `status.recvAttempts` so leader-handover preserves the counter.
- **`$SEND_ID`** = `<swiftmigration.Name>:send:<status.sendAttempts>`
  starting at 0. Stored in `status.sendAttempts`.

Re-issuing the same recv-action with the same `$RECV_ID` is a swiftletd-side
no-op (per Phase 2 PR-B's per-id idempotency). Re-issuing with a fresh ID
(incrementing the attempt counter) triggers fresh dispatch. Phase 3a
ONLY increments the attempt counter on explicit retry decisions —
not on every reconcile.

**`status.recvAttempts` and `status.sendAttempts` semantics**: each
counts dispatches issued, NOT successful completions. A successful
migration has both at 1. A migration with one failed retry has
both at 2. Operators reading `kubectl get smig` see this surface
as a hint that something retried.

**The W1 invariant** (F1.2 + F3.3): the controller's
`src-completed → cutover` transition gates on **src-side
`migration-status=complete` with matching `$SEND_ID`**, NOT on dst-
side observed annotations (which are ambiguous per F1.1). swiftletd-
on-src's `vm.send-migration` dispatch performs the W1 vm_info-on-dst
probe internally before writing terminal status (Phase 2 PR-B),
so observing src=complete implies dst CH is in `Running` state with
the migrated guest. Controller does NOT poll dst pod state directly.

**Defense-in-depth observation** (F3.3 belt-and-braces): the
controller may also observe `kubeswift.io/guest-ip` annotation
appearance on the dst pod (the post-resume DHCP discovery; Phase 1
already uses this signal). If src=complete but dst's `guest-ip`
annotation never appears within `spec.timeout`, transition to
Failed. This is informational and gives the controller a second
reading on whether the guest actually resumed correctly.

##### The cutover ordering invariant (LOAD-BEARING — restated in §3)

The cutover sub-state has a strict three-step sequence that MUST
fire in this order:

1. **Patch `SwiftGuest.status.podRef.name = <dst-pod-name>`**
   (single status subresource patch). After this patch, SwiftGuest
   controller's next reconcile resolves canonical pod = dst pod.
2. **Delete src pod** (single Delete call, default grace period).
3. **Patch SwiftMigration phase = Resuming**.

**The ordering is load-bearing.** If steps are reordered:

- **(2 before 1)**: SwiftGuest controller's reconcile (still
  pointing canonical pod at src) sees src pod NotFound, panics,
  tries to recreate `guest.Name` → conflict with the still-existing
  dst pod (different name) AND attempts to create a fresh pod that
  doesn't carry the migrated guest state. Worst-case data loss.
- **(3 before 1)**: SwiftMigration enters Resuming with podRef
  still pointing at src, which is still alive. SwiftGuest's
  canonical pod is wrong. Reconcile of Resuming triggers Health
  check on the wrong pod.

**Implementation**: the three steps must be issued from a single
reconcile invocation, in this exact order, with each step's
success-check before the next. If step 1 fails, retry step 1 only.
If step 1 succeeds and step 2 fails (e.g., apiserver transient
error), retry step 2 only — DO NOT undo step 1. If steps 1+2
succeed and step 3 fails (status patch on SwiftMigration), retry
step 3 only — DO NOT undo prior steps. This makes cutover a
forward-only sequence with retry-in-place.

This invariant is restated in §3 (Pod lifecycle) for belt-and-
suspenders documentation; the consequences of getting it wrong are
ugly enough to warrant duplication.

**Spike findings**: F1.2 (W1 gate), F1.3 (mirror-as-trigger),
F1.8 (send_id derivation), F3.3 (defense-in-depth observability).

#### Resuming

**Entry conditions**: `phase=Resuming`, cutover complete, src pod
deleted, `SwiftGuest.status.podRef.name = <dst-pod-name>`.

**Actions**:

1. **Wait for SwiftGuest's `GuestRunning=True` condition** on the
   dst pod. swiftletd-on-dst writes the GuestRunning condition via
   the same kube-rs DynamicObject path it uses on first boot
   (Phase 1 architecture, unchanged).
2. **Wait for `kubeswift.io/guest-ip` annotation** on dst pod
   (DHCP discovery completed). Optional belt-and-braces — Phase 3a
   gates on GuestRunning, with guest-ip surfaced in
   `status.targetIP` for operator visibility.

The choice of GuestRunning=True (vs. dst CH state=Running plus
guest-ip annotation) is a load-bearing design decision: live
migration's resume-vs-boot distinction means the guest does NOT
re-run cloud-init or re-DHCP on resume. The dst pod's `guest-ip`
annotation is therefore copied from the src pod's pre-migration
discovery, not freshly derived. **Section 3 elaborates the rationale
and the timeout-handling for guest-side delays.**

**Sub-states**:

| `phaseDetail` | Entry condition |
|---|---|
| `"waiting for guest health on destination"` | Resuming entered |
| `"destination guest healthy"` | GuestRunning=True observed |

**Exit**: GuestRunning=True observed → transition to Completed.

**Failure modes**:

- **dst pod terminated post-cutover** (any reason: K8s eviction,
  drain, OOM, node failure): `r.Get(podRef.name)` returns NotFound
  → transition to Failed with `failureReason: PodTerminated` and
  detail "destination pod terminated post-cutover".
- **`spec.timeout` exceeded since `status.startedAt`**: transition
  to Failed with `failureReason: Timeout`.
- **dst guest never reaches Running**: bounded by spec.timeout
  upper, but a more specific signal is the dst pod entering
  `Phase=Failed` (CH crashed). Treat as Failed with
  `failureReason: Other` and detail from
  `kubeswift.io/migration-status-detail`.

#### Completed

**Terminal**. Set `status.completedAt`. Set
`status.observedDowntime` and `status.observedPauseWindow`
(Section 6).

**Reconcile is a no-op** (per-operation discipline from PR #26).

#### Failed

**Terminal**. Set `status.completedAt`. `status.failureReason`
and `status.failureMessage` populated by the failing transition.

**Reconcile is a no-op**. **Cleanup is the controller's
responsibility** before transitioning to Failed:

| Failure point | dst pod | src pod | SwiftGuest.status.podRef |
|---|---|---|---|
| Pre-cutover (Validating, Preparing, StopAndCopy pre-cutover) | Delete | Untouched | Untouched (still points at src) |
| Cutover (during the 3-step sequence) | Untouched (it IS the canonical pod now) | Untouched (controller's retry-in-place finishes the sequence first) | Whatever the cutover's progress reached |
| Post-cutover (Resuming) | Untouched (it's the canonical pod) | Already deleted | Already updated to dst |

The pre-cutover failure case is the only one that explicitly deletes
a pod the controller created. Post-cutover failures leave the
canonical pod alone — the SwiftGuest's normal lifecycle (e.g., user
deletes the SwiftGuest) handles its eventual deletion.

#### Cancelled

**Terminal**. Operator-initiated via `spec.cancelRequested=true`
(Phase 3a's cancel mechanism — cancel discipline detail below).

**Cancel discipline (F3.4)** — **gating note**: the steps below
fire ONLY in pre-cutover phases; §5.3's `honorCancel` settles the
pre-vs-post-cutover question before this code path is reached. A
post-cutover cancel sets a `CancelIgnored` condition and is a
no-op (rationale in §5.3).

1. Operator sets `spec.cancelRequested=true`.
2. Controller observes via informer; per §5.3 the migration is in
   a pre-cutover phase (else CancelIgnored, return).
3. **First**: write `migration-action: cancel` annotation on the
   **dst pod** (NOT src — see §7.2 D1; cancel is a dst-side
   SIGKILL of the receiver CH process) with fresh
   `$CANCEL_ID = <swiftmigration.Name>:cancel:0`.
4. **Wait** for src pod's `migration-status=failed` with the
   in-flight `<SEND_ID>` and detail containing `"cancelled"`
   (swiftletd-on-src writes this when CH's `send-migration` errors
   out due to the dst-side TCP close; F3.4).
5. **Then** delete dst pod.
6. **Fallback** if no `failed` status from src within 30 seconds:
   force-delete src pod (`grace-period=0 --force`). The migration's
   terminal `cancel` annotation may not be written in this fallback
   path; the controller writes `failureReason: Cancelled` with
   detail "controller forced cleanup; swiftletd cancel ack timed
   out".

**Spike finding**: F3.4 (Phase 2 PR-B's cancel handler is currently
a placeholder; Phase 3a depends on its real implementation as a
must-have-before-ship in §8).

### 2.4 Reconcile-loop interruption recovery

Phase 3a's controller is leader-elected via controller-runtime; if
the leader crashes mid-migration, a new leader picks up the
SwiftMigration in some phase. The new leader's reconcile MUST
reconstruct state **exclusively from cluster observation** —
informer cache + apiserver Get calls. No in-memory state survives
leader-handover.

The cluster-observable state per phase:

| Phase | Reconstruction inputs |
|---|---|
| Pending | SwiftMigration spec |
| Validating | spec |
| Preparing | spec + dst pod existence (deterministic name from UID) + dst pod's Ready condition |
| StopAndCopy | All Preparing inputs + dst pod's `migration-status` + status-id annotations + src pod's `migration-status` + status-id annotations + status.recvAttempts + status.sendAttempts + status.podRef.name + src pod existence |
| Resuming | SwiftGuest's GuestRunning condition + dst pod's `migration-status` + status.podRef.name |
| Completed/Failed/Cancelled | Terminal — reconcile is no-op |

**The recvAttempts and sendAttempts counters are the load-bearing
recovery primitive.** New leader reads them from
`SwiftMigration.status` and uses them as the next dispatch ID.
Phase 1 spike Q1c validated this pattern empirically (≥60s grace,
F1.9; recovery succeeded — F1.8).

**Cutover-mid-flight recovery is the only complex case.** If the
old leader crashed BETWEEN step 1 (podRef patch) and step 2 (src
delete), the new leader observes:

- `SwiftMigration.status.phase=StopAndCopy` (step 3 not done)
- `SwiftMigration.status.phaseDetail="cutover: updating canonical pod"`
- `SwiftGuest.status.podRef.name=<dst-pod-name>` (step 1 done)
- src pod still exists (step 2 not done)

The new leader resumes by re-issuing step 2 (Delete src pod) then
step 3 (patch phase=Resuming). Step 1's repeat is harmless (idempotent
Patch with same value). Step 2 may return NotFound on retry (if the
old leader's Delete actually succeeded but the controller crashed
before observing); NotFound is treated as success.

If the old leader crashed BEFORE step 1, the new leader observes:

- `phase=StopAndCopy`, `phaseDetail="completing cutover"`
- `podRef.name` still pointing at src pod
- src pod still exists
- src pod's `migration-status=complete` already set

The new leader proceeds with the 3-step cutover from step 1.

---

## 3. Pod lifecycle (Option 2 + (P))

### 3.1 The F1.5 decision restated

Phase 3a uses **Option 2 + (P)**: SwiftGuest owns the destination
pod from creation; SwiftGuest's controller resolves the canonical
pod via `status.podRef.name` (Phase 1 already-existing field) with
fallback to `guest.Name`. The (P) suffix denotes the
`status.podRef.name` indirection added in Phase 3a.

The accept rationale (load-bearing per the architect-discipline
review):

1. **The dst pod's post-migration lifetime is long** — it IS the
   guest, indefinitely. SwiftGuest must own it from the start so
   ownership doesn't transfer at cutover (transferring controller
   ownerRefs is a complex operation we explicitly avoid).
2. **Kubernetes pods cannot be renamed.** The dst pod's name
   (`<guest>-mig-<short-uid>`) is permanent; therefore the
   canonical-pod-resolution mechanism must NOT assume name equals
   `guest.Name`. The `status.podRef.name` indirection is the
   minimal mechanism that decouples the two.
3. **Minimal SwiftGuest-controller code change**: a single helper
   `resolveCanonicalPod(guest)` that reads `status.podRef.name`
   (with fallback to `guest.Name` for pre-migration guests).
   Every `r.Get(ctx, key, &existingPod)` in the SwiftGuest
   controller is replaced with this helper. Estimated ~10 sites.

### 3.2 Pod naming and labels

| Pod | Name | Owner | Labels |
|---|---|---|---|
| Source (pre-migration) | `<guest.Name>` | SwiftGuest | `swift.kubeswift.io/guest=<guest.Name>` |
| Destination (created by SwiftMigration controller) | `<guest.Name>-mig-<short-uid>` | SwiftGuest (set via `controllerutil.SetControllerReference`) | `swift.kubeswift.io/guest=<guest.Name>`, `kubeswift.io/migration-role=destination`, `kubeswift.io/migration=<swiftmigration.Name>` |

`<short-uid>` = first 6 characters of `swiftmigration.UID`. This is
deterministic per migration (idempotent recreate on leader-handover)
and unique cluster-wide (UID is unique).

DNS-1123 length budget: `<guest.Name>` is bounded to 253 chars by
SwiftGuest CRD shape; `-mig-` is 5 chars; `<short-uid>` is 6 chars.
Total worst-case 264 chars exceeds Kubernetes' 253-char pod name
limit. **Phase 3a admission webhook ValidateCreate enforces
`len(guest.Name) <= 242`** for SwiftMigration where mode=live, with
a clear rejection message. Operators with longer guest names cannot
live-migrate (Phase 3a constraint; Phase 3c may relax via name
truncation + uid disambiguation if operator demand surfaces).

#### Pod ownership timeline

Pod ownership across migration phases. `→canonical` indicates which
pod `canonicalPodName(guest)` resolves to; `(SG)` = owned by
SwiftGuest controller (controllerRef); `(SM-cre)` = created by the
SwiftMigration controller but ownerRef is SwiftGuest per F1.5
Option 2.

```
                    ┌──────── pre-migration ────────┐
                    │ src pod: <guest.Name> (SG)    │
                    │ → canonical = src             │
                    └───────────────────────────────┘
                                   │
                                   │ SwiftMigration created
                                   ▼
  Phase Validating ─── src pod (SG)  → canonical = src
                       (label patch: kubeswift.io/migration=...)
                                   │
                                   ▼
  Phase Preparing  ─── src pod (SG)  → canonical = src
                       dst pod created by SM controller (SM-cre)
                       dst ownerRef = SwiftGuest (Option 2)
                                   │
                                   ▼
  Phase StopAndCopy ── BOTH pods exist
                       src pod (SG)  → canonical = src (still)
                       dst pod (SG)  receiving migration state
                                   │
                                   │ ── CUTOVER (3-step, atomic from
                                   │     state-machine PoV; §3.5):
                                   │     1. patch SG.status.podRef.name = dst
                                   │     2. delete src pod
                                   │     3. patch SM.phase = Resuming
                                   ▼
  Phase Resuming   ─── dst pod (SG)  → canonical = dst (via podRef)
                       src pod gone
                                   │
                                   ▼
  Phase Completed  ─── dst pod (SG)  → canonical = dst (durable)
                       (dst pod's name persists indefinitely;
                        Kubernetes pods cannot be renamed —
                        F1.5 Option 2 + (P) is forced by this)
```

The (P) indirection — `status.podRef.name` resolved by
`canonicalPodName(guest)` — is what makes the dst pod's permanent
name acceptable: SwiftGuest's reconcile follows the indirection
without caring about name shape.

### 3.3 SwiftGuest controller migration-awareness

Phase 3a modifies the SwiftGuest controller's reconcile loop in
exactly one structural way: pod lookup via `resolveCanonicalPod`.

```go
// internal/controller/swiftguest/canonical_pod.go (NEW)

// canonicalPodName returns the canonical launcher pod name for
// the SwiftGuest. Pre-migration guests use guest.Name. Post-
// migration guests use status.podRef.name (set by the
// SwiftMigration controller's cutover step).
func canonicalPodName(guest *swiftv1.SwiftGuest) string {
    if guest.Status.PodRef != nil && guest.Status.PodRef.Name != "" {
        return guest.Status.PodRef.Name
    }
    return guest.Name
}

// resolveCanonicalPod fetches the canonical pod and verifies it
// belongs to this SwiftGuest. Mitigates against a hypothetical
// scenario where status.podRef.name is set to an arbitrary pod
// (e.g., RBAC misconfiguration leaks swiftguests/status patch).
// Returns NotFound-equivalent if the pod doesn't pass the check.
func (r *Reconciler) resolveCanonicalPod(
    ctx context.Context,
    guest *swiftv1.SwiftGuest,
) (*corev1.Pod, error) {
    var pod corev1.Pod
    key := client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(guest)}
    if err := r.Get(ctx, key, &pod); err != nil {
        return nil, err
    }
    // Defense-in-depth: label + ownerRef must agree.
    if pod.Labels[GuestLabelKey] != guest.Name {
        return nil, fmt.Errorf("canonical pod %q has wrong guest label", pod.Name)
    }
    if !metav1.IsControlledBy(&pod, guest) {
        return nil, fmt.Errorf("canonical pod %q not controlled by SwiftGuest %q", pod.Name, guest.Name)
    }
    return &pod, nil
}
```

Reconcile-loop callers replace:

```go
// BEFORE:
r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: guest.Name}, &existingPod)
// AFTER:
r.Get(ctx, client.ObjectKey{Namespace: guest.Namespace, Name: canonicalPodName(guest)}, &existingPod)
```

The audit shows ~10 call sites; rust-runtime-engineer review on Day 4
confirms the exact set.

**The pod-create path retains `guest.Name`** as the pod name for
fresh SwiftGuests (no migration in flight). Pod-creation is
controlled by a single function in the SwiftGuest controller; the
function reads `canonicalPodName(guest)` and uses the result. For a
pre-migration guest, the result is `guest.Name`; for a post-migration
guest whose canonical pod has been deleted (e.g., user explicitly
deleted the pod), the function recreates with name=`status.podRef.name`
(the post-migration pod name, which is now the guest's permanent
name). This is intentional: post-migration, the dst pod's name is
the durable identity.

### 3.4 swiftctl logs/console/ssh (kubectl-logs foot-gun closure)

`swiftctl logs <guest>`, `swiftctl console <guest>`, and
`swiftctl ssh <guest>` all currently resolve the launcher pod by
name `guest.Name`. Phase 3a updates these subcommands to:

```go
// cmd/swiftctl/canonical.go (NEW shared helper)
func resolveLauncherPodName(guest *swiftv1.SwiftGuest) string {
    if guest.Status.PodRef != nil && guest.Status.PodRef.Name != "" {
        return guest.Status.PodRef.Name
    }
    return guest.Name
}
```

This is cosmetically identical to the controller-side helper but
intentionally duplicated: swiftctl is a separate binary with its
own dependency boundary; reaching into `internal/controller/swiftguest`
from `cmd/swiftctl` would be an architectural smell.

**`kubectl logs <guest-name>` is the foot-gun this closes from
the swiftctl surface.** Operators using kubectl directly are
not protected — the design doc Section 1 surfaces this as an
operator-visible behavior change.

### 3.5 The cutover ordering invariant — restated

(Belt-and-suspenders restatement; primary statement is in §2.3
StopAndCopy.)

**Maintainer note**: this section is intentionally a duplicate of
§2.3's cutover-ordering paragraph. The consequences of getting the
sequence wrong are severe enough to warrant the duplication —
operators searching either §2 (state machine) or §3 (pod lifecycle)
must hit the same rule. **If you update the cutover sequence,
update BOTH sections.** Drift between the two will surface as a
real bug, not a doc inconsistency.

The cutover from src to dst at the StopAndCopy → Resuming
transition fires three steps in this exact order:

```
Step 1: Patch SwiftGuest.status.podRef.name = <dst-pod-name>
Step 2: Delete src pod
Step 3: Patch SwiftMigration.status.phase = Resuming
```

**Why the ordering is load-bearing**:

- Step 1 first means SwiftGuest's reconcile, fired by the status
  patch, immediately resolves canonical pod = dst pod. SwiftGuest
  owns the dst pod (set at Preparing); SwiftGuest's reconcile
  observes its owned canonical pod is Running and is satisfied.
- Step 2 next deletes the src pod. SwiftGuest's reconcile fires
  again on src pod NotFound. Reconcile reads
  `canonicalPodName(guest) = <dst-pod-name>` (from step 1's
  status patch) and is unaffected — the missing src pod is no
  longer canonical. **No panic-recreate.**
- Step 3 last marks the SwiftMigration as moved past cutover.
  Resuming-phase reconcile observes GuestRunning on the dst pod
  (which it has been doing all along; cutover is a no-op for the
  guest's runtime state) and proceeds to Completed.

If any step fails, retry-in-place (don't undo prior steps). The
cutover is forward-only.

### 3.6 Resuming completion gate (the §2.3 forward-pointer)

**Gate**: `SwiftGuest.status.conditions["GuestRunning"]=True` on
the dst pod.

**Why GuestRunning, not guest-ip annotation**:

Live migration is resume-vs-boot. The guest's view of its world is
preserved byte-for-byte — same kernel state, same userspace
processes, same network stack state. **The guest does NOT re-DHCP
on resume.** The dst pod's `kubeswift.io/guest-ip` annotation
therefore CANNOT be derived from a fresh DHCP lease on the dst
pod's br0; the dnsmasq DHCP exchange does not re-occur.

Two design options were evaluated:

1. **Gate on GuestRunning + propagate guest-ip via swiftletd-on-src
   reading src's annotation and stamping it onto dst pre-migration.**
   ACCEPTED for Phase 3a. swiftletd-on-dst writes
   `kubeswift.io/guest-ip` on dst pod at receive-complete time,
   reading from a value passed in `migration-action-args` by the
   controller (the controller reads src's `guest-ip` annotation and
   forwards it to dst). This is a Phase 3a swiftletd dependency
   (see §7); spike-finding F2.5's progress annotation work in Phase
   5 is unrelated.
2. **Gate on GuestRunning AND let guest-ip stay empty** until
   operator-driven re-DHCP (e.g., guest reboot). REJECTED — leaves
   `SwiftGuest.status.network.primaryIP` empty post-migration, breaks
   `swiftctl describe`, breaks operator's mental model that a
   running guest has a known IP.

**Timeout handling**: the dst-side GuestRunning condition is set by
swiftletd-on-dst within 1-2s of receive-complete (the same
DynamicObject-patch path Phase 1 uses on first boot). The 30s
window above is a **soft Phase 3a target** communicated in operator
docs and surfaced via `status.phaseDetail` for visibility — it is
NOT a hard timeout that fires Failed. The only timeout that
transitions to Failed is `spec.timeout` per §4.3; operators
investigating slow Resuming should consult §4.3 for the
controller-enforced cap. Resuming-phase Failed transitions in §2.3
are triggered by dst-pod-terminated (`failureReason=PodTerminated`)
or dst guest entering CH-failed state (`failureReason=Other`), not
by a 30s soft target.

**Spike finding**: F2.5 explicitly establishes guest-ip propagation
as a Phase 3a swiftletd-side requirement; Phase 3a does not
re-derive it.

### 3.7 No PDB on dst pod (F4.4)

Phase 3a does NOT create a PodDisruptionBudget for the destination
pod. The reasoning:

- A PDB protects against voluntary disruption (drain). The dst pod
  is intentionally migration-scoped; if a drain hits the dst node
  mid-migration, the right behavior is for the migration to fail
  cleanly (informer event: dst pod terminated → src writes failed
  → controller transitions to Failed with `failureReason:
  PodTerminated`). PDB would block drain entirely and create
  operator-experience issues.
- Phase 4 introduces drain-aware migration via an eviction webhook
  (PR plan: webhook intercepts the dst pod's eviction request
  and the SwiftMigration controller transitions to Failed before
  the eviction succeeds). PDB-on-dst would conflict with this
  design.

The src pod also has no PDB (Phase 1 didn't create one). Operators
draining a node hosting an in-flight live migration's src pod see
the same flow as drain hitting any swift launcher pod: the
SwiftGuest controller's `runPolicy: Running` reconcile recreates
the src pod elsewhere, src.UID changes, SwiftMigration's UID-change
check (§4.2) fires, migration transitions to Failed.

---

## 4. Failure modes catalog

The controller's state machine must handle five distinct failure-
detection mechanisms identified by the spike. Each mechanism has a
controller-side implementation pattern.

### 4.1 Detection via informer event (terminal status)

**Trigger**: src or dst pod's `migration-status` annotation
transitions to a terminal value (`complete` / `failed`).

**Controller pattern**:

- Informer watches both pods (Section 5 wiring); `migration-status`
  changes fire reconcile.
- Reconcile reads `migration-status-id` to confirm the terminal
  status corresponds to the SwiftMigration's current dispatch ID.
- Stale terminal-status annotations from a prior dispatch (different
  ID) are ignored.

**Failure modes covered**:

- F4.1 — dst pod K8s-terminated → src writes `failed`. Controller
  observes via informer event on src pod's annotations; transitions
  to Failed with `failureReason: PodTerminated, failureMessage:
  "destination pod terminated"`.
- Migration-internal failure (CH crashed during `vm.send-migration`,
  CPU mismatch, version mismatch). swiftletd writes `failed` with
  detail. Controller transitions to Failed with `failureReason:
  Other` and detail propagated.
- W1 violation (Phase 2 PR-B's dispatch-side gate). swiftletd writes
  `failed` instead of `complete` if vm_info-on-dst doesn't confirm
  dst Running post-receive. Controller transitions to Failed with
  `failureReason: Other`.

**Spike findings**: F1.2, F4.1.

### 4.2 Detection via cross-resource UID change

**Trigger**: source pod's UID no longer matches the UID stored in
SwiftMigration.status at Validating-entry time.

**Why this matters** (F4.2): graceful K8s termination of the src
pod (`kubectl delete pod --grace-period=N`) does NOT result in
swiftletd writing a terminal status, because swiftletd's CH-send
call blocks on the network and SIGTERM doesn't unwind it before
SIGKILL fires. The src pod gets force-killed, then the SwiftGuest
controller's `runPolicy: Running` reconcile recreates it with a
fresh UID. The SwiftMigration controller has no annotation event
to react to.

**Controller pattern**:

- At Validating phase entry, store
  `SwiftMigration.status.sourcePodUID` (the source pod's UID at
  that moment).
- On every reconcile in StopAndCopy and Resuming phases, compare
  observed source pod UID against stored UID.
- Mismatch = source pod was replaced. Transition to Failed with
  `failureReason: SourcePodReplaced`.
- src pod NotFound (extra-cautious case): also transitions to
  Failed with `failureReason: SourcePodReplaced` (the new pod
  hasn't been created yet, but will be soon by SwiftGuest
  controller's recreate logic).

**Code pattern**:

```go
var srcPod corev1.Pod
err := r.Get(ctx, srcPodKey, &srcPod)
if apierrors.IsNotFound(err) || srcPod.UID != mig.Status.SourcePodUID {
    return r.transitionFailed(ctx, mig,
        FailureReasonSourcePodReplaced,
        "source pod UID changed mid-migration")
}
```

**This is the most operationally consequential failure-detection
finding.** Phase 3a controller would stall indefinitely without it
(waiting for an annotation that will never arrive). Test plan
section (§8) requires explicit coverage.

**Detection scope**: UID-change detection applies ONLY in phases
where the src pod is expected to exist — Validating, Preparing,
and StopAndCopy pre-cutover sub-states (`recv-issued`,
`recv-accepted`, `send-issued`, `send-in-flight`,
`src-completed`). The cutover sub-state intentionally
deletes the src pod as step 2 of the 3-step sequence (§2.3); from
that point onward (cutover step 2 onwards, all of Resuming, all
terminal phases), src-pod NotFound and a missing source-pod UID
are the EXPECTED post-cutover state, NOT a failure signal. The
controller's UID-change check must gate on the SwiftMigration's
phase + phaseDetail before firing — false-positive Failed
transitions post-cutover would orphan the (correctly migrated)
dst guest and be operator-confusing. Implementation pattern: the
check is invoked from a `shouldCheckSourcePodUID(mig)` helper
returning false for phases where the src pod is intentionally
gone.

**Spike finding**: F4.2.

### 4.3 Detection via spec.timeout

**Trigger**: wall-clock duration since `status.startedAt` exceeds
`spec.timeout` (default 5 minutes per F3.5).

**Controller pattern**:

- Every reconcile (in any non-terminal phase) checks
  `time.Since(mig.Status.StartedAt) > spec.Timeout`.
- If exceeded, transition to Failed with `failureReason: Timeout`.
- Cleanup follows the failure-point matrix in §2.3 (Failed phase).

**Why 5 minutes default** (F3.5):

- Spike's Q3-v3 worst-case: kernel TCP retransmit ~127s before
  ETIMEDOUT.
- Spike's normal-case: ~38s migration body for kernel-boot 4Gi
  guest on Longhorn.
- Production-cluster latency amplification (F2.2 caveat): apiserver
  RT 2-5× spike baseline; controller writes 4× longer.
- 5 minutes = 4-7× headroom over normal case + 2-3× headroom over
  worst case. Operators with very large guests can override
  upward.

**Phase 3a does NOT enforce `spec.maxPauseWindow`** in the timeout
path. maxPauseWindow is a future Phase 5 admission concern. Phase
3a's timeout is the total-migration-wall-clock cap.

**Spike finding**: F3.5.

### 4.4 Detection via reconcile-loop recovery

**Trigger**: leader-handover; new controller pod observes the
SwiftMigration mid-flight.

**Controller pattern**: §2.4. Reconstruction from cluster state
alone. No active failure detection — the new leader continues
where the old leader left off.

**Failure surface within recovery**: cutover-mid-flight (between
step 1 and step 2). §2.4 documents the resume sequence.

**Spike findings**: F1.8, G (open question on leader-handover).

### 4.5 Detection via cancel path

**Trigger**: operator sets `spec.cancelRequested=true`.

**Controller pattern**: §2.3 Cancelled phase. Annotation-FIRST
discipline; pod-deletion fallback only on swiftletd-cancel-ack
timeout.

**Phase 3a dependency**: F3.4 — swiftletd's cancel handler is
currently a Phase 2 PR-B placeholder. Phase 3a SHIPS WITH the real
cancel-handler implementation. Without it, every cancel path goes
through the pod-deletion fallback (functionally correct but
operationally noisy).

**Spike finding**: F3.4.

### 4.6 Detection via dst pod node failure (Q4e equivalent)

**Trigger**: destination node transitions to NotReady mid-migration.

**Why this is a specific case worth calling out**: spike F4.5
established that drain ≠ true node failure but functionally
equivalent for Phase 3a — both produce dst pod terminated +
src CH errors out + src writes `failed`. So **the controller
detects this via 4.1 (informer event for src=failed)**, not via
a dedicated node-watch.

**Optional Phase 3a enhancement** (NOT in scope for the first
implementation PR; defer to Phase 4 if operator demand surfaces):
the controller could watch destination node Ready condition and
fast-fail Validating if the node went NotReady AFTER admission but
BEFORE migration started. For Phase 3a, the spec.timeout fallback
covers this case at higher latency.

**Spike finding**: F4.5.

### 4.7 Failure-mode catalog summary table

Reproduced from the spike for at-a-glance reference, with Phase 3a
controller's response logic per row.

| Mode | Detection | Terminal phase | failureReason |
|---|---|---|---|
| Normal success | informer: src=complete + cutover succeeds | Completed | n/a |
| Migration-internal error | informer: src=failed | Failed | Other |
| W1 violation | informer: src=failed (W1 gate) | Failed | Other |
| Dst pod K8s-terminated (graceful or drain) | informer: src=failed | Failed | PodTerminated |
| Src pod K8s-terminated | UID change (§4.2) | Failed | SourcePodReplaced |
| Src node failure | informer: src pod NotFound (after node-controller GC) → UID-change check | Failed | SourcePodReplaced |
| Network blackhole | spec.timeout | Failed | Timeout |
| Dst node failure | informer: src=failed (kernel TCP timeout ~127s) | Failed | PodTerminated |
| Operator cancel — happy path | informer: src=failed with cancel-id | Cancelled | Cancelled |
| Operator cancel — fallback | controller force-deletes src after 30s | Cancelled | Cancelled (with detail "controller forced cleanup") |
| Operator cancel — post-cutover | `CancelIgnored` condition set; migration proceeds | Completed | n/a (CancelIgnored is a condition, not a failure) |
| Cutover mid-flight crash | reconcile-recovery (§2.4) | continues StopAndCopy or Resuming | n/a |
| Phase stall in Resuming | spec.timeout | Failed | Timeout |
| Phase stall in StopAndCopy | spec.timeout | Failed | Timeout |

The table is Phase 3a's exhaustive failure-mode coverage. A
reader looking for "what happens if X" should find X in the
Trigger column or surface it as a gap that the doc must address.

---

## 5. Controller-runtime integration

### 5.1 Watches

The SwiftMigration controller's `Watches` set:

| Resource | Filter | Why |
|---|---|---|
| SwiftMigration | own object | primary CR |
| Pod | label `kubeswift.io/migration` exists (refers back to a SwiftMigration) | dst pod creation/Ready/termination events; src pod terminal-status annotation events |
| SwiftGuest | own (via SwiftMigration's `spec.guestRef`) | observe `status.conditions["GuestRunning"]` for Resuming gate |

The Pod watch's filter is **the load-bearing design choice**. The
dst pod carries `kubeswift.io/migration=<swiftmigration.Name>`
(set by the SwiftMigration controller at Preparing entry, on a
pod it owns).

**The src pod also carries the migration label**, set by the
SwiftMigration controller via a single-field metadata Patch at
Validating-entry: `kubeswift.io/migration=<swiftmigration.Name>`
+ `kubeswift.io/migration-role=source`. Patching the src pod's
labels is acceptable here despite SwiftGuest's controller owning
the pod — labels are additive and the SwiftMigration controller's
RBAC includes `pods/patch`. This was an architect-discipline
correction over an earlier draft that proposed observing src-pod
annotation events via cross-pod event indirection on a 30s
SyncPeriod (which would have inflated `observedDowntime` by up
to 30s on every migration — unacceptable).

With both pods labeled, the controller's labeled-watch fires
reconcile on annotation events for either side. Cleanup at
terminal phase deletes the labels via metadata Patch (best-effort;
src pod is gone post-cutover anyway).

The 30s `SyncPeriod` (§5.5) remains as defense-in-depth resync,
not as the primary src-observation path.

**Alternative considered: watch all pods in all namespaces with a
predicate that filters by SwiftMigration cross-reference.** Too
expensive on large clusters. The dst-pod-labeled watch + 30s
SyncPeriod is the chosen trade-off for the first implementation;
§5.5 elaborates the explicit-per-migration-watch alternative.

### 5.2 RBAC

Phase 3a's controller manager already has migration.kubeswift.io/
swiftmigrations get/list/watch/update/patch from Phase 1.
Phase 3a adds:

- **pods**: get/list/watch/create/delete (Phase 1 had `create` for
  source-side pod-recreate via SwiftGuest indirection; Phase 3a
  needs direct delete during cutover and during pre-cutover
  cleanup).
- **swiftguests**: get/list/watch/update/patch (Phase 1 had
  get/list/watch only; Phase 3a's cutover step 1 patches
  `status.podRef.name` directly on the SwiftGuest's status
  subresource, so the controller needs `swiftguests/status` patch
  permission too).
- **swiftguests/status**: patch (new for Phase 3a).
- **nodes**: get/list/watch (Phase 1 already has this for the
  capacity check).

The recurring W7/W8 lesson (RBAC for cached-client `r.Get` requires
list,watch alongside get) applies: every new resource the
controller reads needs `list,watch`. Phase 3a adds nothing the
cache hadn't already opened in Phase 1, except `swiftguests/status`
which is a status-subresource verb (not cached). So no W-style
RBAC regression risk.

**Already satisfied by prior superset grant** (audited as part of
PR 1 Group B0.5, 2026-05-02). The four verbs enumerated above
(swiftguests update/patch, swiftguests/status patch, pods delete,
nodes get/list/watch) are all already granted in
`config/manager/controller-manager-rbac.yaml` and
`charts/kubeswift/templates/controller-manager/rbac.yaml`. The
existing rule on lines 10-12 of both files —
`["swiftguests", "swiftguests/status"]` with full
`["get", "list", "watch", "create", "update", "patch", "delete"]`
— covers Phase 3a's needs without YAML change. Audit also
verified VolumeAttachments verbs (already Phase 1, line 114-116),
Events verbs (already Phase 1, line 107-108), and Leases verbs
(already Phase 1, line 130-132). No Phase 3a RBAC additions
required. The "Phase 3a adds" framing above describes the
verb-set Phase 3a code *relies on*, not a list of YAML changes
to make. `kubectl auth can-i` verification commands for
operators redeploying a Phase 3a controller-manager:

```bash
kubectl auth can-i patch swiftguests/status \
  --as=system:serviceaccount:kubeswift-system:controller-manager
# expected: yes
kubectl auth can-i delete pods \
  --as=system:serviceaccount:kubeswift-system:controller-manager
# expected: yes
kubectl auth can-i patch swiftmigrations/status \
  --as=system:serviceaccount:kubeswift-system:controller-manager
# expected: yes
```

**RBAC scope discipline**: the SwiftMigration controller patches
`swiftguests/status` only to set `status.podRef.name` (cutover
step 1) and `status.conditions[]` (existing Phase 1 condition
writes via DynamicObject). Kubernetes RBAC has no field-level
granularity for status subresource, so `patch` is necessarily
broader than the actual write set. The narrow write contract is
documented here for security-audit visibility; a future
ValidatingAdmissionPolicy (CEL) on SwiftGuest UPDATE could
constrain SwiftMigration's SA to those two field paths if
operators require defense-in-depth. **Tracked as a Phase 5
operational-polish item** (no Phase 3a code change).

### 5.3 Cancel handling (the §2.3 forward-pointer)

The cancel mechanism is `spec.cancelRequested=true` (a bool field on
SwiftMigration). **Authorization model**: cancel authorization is
governed by standard `swiftmigrations` UPDATE RBAC; cancel is an
idempotent intent flag and surfaces no privilege beyond what the
requestor already has on the SwiftMigration CR. Setting cancel is
an authority-reduction action (it stops a process the requestor
already has the authority to start), so no privilege-escalation
surface opens.

PR #26's per-operation discipline rules out webhook-side rejection
of post-cutover cancel because ValidateUpdate is shape-only. Phase
3a handles this controller-side:

```go
// internal/controller/swiftmigration/cancel.go (NEW)
func (r *Reconciler) honorCancel(ctx context.Context, mig *migv1.SwiftMigration) (ctrl.Result, error) {
    if !mig.Spec.CancelRequested {
        return ctrl.Result{}, nil
    }
    if isPostCutover(mig) {
        // No-op; surface a condition for operator visibility.
        return ctrl.Result{}, r.setCondition(ctx, mig, &metav1.Condition{
            Type:    "CancelIgnored",
            Status:  metav1.ConditionTrue,
            Reason:  "PastCutover",
            Message: "migration past cutover; cancel cannot unwind",
        })
    }
    // Pre-cutover: drive to Cancelled via §2.3 cancel discipline.
    return r.transitionCancel(ctx, mig)
}

// isPostCutover returns true when cutover step 1 (status.podRef.name
// patch on the SwiftGuest) has succeeded.
func isPostCutover(mig *migv1.SwiftMigration) bool {
    p := mig.Status.Phase
    if p == migv1.SwiftMigrationPhaseResuming ||
       p == migv1.SwiftMigrationPhaseCompleted ||
       p == migv1.SwiftMigrationPhaseFailed ||
       p == migv1.SwiftMigrationPhaseCancelled {
        return true
    }
    // Mid-cutover: read the PodRefSwapped condition (§6.3).
    return meta.IsStatusConditionTrue(mig.Status.Conditions, "PodRefSwapped")
}
```

The `PodRefSwapped` condition is set True by cutover step 1's
success (Section 6). It's the canonical "cutover step 1 done"
signal that survives leader-handover, lets `isPostCutover` answer
correctly even mid-cutover, AND carries `lastTransitionTime` so
operators debugging a `CancelIgnored` outcome can correlate the
swap-time with their cancel attempt.

**Rationale for CancelIgnored vs marking Cancelled (post-cutover)**:
post-cutover, the migration's outcome is fixed — the dst pod is
canonical, the src pod is gone, the guest is running on the target
node. Marking the SwiftMigration as Cancelled would misrepresent
cluster state: the migration succeeded, the resources moved, the
operator-observable end-state matches a Completed migration.
CancelIgnored preserves the operator's intent in the audit trail
(via `lastTransitionTime` and `reason`) without lying about the
migration's actual result. The migration still proceeds to
Completed normally; the CancelIgnored condition is informational.

### 5.4 Webhook integration

The SwiftMigration validating webhook (PR #26's per-operation
discipline) gains two new ValidateCreate-time rules for Phase 3a:

1. **Per-source-node concurrency**: reject if any other
   SwiftMigration with the same `status.sourceNode` is in a
   non-terminal phase. The webhook reads SwiftMigrations via the
   apiserver (cached via webhook client).
2. **Guest-name length cap for live mode**: reject if
   `mode=live && len(guestRef.name) > 242` per §3.2.

ValidateUpdate remains shape-only per PR #26. ValidateDelete
remains pass-through.

The webhook does NOT validate `cancelRequested` transitions —
cancel-after-cutover is handled controller-side per §5.3.

### 5.5 SyncPeriod vs explicit per-migration watch — trade-off

Per §5.1, both src and dst pods carry the
`kubeswift.io/migration=<swiftmigration.Name>` label and the
labeled-watch fires reconcile on either side's annotation events.
The 30s `SyncPeriod` is defense-in-depth, not the primary
observation path — annotation-event latency is informer-cache
push latency (~25ms per spike F2.4), not 30s.

An earlier draft considered an explicit-per-migration-watch
alternative scaling with active migrations rather than total pod
count. That alternative has two issues that defang it: (a)
controller-runtime caches are append-only — dynamic informer
teardown is not first-class; (b) once a Pod-shaped informer is
established for a namespace, controller-runtime reuses it for
subsequent watches, so the first migration's setup widens the
cache scope cluster-wide anyway. The labeled-watch + SyncPeriod
approach matches the explicit-watch approach in practice.

**Decision**: ship the labeled-watch + 30s SyncPeriod. If
apiserver-load scaling concerns surface in production (clusters
with thousands of historical SwiftMigrations + frequent activity),
revisit in Phase 5 via a finalizer-driven reconcile-skip for
terminal-phase SwiftMigrations older than a retention window.
Reversible; no CRD impact; no operator-visible behavior change.

---

## 6. Status schema

### 6.1 SwiftMigration status additions

Phase 1's SwiftMigration status fields are reused. Phase 3a adds
the following (all optional, all backwards-compatible with Phase 1
offline migrations that don't populate them):

```go
type SwiftMigrationStatus struct {
    // ... existing Phase 1 fields ...

    // SourcePodUID is the source pod's UID at Validating-entry.
    // Used by §4.2 UID-change failure detection.
    // Set in Validating phase; never updated thereafter.
    // +optional
    SourcePodUID types.UID `json:"sourcePodUID,omitempty"`

    // RecvAttempts counts vm.receive-migration dispatches issued
    // on the destination pod. Increments on retry. Used to derive
    // $RECV_ID per F1.8.
    // +optional
    RecvAttempts int32 `json:"recvAttempts,omitempty"`

    // SendAttempts counts vm.send-migration dispatches issued
    // on the source pod. Increments on retry. Used to derive
    // $SEND_ID per F1.8.
    // +optional
    SendAttempts int32 `json:"sendAttempts,omitempty"`

    // ObservedDowntime is the wall-clock duration from cutover step 2
    // (src pod delete dispatched) to GuestRunning=True observed on
    // dst pod. The operator-visible "downtime" metric.
    // +optional
    ObservedDowntime *metav1.Duration `json:"observedDowntime,omitempty"`

    // ObservedPauseWindow is swiftletd-on-src-reported vCPU-paused
    // duration during stop-and-copy. Forwarded from src pod's
    // migration-status-detail annotation. NOT the same as
    // ObservedDowntime (which is dominated by cutover + apiserver
    // latency, not vCPU pause).
    // +optional
    ObservedPauseWindow *metav1.Duration `json:"observedPauseWindow,omitempty"`

    // FailureReason classifies terminal Failed transitions. One of:
    //   Cancelled, PodTerminated, SourcePodReplaced, Timeout, Other
    // (F4.3)
    // +kubebuilder:validation:Enum=Cancelled;PodTerminated;SourcePodReplaced;Timeout;Other
    // +optional
    FailureReason string `json:"failureReason,omitempty"`

    // FailureMessage is human-readable detail for terminal Failed
    // transitions. Free-form. Logged + surfaced in
    // `kubectl describe swiftmigration`.
    // +optional
    FailureMessage string `json:"failureMessage,omitempty"`

    // TargetIP is the dst pod's primary guest IP, propagated from
    // src's pre-migration discovery via swiftletd at receive-complete.
    // §3.6.
    // +optional
    TargetIP string `json:"targetIP,omitempty"`
}
```

### 6.2 SwiftMigration spec additions

Phase 3a's spec extension is minimal:

```go
type SwiftMigrationSpec struct {
    // ... existing Phase 1 fields ...

    // CancelRequested triggers controller-side cancel. Set by
    // the operator. Honored only in pre-cutover phases; ignored
    // post-cutover with a CancelIgnored status condition. §5.3.
    // +optional
    CancelRequested bool `json:"cancelRequested,omitempty"`
}
```

`spec.timeout` (Phase 1, default 1 hour) is reused. Phase 3a
recommends operators set this to 5 minutes for kernel-boot
guests (per F3.5); the Phase 1 default of 1 hour is preserved
for backwards compatibility. CRD comment on the field will note
the Phase 3a recommendation.

### 6.3 New conditions

| Type | Reason values | Notes |
|---|---|---|
| `PodRefSwapped` | `CutoverStep1Complete` | Set True at cutover step 1 success. The audit trail for "when did the canonical pod swap happen." Used by `isPostCutover` (§5.3) to distinguish pre-cutover from cutover-mid-flight. Conditions carry `lastTransitionTime` natively, which is operationally useful when debugging. |
| `CancelIgnored` | `PastCutover` | §5.3 |

The existing Phase 1 conditions (`Compatible`, `Ready`,
`IPWillChange`) are unchanged.

`PodRefSwapped` is modeled as a condition (not a top-level
boolean) because conditions carry `lastTransitionTime`, `reason`,
`message` natively — operators debugging a `CancelIgnored`
condition need to know WHEN the swap happened to correlate with
their cancel attempt. The `isPostCutover` helper finds the
condition with `Type=PodRefSwapped, Status=True`; absence (or
`Status=False`) means pre-cutover.

### 6.4 phaseDetail vocabulary additions

Phase 3a adds the following `status.phaseDetail` values, all
StopAndCopy and Resuming sub-states:

- `"issuing receive on destination"`
- `"destination receiving"`
- `"issuing send on source"`
- `"transferring guest state"`
- `"completing cutover"`
- `"cutover: updating canonical pod"`
- `"waiting for guest health on destination"`
- `"destination guest healthy"`

Used by both operator-visible `kubectl describe swiftmigration`
output and reconcile-loop recovery (§2.4).

#### Stability discipline for phaseDetail values

Phase 3a treats `phaseDetail` values as stable strings — operators
may parse them, dashboards may match against them, reconcile-loop
recovery (§2.4) reads them. Additions are non-breaking; renames go
through one-minor-release deprecation cycles (emit both forms in
parallel); semantic changes require a new value rather than
repurposing an existing one. Phase 3b/3c/4/5 inherit the same
discipline.

---

## 7. swiftletd interface contract

Phase 3a's controller drives swiftletd via the Phase 2 annotation
surface (PRs #28/#29/#31). This section enumerates the contract
the controller depends on, calls out new dependencies, and clarifies
non-dependencies that future readers might mistakenly treat as
blockers.

### 7.1 Annotation surface (Phase 2 baseline)

The controller writes these annotations on src and dst pods
(direction labels: C→S = controller writes for swiftletd; S→C =
swiftletd writes for controller):

| Annotation | Direction | Set by | Read by | Purpose |
|---|---|---|---|---|
| `kubeswift.io/migration-action` | C→S | controller | swiftletd | dispatch trigger: `receive`, `send`, `cancel` |
| `kubeswift.io/migration-action-id` | C→S | controller | swiftletd | per-dispatch idempotency (Phase 2 PR-B) |
| `kubeswift.io/migration-action-args` | C→S | controller | swiftletd | structured args (e.g., target URL, send-id, guest-ip-to-propagate) |
| `kubeswift.io/migration-status` | S→C | swiftletd | controller | progress: `running` (intermediate or post-receive accept) / `complete` / `failed` |
| `kubeswift.io/migration-status-id` | S→C | swiftletd | controller | matches `migration-action-id` of the dispatch the status reports for (F1.3) |
| `kubeswift.io/migration-status-detail` | S→C | swiftletd | controller | free-form detail; sanitized by Phase 2 PR-B's category-token sanitizer |
| `kubeswift.io/migration-phase2-unsafe-plaintext` | C→S (pod creation) | controller | swiftletd | ack gate (Phase 2 PR-B). Set to `ack` at dst pod creation; required for swiftletd to accept any migration action |
| `kubeswift.io/guest-ip` | (mixed) | swiftletd-on-dst (post-resume) | controller (read into `status.targetIP`) | dst guest IP, propagated from src via `migration-action-args` |

**S1 deprecation contract** (Phase 3b): in Phase 3b, URL inputs
move from `migration-action-args` annotation to fields on the
SwiftMigration CR. Phase 3a writes URL inputs into
`migration-action-args` per the Phase 2 design §8.2.3 manual-path
acceptable-since-operator-is-writer rule. Every annotation-URL-
read site in swiftletd is tagged `// SECURITY-S1` per Phase 2 PR-B
for the Phase 3b grep-and-delete sweep.

### 7.2 Phase 3a swiftletd dependencies (must-have-before-ship)

Three swiftletd changes are Phase 3a dependencies. Each is small;
each has been called out in the spike findings or earlier sections.
Section 8's checklist tracks them.

#### D1 — Real cancel handler (F3.4)

Phase 2 PR-B shipped a placeholder for the `cancel` action. Phase
3a's cancel discipline (§2.3, §5.3) requires a real implementation.

**Cancel primitive: dst-side SIGKILL of receiver CH, NOT src-side
CH API call.** Cloud Hypervisor v51.1 has **no** `vm.cancel-migration`
or equivalent API (verified by Phase 2 spike F4 and direct audit of
`rust/swift-ch-client/src/methods.rs` — only `send_migration` and
`receive_migration` exist; no cancel verb). The only mechanism that
breaks an in-flight `vm.send-migration` cleanly is closing the dst
side of the TCP stream, which causes src CH's `send-migration` to
return error and src swiftletd to write terminal `failed`. Phase 3a
therefore routes cancel via the **destination pod**:

1. Controller writes `migration-action: cancel` with
   `migration-action-id: <CANCEL_ID>` to the **dst pod** (NOT src
   as a previous draft of this section incorrectly stated).
2. swiftletd-on-dst's action loop dispatches the cancel handler
   which SIGKILLs the receiver CH child process.
3. The TCP stream closes; src CH's `send-migration` HTTP call
   returns error; src swiftletd's status-id-paired-write writes
   `migration-status: failed` with the in-flight `<SEND_ID>` and
   detail containing `"cancelled"` (the controller's match string).
4. swiftletd-on-dst also writes its own terminal `migration-status:
   failed` paired with `<CANCEL_ID>` and detail `"cancelled"`.

**Implementation impact (rust/swiftletd)**: D1 requires plumbing a
shared handle to the CH child process from `launch.rs::run_ch_receive`
(which currently owns the `tokio::process::Child` by-value and
blocks on `child.wait()`) into the action loop in `action.rs`. The
mechanism is `Arc<Mutex<Option<Child>>>` or a `oneshot::Sender<Kill>`
channel — either way, a non-trivial ownership refactor that the
"D1 is small" framing previously elided. The architect-discipline
restated estimate is **low-thousands of LOC** for D1 alone, not
hundreds.

**Bounded by 30s**: if the dst-side SIGKILL doesn't produce a src-
side `failed` status within 30s (e.g., kernel TCP retransmit
delaying the error propagation up to ~127s in the worst case per
spike Q3-v3), swiftletd-on-dst writes `failed` anyway. The
controller's fallback (§2.3 step 6) force-deletes the src pod if
even this 30s doesn't suffice — closing the src TCP socket
unconditionally and unwinding any swift-ch-client thread leak.

**swift-ch-client async caveat**: `swift-ch-client` is currently
synchronous (`UnixStream` + blocking `request_ok` in
`methods.rs::send_migration`). A clean cancel-mid-flight on src is
not structurally feasible without either (a) running send_migration
in `tokio::task::spawn_blocking` and accepting a worker-thread leak
on cancel, or (b) refactoring swift-ch-client to async. Phase 3a
ships option (a) and accepts the leak (it terminates with the swiftletd
process). Phase 3b should consider the async refactor; tracked as a
swiftletd follow-up.

#### D2 — Auto-write `failed` on abnormal listener exit (F3.2)

Receiver-mode swiftletd-on-dst listens on a TCP socket. If the CH
listener process dies abnormally (panic, SIGKILL'd by something
inside the pod, kernel OOM), swiftletd has historically not written
a terminal status — leaving the controller waiting for a status
that will never arrive.

Phase 3a-shipped behavior: swiftletd-on-dst monitors the CH
listener process via a watchdog task that races against
`child.wait()`; on abnormal exit, writes `migration-status:
failed` with detail "destination listener exited abnormally".
This is a defense-in-depth signal; the controller's `spec.timeout`
(F3.5, default 5min) is the floor.

**Action-id-pairing exception (shape note)**: every other status
write in `action.rs::write_migration_status` is paired with a
`migration-action-id` per Phase 2 PR-B's status-id-paired-write
invariant. D2's abnormal-exit `failed` write has no in-flight
action-id — the watchdog fires asynchronously to any dispatch.
swiftletd-on-dst pairs the write with the LAST-OBSERVED action-id
(typically the `<RECV_ID>` from the receive dispatch that was
in flight when CH died), and includes a write-once guard so a
panicked-then-restarted swiftletd does not double-write.
Controllers reading this `failed` status MUST match against the
`<RECV_ID>` they last issued. §7.4 records this as a contract
extension over Phase 2 (the only one).

**Plumbing impact (rust/swiftletd)**: receiver-mode
`run_ch_receive` in `launch.rs` currently blocks on `child.wait()`
synchronously. D2 requires a tokio `select!` or split task, with
the watchdog reusing the same shared `Child` handle the D1 cancel
plumbing introduces. Estimated 100-200 LOC additional on top of
D1's ownership refactor.

#### D3 — Guest-IP propagation (§3.6 dependency)

The controller reads src pod's `kubeswift.io/guest-ip` annotation
at StopAndCopy entry, forwards the value into the dst pod's
`migration-action-args` for the `receive` action. swiftletd-on-dst
reads the value out of the args, and at receive-complete time
writes it as the dst pod's `kubeswift.io/guest-ip` annotation.

This makes the dst-side `guest-ip` annotation a propagation, not
a fresh DHCP discovery. Live migration's resume-vs-boot semantics
mean DHCP cannot re-fire on the dst pod's br0 (§3.6). The
propagation closes the gap.

**SECURITY-S1 tag**: although the propagated value is IP-shaped
(not URL-shaped), the trust property is identical to Phase 2's
URL annotation reads — it flows from operator-controlled
SwiftMigration → controller-written annotation → swiftletd-read.
The Phase 3a swiftletd D3 read site MUST be tagged
`// SECURITY-S1` so the Phase 3b grep-and-delete sweep that moves
operator-controlled inputs from annotations to CR spec fields
covers D3 too. Implementation reuses
`rust/swiftletd/src/lease.rs::patch_pod_annotation` (existing
~30-50 LOC integration point).

### 7.3 Phase 3a swiftletd non-dependencies (explicit)

These items appeared in the Phase 3a spike or design conversations
and might be misread as blockers. They are NOT.

#### F1.1 — dst-side terminal-value rename

The dst-side `migration-status=running` annotation has two distinct
meanings: (a) at receive-accept time (the listener is up and
accepting), (b) at terminal-success time on dst (the migrated guest
is running). F1.1 proposes renaming (b) to `complete` for cleaner
state-machine semantics.

**Phase 3a does NOT depend on F1.1.** The controller's Resuming →
Completed gate uses src-side `migration-status=complete` (F1.2),
which is unaffected by the dst-side ambiguity. Future readers
should NOT treat F1.1 as a Phase 3a blocker. It ships as a
separate small swiftletd PR if and when convenient.

#### F2.5 — intermediate progress annotations (precopy/stopcopy)

Phase 2 design §3 mentioned intermediate progress annotation values
swiftletd does not currently emit. Operators watching a 38s
migration with no intermediate progress visibility surface this as
a usability gap.

**Phase 3a does NOT depend on F2.5.** The controller-level
state-machine works correctly on the existing `running` /
`complete` / `failed` vocabulary. F2.5 ships as Phase 5 (operational
polish) — see Phase 5 roadmap entry.

#### CPU-feature pre-flight check (Phase 2 spike F12)

CPU microarch incompatibility between src and dst is the realistic
production failure mode for migration. Phase 2 PR-B's category-
token sanitizer collapses raw CH errors into `cpu_incompat` so the
controller can surface a clean failure reason.

**Phase 3a does NOT ship a SwiftMigration webhook pre-flight check
for CPU compatibility.** That is Phase 3b work. The Phase 3a
controller treats CPU incompatibility as the same Failed transition
as any other migration-internal error (§4.1, `failureReason: Other`),
with detail propagated via `migration-status-detail`.

### 7.4 swiftletd contract version

Phase 3a's annotation surface is identical to Phase 2's plus D1/D2
/D3 above. No new annotation keys; no new actions. **One shape
extension over Phase 2**: D2's abnormal-exit `failed` write is
paired with the last-observed action-id, not with a fresh in-flight
dispatch's action-id (§7.2 D2 details). The pairing semantics are
unchanged — controllers MUST match the action-id; what changes is
that the dispatch correlated with the failed write may have already
completed normally (the failure happened post-dispatch). swiftletd
images that ship D1+D2+D3 are Phase-3a-capable; older swiftletd
images are not.

The controller does not version-detect swiftletd at runtime —
operators are expected to deploy controller and swiftletd from the
same release. Phase 3a's controller release notes call out the
swiftletd image hash that ships D1+D2+D3 and the must-have-before
-ship checklist (§8) keys on this.

---

## 8. Implementation checklist + must-have-before-ship

Section 8 is the operator-and-implementer-facing summary of what
Phase 3a's first PR must contain to be shippable. Items are
grouped by component; each links to its design source.

### 8.1 Must-have-before-ship checklist

Phase 3a is shippable when ALL of the following are true. The
checklist is binding; partial-ship is not acceptable.

#### Controller (Go)

- [ ] **MH-C1** — SwiftMigration controller `mode: live` state
  machine (§2). All eight phases handled. Per-operation
  discipline preserved (terminal phases are no-op).
- [ ] **MH-C2** — Cutover ordering invariant (§2.3, §3.5):
  3-step sequence (podRef patch → src delete → phase patch) with
  retry-in-place on each step's failure. Test: kill controller
  between each step, observe new leader resumes correctly.
- [ ] **MH-C3** — UID-change failure detection (§4.2) with
  `shouldCheckSourcePodUID(mig)` helper gating to pre-cutover
  phases only. Test: graceful-delete src pod mid-StopAndCopy,
  observe Failed with `failureReason: SourcePodReplaced`.
- [ ] **MH-C4** — `spec.timeout` default 5min for live mode (F3.5).
  Phase 1 default of 1h preserved for offline mode (CRD
  conditional default not feasible; defaulting webhook applies
  the live-mode default at admission time).
- [ ] **MH-C5** — `status.failureReason` enum populated correctly
  on all Failed transitions per §4.7 summary table. Test:
  exhaustive failure-mode coverage, one test per row.
- [ ] **MH-C6** — Cancel handling controller-side (§5.3): cancel
  honored only in pre-cutover phases; post-cutover sets
  `CancelIgnored` condition without transitioning to Cancelled.
- [ ] **MH-C7** — Reconcile-loop recovery (§2.4): cluster-state
  reconstruction works for every non-terminal phase. Test:
  controller-pod-kill at each phase boundary, observe state-
  machine continues correctly under new leader.

#### SwiftGuest controller (Go)

- [ ] **MH-G1** — `canonicalPodName(guest)` helper (§3.3) replaces
  every cached `r.Get` for a Pod scoped to the SwiftGuest's
  namespace+name in the SwiftGuest reconciler. **Acceptance**:
  (a) static-analysis grep `r\.Get\(.*Name:\s*guest\.Name.*Pod` in
  `internal/controller/swiftguest/` returns zero matches; (b)
  per-call-site table-driven unit test constructs a SwiftGuest
  with `status.podRef.name != guest.Name` and exercises every
  reconcile entry point, asserting the resolved pod key uses
  `podRef.Name`. The grep + per-call-site assertion gate replaces
  the earlier "audit confirms exact set" framing — a single
  fallback test does not catch a missed call site that defaults
  to `guest.Name` (W3-style regression risk).
- [ ] **MH-G2** — `canonicalPodName` defense-in-depth verification:
  before treating the resolved pod as canonical, the SwiftGuest
  reconciler verifies the pod has
  `swift.kubeswift.io/guest=<guest.Name>` label AND ownerRef
  pointing to the SwiftGuest. Mitigates a hypothetical
  RBAC-misconfiguration scenario where `swiftguests/status` patch
  leaks (§5.2): an attacker could redirect `status.podRef.name`
  to an arbitrary pod in the same namespace; the label+ownerRef
  check rejects the redirect.

#### Webhook (Go)

- [ ] **MH-W1** — ValidateCreate: per-source-node concurrency check
  (§5.4). Test: two concurrent SwiftMigrations with the same
  source node, second is rejected.
- [ ] **MH-W2** — ValidateCreate: 242-char guest-name cap for
  `mode=live` (§3.2). Test: 243-char guest-name SwiftMigration
  rejected with clear message.
- [ ] **MH-W3** — ValidateUpdate remains shape-only (PR #26
  discipline preserved). No cluster-state checks.

#### swiftctl (Go)

- [ ] **MH-S1** — `swiftctl logs/console/ssh` resolve via
  `status.podRef.name` (§3.4) with fallback to `guest.Name`. Test:
  post-migration guest, kubectl logs by guest-name fails (foot-
  gun is real); swiftctl logs by guest-name succeeds via the
  helper. **This is a separate must-have item per the user's
  Day-1 instruction; bundling with the SwiftMigration controller
  PR is fine, but it must not be deferred.**

#### swiftletd (Rust)

- [ ] **MH-R1** — D1: real cancel handler (§7.2). Replaces Phase 2
  PR-B's placeholder. Test: cancel mid-StopAndCopy, observe
  `migration-status=failed` with cancel-id and "cancelled" detail
  within 30s.
- [ ] **MH-R2** — D2: auto-write `failed` on abnormal listener
  exit (§7.2 / F3.2). Test: SIGKILL the CH listener mid-receive
  on dst pod, observe swiftletd writes `failed`.
- [ ] **MH-R3** — D3: guest-IP propagation (§7.2 / §3.6).
  swiftletd-on-dst reads `guest_ip` from `migration-action-args`
  and writes it as the dst pod's `kubeswift.io/guest-ip`
  annotation at receive-complete.

#### Pre-existing (carried into Phase 3a)

- [x] **MH-N1** — **B0 br0/Calico CIDR collision fix** (PR #39 —
  **status: merged 2026-04-30, cluster-validated on miles+boba
  via `test/networking/b0-cross-node-tcp.sh`**). br0 subnet
  moved from `10.244.125.0/24` to `192.168.99.0/24` (RFC1918
  reservation), eliminating the route-shadowing bug that
  silently broke cross-node pod-to-pod TCP on Calico-using
  clusters. Migration cannot be validated without this; spike
  could not validate Q1 until this fix shipped. Listed as
  pre-existing because it is already on `main`; the Phase 3a
  PR does not re-touch it. Verification gate for the Phase 3a
  PR is "deployed cluster passes `make b0-cross-node-tcp-test`."

### 8.2 Test plan

Three test surfaces; each maps to checklist items.

#### Unit tests (Go)

- Per-failure-mode coverage of §4.7 summary table — 11 rows, 11
  table-driven test cases minimum.
- Cutover ordering: kill-between-steps test using a
  patch-counting fake client (Phase 1's `patchCountingClient`
  hardened in PR #25 is the pattern).
- isPostCutover correctness across all phase × condition combos.
- Webhook per-source-node concurrency check.
- Webhook 242-char admission rule.
- canonicalPodName fallback behavior (pre-migration → guest.Name;
  post-migration → status.podRef.name).

#### Cluster-e2e tests (test/migration/)

The Phase 3a spike validated four scenarios manually (Q1-Q4); the
e2e suite operationalizes those plus the Phase 3a-specific
additions surfaced during design. Scenarios are tagged by
shipping bar: **[CI]** = CI-blocking, runs on every PR touching
Phase 3a paths; **[Walk]** = operator-walkthrough, runs manually
between phases (per the mini-walkthrough discipline that has
caught W3/W4/W6/W8/W9/W10/W11); **[Defer]** = deferred to a later
phase, listed here for traceability.

| # | Scenario | Tag | Phase 3a spike origin |
|---|---|---|---|
| E1 | Happy path: kernel-boot 4Gi guest cross-node round-trip on Longhorn. Asserts: phase progression, sentinel-md5 survival pre/post-migration, `observedDowntime` ≤ spec.timeout, `observedPauseWindow` ≤ 5s, `status.targetIP` populated. | **[CI]** | Q1 |
| E2a | Reconcile-interruption recovery — pre-step-1: kill controller-manager pod after `src-completed` sub-state but BEFORE cutover step 1 (podRef patch). Assert new leader proceeds with all 3 cutover steps, migration completes, src pod cleanly deleted, dst pod is canonical. | **[CI]** | Q1c spike (F1.8); §2.4 |
| E2b | Reconcile-interruption recovery — between step 1 and step 2: kill controller-manager mid-cutover (after podRef patch succeeds, before src pod delete dispatched). Assert new leader resumes with step 2, migration completes, no panic-recreate of src pod by SwiftGuest reconciler (the latter must observe `canonicalPodName(guest) = <dst-pod-name>` and treat src pod NotFound as expected). PodRefSwapped condition asserted True at handover. | **[CI]** | Q1c spike (F1.8); §2.4 cutover-mid-flight |
| E2c | Reconcile-interruption recovery — Resuming phase: kill controller-manager during Resuming (after cutover, before GuestRunning observed). Assert new leader observes GuestRunning=True on dst pod and transitions to Completed. | **[CI]** | §2.4 |
| E3 | Listener-timeout under silence: dst pod's CH listener intentionally never accepts the connection (block via iptables on dst pod). Assert `failureReason=Timeout` after `spec.timeout` elapses. | **[CI]** | Q3-v1 (F1.9) |
| E4 | Source pod K8s termination mid-StopAndCopy: `kubectl delete pod <src>` graceful. Assert `failureReason=SourcePodReplaced` via UID-change detection (§4.2). | **[CI]** | Q4 (F4.2) |
| E5 | Destination pod K8s termination mid-StopAndCopy: `kubectl delete pod <dst>`. Assert `failureReason=PodTerminated` via informer event on src=failed (§4.1). | **[CI]** | Q4 (F4.1) |
| E6 | Drain on destination node mid-StopAndCopy: `kubectl drain` the dst node. Assert dst pod evicted, src writes failed, controller transitions to Failed with `failureReason=PodTerminated`. (No PDB on dst pod per §3.7.) | **[CI]** | Q4 / F4.4 |
| E7 | Operator cancel pre-cutover: set `spec.cancelRequested=true` mid-StopAndCopy. Assert phase transitions to Cancelled with `failureReason=Cancelled`, src pod recovers to Running, dst pod deleted. | **[CI]** | Phase 3a-specific (§5.3) |
| E8 | Operator cancel post-cutover: set `spec.cancelRequested=true` after cutover step 1 succeeded. Assert `CancelIgnored` condition set with `reason=PastCutover`, migration proceeds normally to Completed. | **[CI]** | Phase 3a-specific (§5.3) |
| E9 | Per-source-node concurrency: two SwiftMigrations with the same source node submitted simultaneously. Assert webhook rejects the second at admission. | **[CI]** | Phase 3a-specific (§5.4) |
| E10 | 242-char guest-name admission cap: SwiftMigration with mode=live and 243-char guest-name. Assert webhook rejects with clear message. | **[CI]** | Phase 3a-specific (§3.2) |
| E11 | Post-migration pod-name behavior: post-E1, assert `kubectl logs <guest-name>` fails (foot-gun is real), `swiftctl logs <guest-name>` succeeds via the `status.podRef.name` resolver. | **[Walk]** | §3.4 |
| E12 | RWX+Block disk-boot live migration round-trip on Longhorn-migratable. Boot + migration + sentinel-md5 + observedDowntime assertions. | **[CI]** | Phase 3a-specific; PR #35 carry-forward |
| E12-walk | RWX+Block walkthrough verification: `df -h /` shows expected size on dst, F2 split-brain not triggered (PR #35's storage-level invariant), Longhorn-migratable StorageClass cluster-state correct. | **[Walk]** | Phase 3a-specific; PR #35 |
| E13 | Network blackhole mid-transfer: silent drop of TCP between src and dst pods (iptables DROP on src→dst path, not REJECT). Assert kernel TCP retransmit ~127s, then `failureReason=Timeout` after `spec.timeout`. | **[Walk]** | Q3-v3 spike |
| E14 | Source-node hard failure mid-migration: `kubectl drain` source node (Phase 4 webhook absent in Phase 3a). Assert eventual `failureReason=SourcePodReplaced` via UID-change. | **[Defer]** | Phase 4 (drain integration) |
| E15 | Resuming-gate timeout (distinct from E3 listener-timeout): src writes `migration-status=complete` and cutover succeeds, but dst pod's `GuestRunning=True` condition never appears (simulate by holding swiftletd-on-dst at receive-accept; CH crashes during resume). Assert phase stays in Resuming until `spec.timeout`, then Failed with `failureReason=Timeout`. Distinct from E3 (which times out at listener-accept BEFORE src-complete). | **[CI]** | §3.6, §4.3 |
| E16 | RBAC sufficiency on real cluster: deploy controller-manager with `swiftguests/status` patch verb intentionally omitted (single-line ClusterRole edit). Run E1; observe the precise failure mode (cutover step 1 hangs with apiserver Forbidden). Tagged [Walk] because intentional RBAC sabotage is fragile in CI; the W7/W8 walkthrough discipline catches what fake-client tests cannot. | **[Walk]** | W7/W8 lesson; §5.2 |
| E17 | PodRefSwapped condition correctness: in E1, assert the `PodRefSwapped=True` condition is set with `lastTransitionTime` AFTER cutover step 1's status patch on SwiftGuest succeeds AND BEFORE src pod delete (step 2) is dispatched. In E2b, assert new leader reads PodRefSwapped from `status.conditions` and uses it correctly in `isPostCutover`. | **[CI]** | §6.3, §5.3 |

**Mapping to spike scenarios**: Q1 → E1+E2a+E2b+E2c; Q2 →
covered by E1's post-migration informer-latency assertions; Q3 →
E3+E13+E15; Q4 → E4+E5+E6+E14.

**E1 + E2a/b/c + E3 + E15 are the minimum CI-blocking set.**
Without these, a Phase 3a regression in cutover ordering,
leader-handover recovery at any of three crash sites, listener-
timeout handling, or Resuming-gate timeout could ship undetected.
E4-E10, E12, E17 are also CI-blocking because each maps to a
distinct `status.failureReason` enum value, webhook rule,
storage-runtime path, or load-bearing condition (PodRefSwapped)
that has no other test coverage.

**E11 + E12-walk + E13 + E16 are walkthrough-tagged** for two
reasons: (a) E11 + E16 exercise operator UX / RBAC surfaces that
benefit from a human reading the output; (b) E12-walk requires
post-boot in-guest verification that automated assertions are
brittle for; (c) E13 requires careful iptables manipulation that
is fragile in CI but straightforward as an operator walkthrough.
The mini-walkthrough pattern (W3/W4/W6/W8/W9/W10/W11) has
consistently caught real issues; explicitly tagging walkthrough
scenarios keeps that signal alive for Phase 3a.

**Fresh-namespace gate**: Phase 3a's first walkthrough MUST run
in a fresh non-`default` namespace. The W3/W7/W8 pattern is
durable signal — per-namespace RBAC, finalizer, and canonical-pod-
name issues that fake-client tests cannot surface only appear in
namespace bootstrapping. Codifying this as a Phase 3a-specific
gate (not folklore) prevents the W3/W7/W8 lesson from regressing.
Walkthrough sign-off requires the operator to attest the
walkthrough namespace was not `default` and was created cleanly
for the walkthrough run.

#### CI wiring

- Path-touch trigger added to `e2e-on-cluster.yaml` for
  `internal/controller/swiftmigration/**`,
  `api/migration/v1alpha1/**`,
  `cmd/swiftctl/migration**`,
  `rust/swift-ch-client/src/**` (D1+D2+D3 affect this path).
  Phase 3a tests run on every PR touching these paths.

### 8.3 Documentation deliverables

Phase 3a's first PR includes the following documentation updates:

- This design doc (`docs/design/live-migration-phase-3a.md`) is the
  primary deliverable.
- `docs/migration/overview.md`: add `mode: live` paragraph;
  cross-reference Phase 3a design doc; document the
  post-migration pod-name change as a behavior callout.
- `docs/migration/troubleshooting.md`: add sections for
  - "guest-ip not appearing post-migration" (read
    `status.targetIP` instead of waiting for fresh DHCP)
  - "kubectl logs by guest-name fails post-migration" (the
    foot-gun explanation; recommend `swiftctl logs`)
  - "CancelIgnored condition appeared" (operator-visible
    explanation of why post-cutover cancel is no-op)
- `kubeswift_context.md`: Phase 3a Decisions Resolved subsection
  updated with the must-have-before-ship checklist refined per
  §8.1 above.
- `CHANGELOG.md`: Phase 3a entry covering CRD additions, API-
  group additions, behavior changes (post-migration pod name).

### 8.4 Cross-reference health

This subsection lists every cross-reference made by this design
doc, for reviewer audit on Day 4-5.

**Internal (within this doc)** — file order is §1 → §2 → §3 → §4
→ §5 → §6 → §7 → §8 (post-edit-pass; earlier drafts had §3 and §4
swapped in file order):

- §0 glossary → all F/S/W/MH/OQ/G/D/E/B references throughout
- §1 → §3.4 (swiftctl resolution)
- §1 → §7.3 (F1.1 / F2.5 / CPU non-dependencies)
- §2.3 → §3.5 (cutover ordering belt-and-suspenders)
- §2.3 → §3.6 (Resuming gate forward-pointer)
- §2.3 → §5.3 (Cancelled controller-side handling and dst-side cancel primitive)
- §2.3 → §7.2 D1 (cancel implementation contract)
- §2.4 → §6 (status fields used in recovery)
- §3.3 → §5.2 (RBAC + defense-in-depth `resolveCanonicalPod`)
- §3.6 → §7.2 D3 (guest-ip propagation swiftletd dependency)
- §3.6 → §4.3 (Resuming gate uses spec.timeout, not 30s soft target)
- §3.7 → Phase 4 (PDB/eviction-webhook handoff)
- §4.2 → §2.3 cutover (UID-check scope)
- §4.7 → §2.3 Cancelled (three cancel rows)
- §5.1 → §5.5 (SyncPeriod choice as defense-in-depth, not primary)
- §5.3 → §6.3 (PodRefSwapped condition usage)
- §5.3 → §7.2 D1 (dst-side cancel)
- §5.4 → §3.2 (242-char rule)
- §6.4 stability discipline → cross-phase contract
- §7.1 → Phase 3b (S1 URL deprecation)
- §7.2 D1 → §2.3 Cancelled
- §7.2 D3 → §3.6 + SECURITY-S1
- §8.1 MH-C2 → §2.3, §3.5 (cutover invariant)
- §8.1 MH-G1 + MH-G2 → §3.3 (canonicalPodName + defense-in-depth)
- §8.2 E2a/b/c → §2.4 (recovery cases enumerated)
- §8.2 E15 → §3.6, §4.3 (Resuming-gate timeout)
- §8.2 E16 → §5.2 (RBAC sufficiency)
- §8.2 E17 → §6.3, §5.3 (PodRefSwapped condition)
- §8.2 fresh-namespace gate → W3/W7/W8 lesson

**External**:

- `docs/design/live-migration-phase-3a-spike.md` — F-numbers
  throughout
- `docs/design/live-migration-phase-2.md` — annotation surface,
  PR-A/B/C lineage
- `docs/design/live-migration.md` — Phase 1 baseline,
  per-operation discipline (PR #26)
- `docs/design/THREAT-MODEL.md` — S1 boundary, plaintext-ack gate
- `kubeswift_context.md` — Phase 3a Decisions Resolved
- `CLAUDE.md` — overall architecture (CH-first, raw disk runtime)

The Day 4-5 sub-agent reviews must verify that every reference
above resolves to actually-present content. If any reference has
drifted (e.g., section moved or renamed), the design doc is the
authoritative source and the cross-reference must be updated to
match the doc as it ships.

---

End of Phase 3a design doc.
