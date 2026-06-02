# Live Migration Phase 4 — Drain Integration (Design)

> Status: design-locked (decisions resolved 2026-06-02). Spike COMPLETE.
> Implementation pending across the PRs in §9.

## 1. Goal & non-goals

**Goal.** `kubectl drain <node>` (and any eviction-API caller — cluster-
autoscaler, node upgrades) **automatically and safely evacuates SwiftGuest
VMs**: the guest is migrated off the node (live where possible, offline for
VFIO/GPU), and the eviction is blocked until the guest is gone. A VM is
**never evicted-to-death**.

**Non-goals.**
- No new migration *transport* (Phase 4 reuses the Phase 1 offline and
  Phase 3a/b/c live paths verbatim).
- No scheduler replacement — target selection is a simple capacity pick.
- No multi-guest batch orchestration beyond "evacuate each guest on the
  node independently."

## 2. Spike findings (validated on cluster, miles/boba)

A throwaway `pods/eviction` deny-webhook (`spike/phase-4-eviction-webhook`,
NOT for merge) validated the core mechanism:

- A `ValidatingWebhook` on `pods/eviction` **does** intercept `kubectl
  drain`'s evictions.
- **Denying with `429 TooManyRequests` makes drain retry every 5s** — the
  same code path the eviction API uses for PodDisruptionBudget blocks —
  cleanly, with no hard failure. drain output: `error when evicting ...
  (will retry after 5s): admission webhook denied the request`.
- Drain's default `--timeout` is **infinite** (retries until the pod is
  gone), so a ~38s live migration + cutover fits comfortably; the webhook
  just keeps denying until cutover deletes the source pod, after which the
  next retry sees `pod not found` → allow → drain proceeds.
- The webhook fires on **every** eviction cluster-wide; the handler
  fast-allows non-guest pods via a cheap cached `Get`.

These shape the design below (5s retry granularity is plenty; deny-with-429
is the right primitive; the allow-after-cutover transition is automatic).

## 3. Architecture — webhook marks, controller creates, PDB guarantees

Three pieces:

```
 kubectl drain node-A
        │  POST pods/eviction (every 5s)
        ▼
 ┌──────────────────────────┐    stamp drain-requested      ┌─────────────────────┐
 │ Eviction webhook         │ ───────────────────────────►  │ SwiftGuest           │
 │ (pods/eviction, CREATE)  │    on the SwiftGuest           │ (annotation marker)  │
 │ - guest pod? deny(429)   │                               └──────────┬──────────┘
 │ - non-guest/gone? allow  │                                          │ watch
 └──────────────────────────┘                                          ▼
        ▲  (eviction also blocked by)                        ┌─────────────────────┐
        │                                                    │ Drain controller    │
 ┌──────┴───────────────────┐                                │ - create Migration  │
 │ per-guest PodDisruption  │                                │   (auto/live/offline)│
 │ Budget (maxUnavailable:0)│                                │ - clear marker when  │
 │ — the HARD guarantee     │                                │   guest off the node │
 └──────────────────────────┘                                └─────────────────────┘
```

1. **Eviction webhook** (`pods/eviction` CREATE; `failurePolicy: Ignore`;
   `sideEffects: NoneOnDryRun`). For a SwiftGuest launcher pod:
   - migratable (`drainPolicy` ∈ {Migrate, LiveMigrate}, `migration.enabled
     != false`) → stamp `kubeswift.io/drain-requested: <node>` on the
     SwiftGuest (skip the patch on dry-run) and **deny (429, retry)**.
   - `drainPolicy: Block` or `migration.enabled: false` → **deny (429)**
     with a "cannot auto-migrate; handle manually" message (no marker, no
     migration).
   - not a SwiftGuest pod, or pod already gone → **allow**.

2. **Drain controller** (watches SwiftGuests). On `drain-requested`
   present and no migration in flight for the guest → create a
   `SwiftMigration` (deterministic name) with the mode resolved from
   `drainPolicy` (§4.3) and the target node from §4.2. Clears the marker
   once the guest is off the draining node (migration Completed / source
   pod gone).

3. **Per-guest PodDisruptionBudget** (`maxUnavailable: 0`), created by the
   SwiftGuest controller for protected guests. This is the **hard
   guarantee, independent of the webhook**: the eviction API blocks on it.
   The migration's cutover **`Delete`s** the source pod (a direct Delete,
   not an eviction), which bypasses the PDB — so the guest moves and drain
   proceeds. Failure modes (§7): webhook up → smart auto-migrate; webhook
   DOWN → drain stalls **safely** (PDB still blocks; VM protected).

## 4. Decisions resolved

### 4.1 Webhook-marks / controller-creates (not webhook-creates)

The webhook is a thin admission gate that persists a marker; all
`SwiftMigration` creation lives in a controller. Keeps the webhook's
side-effect minimal (one annotation patch, `NoneOnDryRun`) and the
CR-creation logic in the reconcile loop where retries/races are natural.

### 4.2 Defense-in-depth: webhook + per-guest PDB, `failurePolicy: Ignore`

`failurePolicy: Ignore` so a webhook outage **never** breaks cluster-wide
evictions. The `maxUnavailable: 0` PDB is the floor that protects the VM
even when the webhook is down. (Rejected: webhook-only — a webhook outage
either kills the VM (`Ignore`) or breaks all evictions (`Fail`).)

### 4.3 Per-guest `spec.migration.drainPolicy`

New field `spec.migration.drainPolicy`, enum, default `Migrate`:

| Value | Drain behaviour |
|---|---|
| `Migrate` (default) | `mode=auto` — live where possible, **offline** (bounded downtime) otherwise. Drain always succeeds **for non-VFIO guests**. |
| `LiveMigrate` | live only; if the guest can't live-migrate, **deny the drain** (block) rather than incur downtime. |
| `Block` | always deny the drain; operator handles the guest manually. |

`migration.enabled: false` is orthogonal and stronger — it disables
migration entirely; drain denies with a manual-handling message.

> **VFIO/GPU scope correction (initial Phase 4).** The original §4.3 text
> promised `Migrate` does "offline for VFIO/GPU." That is **not deliverable
> in the initial Phase 4**: the SwiftMigration webhook still rejects ALL
> VFIO/GPU cross-node migration (`internal/webhook/swiftmigration/validator.go`
> — "Phase 4+ work pending a release-and-reallocate primitive"), and that
> primitive (SwiftGPU deallocate-on-source + reallocate-on-target) does not
> exist. Until it ships, **VFIO/GPU guests block the drain under ANY
> drainPolicy** (the eviction webhook denies them with a manual-handling
> message and does NOT mark them — a marker would drive the drain controller
> to create a webhook-rejected migration every 5s). Building the
> release-and-reallocate primitive is a follow-on sub-phase (decision
> 2026-06-02: build it next, after the non-VFIO drain ships); it cannot be
> cross-node-validated on the current single-GPU-node cluster (validation:
> same-node release→reacquire on boba + mocked second SwiftGPUNode in
> unit/envtest). Tracked in `kubeswift_context.md`.

### 4.4 Target-node selection

The drain controller picks the schedulable peer node (excluding the
draining/cordoned node) with the most headroom, reusing the offline-path
`checkNodeCapacity`. **No schedulable target** → keep denying with a "no
target with capacity" message (drain stalls safely; operator frees
capacity or `--force`s).

### 4.5 Consumes TFU #24 (the `lifecycle: run` freeze)

Drain is the canonical **stop-during-migration** path: an eviction-driven
stop could coincide with a live migration's dst receiver. The W-3c-1
`lifecycle: run` freeze on the dst intent (tracked as TFU #24, deferred
through Phase 3c as not-reachable) **must land here** — Phase 4 is where it
becomes reachable. See TFU #24 + `dst_pod.go::newDstPod`.

## 5. CRD change

`api/swift/v1alpha1` — add to `MigrationSpec`:

```go
// DrainPolicy controls how kubectl drain / the eviction API evacuates
// this guest. +kubebuilder:validation:Enum=Migrate;LiveMigrate;Block
// +kubebuilder:default=Migrate
DrainPolicy string `json:"drainPolicy,omitempty"`
```

Requires `make generate` + chart CRD sync (per the recurring lesson:
enum fields must be regenerated, not just constant-added — Phase 3c PR 5).

## 6. RBAC

- Webhook: `pods/eviction` admission; `get` pods; `get,patch` swiftguests
  (the marker).
- Drain controller: `get,list,watch,patch` swiftguests; `create,get,list`
  swiftmigrations; `get,list,watch` nodes + pods (target capacity);
  `create,get,update,delete` poddisruptionbudgets (policy/v1).
- Cached-client `list,watch` on every GET-ed resource (the W7/W8 lesson —
  poddisruptionbudgets + nodes need `list,watch`, not just `get`).

## 7. Failure modes

| Mode | Behaviour |
|---|---|
| Webhook up, migratable guest | mark → migrate → cutover deletes src → drain proceeds |
| Webhook **down** (`Ignore`) | admission skipped → PDB still blocks the eviction (429) → drain stalls safely; VM protected; no auto-migration (operator notices, investigates) |
| No schedulable target | webhook keeps denying with a clear message; drain stalls; operator frees capacity |
| Migration fails mid-drain | marker stays; controller surfaces the failure on the SwiftMigration; webhook keeps denying (VM unharmed — live pre-copy never pauses the source) |
| `drainPolicy: Block` / `migration.enabled=false` | deny with manual-handling message; operator `--force`s or moves manually |
| Dry-run eviction (`drain --dry-run`) | webhook denies (shows it would block) but skips the marker patch (`NoneOnDryRun`) |

## 8. Open implementation sub-decisions

- **PDB scope (RESOLVED: universal, per-guest).** Shipped in PR 4b: the
  SwiftGuest controller creates a `maxUnavailable: 0` PDB for **every** guest
  with a launcher pod (migratable, `Block`, and `migration.enabled: false`
  alike — a VM is never evicted-to-death regardless of policy). Created only
  past the pod-ensure block (NOT before the launcher pod exists); selects the
  launcher pod by `swift.kubeswift.io/guest` (so protection follows the guest
  across a live-migration pod rename); owned by the guest (GC on delete).
  SwiftGuestPool replicas get a per-pod PDB each (uniform, simpler than
  pool-level). The PDB does not impede the happy-path drain — the webhook
  denies and the migration cutover `Delete`s the source pod (a Delete, not an
  Eviction); the PDB only bites when the webhook is down.
- **Marker on the SwiftGuest vs the pod:** the SwiftGuest (survives the
  cutover pod rename; the controller already reconciles it).
- **Re-drain idempotency:** the deterministic SwiftMigration name +
  `migration-in-progress` annotation (Phase 1) prevent duplicate
  migrations across the 5s eviction retries.

## 9. Implementation plan (PRs)

1. **PR 1** (#87, merged) — this design doc + the `drainPolicy` CRD field
   (+ generate + chart sync). Design + API surface only.
2. **PR 2** (#88, merged) — the TFU #24 `lifecycle: run` freeze on the dst
   intent (now reachable: Phase 4 introduces the stop-during-migration
   path). Split out because it touches `newDstPod` / dst-pod construction
   and is logically independent of drain.
3. **PR 3** (#89, merged) — the eviction webhook (`pods/eviction` admission
   handler, VWC rule, `failurePolicy: Ignore`, marker patch with dry-run
   skip). Unit-tested; marker inert until PR 4a.
4. **PR 4a** (#90, merged) — the drain controller (marker → SwiftMigration →
   clear + target selection). Handles **non-VFIO** guests; VFIO/GPU guests
   are denied-without-marking by the eviction webhook (VFIO correctness fix
   folded in here, since 4a makes the marker live). Reuses the migration
   Validating phase's `NodeHasCapacity` gate. Unit-tested.
   *(Split from the original single PR 4 per the same Design-Principle-#1
   reviewability discipline used throughout Phase 4.)*
5. **PR 4b** (#91, merged) — universal per-guest `maxUnavailable: 0`
   PodDisruptionBudget creation in the SwiftGuest controller (the hard floor,
   independent of the webhook/controller; §4.2). Logically independent of 4a.
6. **PR 5** (SHIPPED) — cluster walkthrough + operator runbook
   (`docs/migration/phase-4.md`) + drainPolicy samples. Validated on
   miles/boba (image sha-04c054d): drain → live-migrate to boba
   (observedDowntime 2.30s) → drain completes; `Block` deny; webhook-down PDB
   safety; per-guest PDB. All PASS, no bugs. VFIO-offline deferred to the
   release-and-reallocate sub-phase.

### Follow-on sub-phase — VFIO/GPU release-and-reallocate

Builds the missing primitive so VFIO/GPU guests can be offline-evacuated on
drain (decision 2026-06-02: build it after the non-VFIO drain ships). Its
own design doc + spike + PRs. Surface (from recon):
- **New SwiftGPU capability**: allocate on a *specific* requested node (today
  `findAndAllocate` auto-picks the first node with capacity) + release-from-
  node, exposed for the migration controller to call.
- **GPU target pre-flight**: target node must have free GPUs matching the
  profile (count/model/tier/NUMA/FM partition) — a GPU analogue of
  `NodeHasCapacity`, in both drain target-selection and the migration
  Validating phase.
- **Two-phase atomicity**: reserve target GPUs *before* stopping the source
  (Phase 1's "drive-forward post-cutover, restore pre-cutover"), else a
  failed realloc strands a stopped, GPU-less guest.
- **Lift the webhook VFIO rejection** for offline mode only (live + VFIO
  stays blocked).
- **FM partition handoff** (Tier 2/3): deactivate on source, activate on
  target.
- **Validation**: same-node `boba→boba` release→reacquire on the real GTX
  1080 + mocked second `SwiftGPUNode` in unit/envtest. Cross-node GPU
  migration **cannot** be hardware-validated on the single-GPU-node cluster;
  ships explicitly labeled as such.
