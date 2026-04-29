# Live Migration Phase 2 — swiftletd Plumbing Design

> **Status:** Design — implementation pending
> **Audience:** Phase 2 implementer (rust-runtime-engineer), Phase 3 controller author, security review, operator-walkthrough validators
> **Last updated:** 2026-04-29
> **Prerequisites (read in order):**
> 1. `docs/design/live-migration.md` — overall live-migration design (the Phase 2 section there is the high-level plan; this doc fleshes it out)
> 2. `docs/design/live-migration-phase-2-spike.md` — the empirical spike findings that resolved the four pending Phase 2 decisions
> 3. `docs/design/snapshots.md` — snapshot Phase 2 is the closest implementation precedent for swiftletd's annotation-driven action loop
> 4. `kubeswift_context.md` — current project state and the Phase 2 must-have-before-ship checklist
>
> This doc is the contract between the spike and the implementation. Everything that follows is grounded in spike findings F1–F12, S1–S4, W1–W2, and OQ1–OQ7.

---

## 1. Goal and Non-goals

### Goal

Phase 2 ships **swiftletd-side migration plumbing** for Cloud Hypervisor cross-node live migration: a destination "awaiting migration" launcher pod mode, a source-side send-migration handler, an annotation-driven control surface, and a manual operator demonstration path that exercises the new surface end-to-end.

Phase 2 is independently shippable as **swiftletd plumbing in isolation**. It produces no new operator-facing UX, no controller orchestration, and no automatic migration triggering. Phase 3 wires the SwiftMigration controller through this plumbing; Phase 4 adds drain integration; Phase 5 adds operational polish.

The Phase 2 deliverable proves the swiftletd contract with the same shape Phase 3's controller will consume, so Phase 3 becomes mechanical rather than exploratory.

### Non-goals

- **No SwiftMigration controller integration.** The existing SwiftMigration controller from Phase 1 (offline migration only) is unchanged in Phase 2. Live mode is wired in Phase 3.
- **No mTLS.** Phase 2 carries unauthenticated guest state in cleartext on the cluster network. mTLS is Phase 3 work.
- **No SwiftMigration CRD schema changes.** Phase 2 does not add fields to the existing CRD beyond what is strictly necessary for the manual demo path (currently zero new fields — Phase 2 operates entirely via launcher-pod annotations).
- **No SwiftGuest CR mutations in the Phase 2 manual path.** Phase 1 preserves SwiftGuest CR identity across migration; Phase 2's manual demo MUST NOT touch SwiftGuest CRs at all. This is an isolation invariant — without it, the implementer would entangle Phase 2 with Phase 1's controller behavior. Operators in the Phase 2 manual path apply launcher pod YAML directly. (This non-goal is reasserted in §7's manual-demo plan.)
- **No pre-copy convergence tuning.** CH v51.1's hardcoded 5-iteration cap is the convergence gate; there is no operator-tunable knob. `spec.maxPauseWindow` is a Phase 3 SwiftMigration field; Phase 2 swiftletd records the observed pause window into a status annotation but enforces no policy.
- **No drain integration / eviction webhook.** Phase 4.
- **No GPU / VFIO / SR-IOV migration.** Constraint 1 of the live-migration design — VFIO blocks live migration upstream. Phase 1's webhook already rejects these; Phase 2 inherits that rejection.

### Phase 2/3 boundary in one sentence

Phase 2 ships `swiftletd` action handlers that an operator can drive by hand via `kubectl annotate`; Phase 3 ships the `SwiftMigration` controller that drives those same actions automatically.

---

## 2. The Eight-Action Contract

The control surface from spike Resolved Decision 1. Each row defines one action: **initiator** (who writes the trigger annotation), **trigger** (the annotation key + value that fires the action), **observable** (what the consumer reads to know the action completed), and **failure mode** (the spike-evidence-grounded error path).

Phase 2 ships seven explicit handlers; **`wait-keepalive` is implicit in the blocking semantics of `start-receive` and is not a separate handler**. It is documented here for completeness, marked `(implicit)`, so the implementer does not build a redundant handler.

| # | Action | Initiator | Trigger annotation | Observable | Failure mode |
|---|---|---|---|---|---|
| 1 | **prepare-destination** | Pod-builder (init containers), NOT swiftletd | None — happens at pod startup before swiftletd starts CH | Pod's init containers reach `Ready`; `tap` interface and PVC mounts exist on the destination node | Init container failure → Pod stuck in `Init`; swiftletd never starts; controller observes via Pod status (Phase 3) or operator observes via `kubectl describe pod` (Phase 2). Spike F5 evidence. |
| 2 | **start-receive** | Controller (Phase 3) / operator (Phase 2 manual) writes annotation on **dst pod** | `kubeswift.io/migration-action: receive` + `kubeswift.io/migration-action-id: <ulid>` on the destination launcher pod | swiftletd writes `kubeswift.io/migration-status-id: <ulid>` + `migration-status: listening` once the CH listener is bound | If dst CH has not been started, or `--api-socket` is absent, swiftletd writes `migration-status: failed` with a detail explaining the precondition. If `receive-migration` fails (e.g., port already bound — F4), swiftletd writes `migration-status: failed` with the underlying error string. Spike Q1c, F4 evidence. |
| 3 | **start-send** | Controller / operator writes annotation on **src pod** | `kubeswift.io/migration-action: send` + `kubeswift.io/migration-action-id: <ulid>` + `kubeswift.io/migration-target-url: tcp:<dst-ip>:<port>` (URL source: see §3 and §8 for S1 mitigation) | swiftletd writes `migration-status-id: <ulid>` + `migration-status: precopy` once `ch-remote send-migration` is in flight; `migration-status: complete` on clean exit; `migration-status: failed` on error | If the src CH is not Running, swiftletd rejects with `migration-status: failed`. If `send-migration` returns non-zero (network drop F3, dst gone F2, version mismatch F11/F12), swiftletd captures the error string and writes `failed`. Spike F1, F2, F3, F11, F12 evidence. |
| 4 | **report-progress** | swiftletd on src pod (no controller trigger) | None — emitted automatically while `start-send` is in flight | swiftletd polls local `vm.info` API at 2 s cadence and writes `migration-progress: <state>` (states: `precopy`, `stopcopy`, `complete`). Phase 2 uses **info-state polling**, NOT `--log-file` parsing (F8 + S4 + load-bearing item B — see §4). | If `vm.info` polling fails (CH unresponsive), swiftletd logs but does not crash — the next tick retries. If polling never recovers, the eventual `start-send` exit covers the failure path. |
| 5 | **report-complete** | swiftletd on both pods | None — emitted on observation of terminal CH state | **Source pod**: swiftletd observes its CH process has exited cleanly (Q1c — src auto-exits on successful migration); writes `migration-status: complete`. **Destination pod**: swiftletd observes its CH `vm.info` has transitioned to `state=Running` (Q1c — dst auto-resumes); writes `migration-status: running` + records `kubeswift.io/migration-pause-window-ms: <observed-ms>`. | Source pod's CH exit code is the only "complete" gate on src. Destination pod's `state=Running` is the only "complete" gate on dst. **`send-migration` exit=0 on src is necessary but not sufficient — see §6 W1 invariant.** |
| 6 | **report-failed** | swiftletd on either pod | None — emitted on any non-clean terminal observation | swiftletd writes `migration-status: failed` + `migration-status-detail: <error string>`. Phase 2 manual path: operator reads the detail. Phase 3 controller: maps to SwiftMigration `phase=Failed`. | n/a (this is itself the failure path) |
| 7 | **cancel** | Controller / operator writes annotation on **dst pod** | `kubeswift.io/migration-action: cancel` + `kubeswift.io/migration-action-id: <ulid>` | swiftletd writes `migration-status-id: <ulid>` + `migration-status: cancelled`; **kills the local CH process** (the dst-kill cancel primitive — Q1d-F2). On the source side, the in-flight `send-migration` returns `Connection refused` and the source CH guest **automatically resumes** running. | If swiftletd cannot SIGKILL its CH child (process already gone), it still reports `cancelled` — the desired effect was already achieved. Spike F2 evidence. |
| 8 | **wait-keepalive** | *(implicit)* | None — implicit in `start-receive`'s blocking semantics | `ch-remote receive-migration` blocks until source connects, completes, or the listener gives up (F4: a few seconds on network silence). swiftletd does not have a separate "keepalive" handler — it just must not time out the receive call too aggressively. | The destination listener self-terminates on TCP retransmit timeout (F4). Phase 2 swiftletd's `receive-migration` invocation has no application-level timeout; CH's TCP layer is the timeout source. Phase 3's controller heartbeat budget must be measured in seconds, not minutes. |

### 2.1 Why no `spec.maxPauseWindow` enforcement in Phase 2 (load-bearing item A)

CH v51.1 does not expose a pre-hoc dirty-rate estimation API. The spike (Q2) shows that pre-copy is hardcoded at 5 iterations (**F6**: CH caps pre-copy at 5 iterations regardless of dirty rate); high-dirty-rate workloads emerge from pre-copy with stop-and-copy ≈ one iteration window of dirty pages, not failure. The realized vCPU-paused window can only be measured post-migration (**F7**: vCPU-paused window 0.5–5 s for typical workloads; operator-visible BEACON gap 20–40 s — these are distinct numbers).

Phase 2's `report-complete` records the observed pause window into `kubeswift.io/migration-pause-window-ms` for the controller (Phase 3) to enforce policy against. Phase 2 itself enforces no policy — it just reports. **Operators running Phase 2's manual path must not assume a `maxPauseWindow` hint anywhere will pre-empt a long migration.** Phase 3's SwiftMigration `spec.maxPauseWindow` is a *post-hoc* admission criterion: if observed > spec, mark `phase=Failed` and let the source resume (F2). Pre-hoc estimation is deferred until/unless operator demand surfaces.

---

## 3. Annotation Surface Specification

### 3.1 Annotation keys

**Naming note**: the status-id key is named `migration-status-id` (no `-mirror` suffix), matching the existing snapshot Phase 2 precedent (`STATUS_ID_KEY = "kubeswift.io/snapshot-status-id"` in `rust/swiftletd/src/action.rs:85`). The "mirror" semantic — swiftletd echoes the controller's `migration-action-id` back to signal observation — is preserved in this doc's prose ("the status-id-mirror invariant") but is not in the key name.

**Source-side launcher pod** (the pod that contains the running CH):

| Key | Writer | Read by | Purpose |
|---|---|---|---|
| `kubeswift.io/migration-action` | Operator (Phase 2 manual ONLY); Phase 3 deletes this annotation key (see §8.2.5) | swiftletd | Action verb: `send`, `cancel` |
| `kubeswift.io/migration-action-id` | Operator (Phase 2 manual ONLY); Phase 3 reads from SwiftMigration CR | swiftletd | Idempotency key — opaque ULID per action attempt (referred to as **action-id** elsewhere in this doc) |
| `kubeswift.io/migration-target-url` | Operator (Phase 2 manual ONLY); Phase 3 deletes this annotation key (see §8.2.5) | swiftletd | `tcp:<dst-ip>:<port>` — annotation-trust-boundary risk addressed in §8.2.3 |
| `kubeswift.io/migration-status` | swiftletd | Controller / operator | Lifecycle state: `precopy`, `stopcopy`, `complete`, `cancelled`, `failed`, `rejected` |
| `kubeswift.io/migration-status-id` | swiftletd | Controller / operator | Echoes the action-id of the action this status corresponds to. **swiftletd writes `migration-status-id` BEFORE writing `migration-status`** (so the controller cannot observe a status without its associated action-id). |
| `kubeswift.io/migration-status-detail` | swiftletd | Controller / operator | Free-form error/diagnostic text on `failed` and `rejected`. **swiftletd MUST sanitize**: capture only error category, exit code class, and CH error variant — NEVER raw stderr or memory-region offsets (defensive against future failure modes that could leak partial guest state; see §8 S3-related guidance). |
| `kubeswift.io/migration-pause-window-ms` | swiftletd | Controller | Observed vCPU-paused window in ms, set on `complete` |

**Destination-side launcher pod** (the pod awaiting migration):

| Key | Writer | Read by | Purpose |
|---|---|---|---|
| `kubeswift.io/migration-action` | Operator (Phase 2 manual ONLY); Phase 3 deletes this annotation key (see §8.2.5) | swiftletd | Action verb: `receive`, `cancel` |
| `kubeswift.io/migration-action-id` | Operator (Phase 2 manual ONLY); Phase 3 reads from SwiftMigration CR | swiftletd | Idempotency key (action-id) |
| `kubeswift.io/migration-listen-url` | Operator (Phase 2 manual ONLY); Phase 3 deletes this annotation key (see §8.2.5) | swiftletd | `tcp:0.0.0.0:<port>` — annotation-trust-boundary risk addressed in §8.2.3 |
| `kubeswift.io/migration-role` | Pod-builder (set at pod-create, NOT after) | swiftletd | Documentation-only mirror; the **load-bearing** signal is the env var `KUBESWIFT_MIGRATION_ROLE=receiver` set on the swiftletd container (see §4.3.2 for rationale). swiftletd does NOT read this annotation at startup. |
| `kubeswift.io/migration-status` | swiftletd | Controller / operator | `listening`, `running`, `cancelled`, `failed`, `rejected` |
| `kubeswift.io/migration-status-id` | swiftletd | Controller / operator | Same invariant as src |
| `kubeswift.io/migration-status-detail` | swiftletd | Controller / operator | Sanitized diagnostic, same rules as src |

**Phase 2 gating annotation** (S2 — see §8):

| Key | Writer | Read by | Purpose |
|---|---|---|---|
| `kubeswift.io/migration-phase2-unsafe-plaintext` | Operator (must be set to literal `ack` on the SOURCE launcher pod) | swiftletd | Required to enable the swiftletd migration handlers. swiftletd refuses any `send` or `receive` action on a pod missing this annotation. Removed in Phase 3 once mTLS lands. |

### 3.2 The action-id-mirror invariant (load-bearing item E)

**Same `migration-action-id` value across the entire migration's lifecycle.** swiftletd writes `migration-status-id = <action-id>` paired with each `migration-status` transition. swiftletd writes `migration-status-id` BEFORE `migration-status`; the controller (Phase 3) refuses to consume a status whose `migration-status-id` ≠ its current `migration-action-id`.

This invariant is the snapshot Phase 2 Bug 14 precedent (`internal/controller/swiftsnapshot/local.go`, PR #14). In snapshot Phase 2, an early implementation re-rolled the action-id across status patches; the controller observed an action-id in the status that didn't match its own latest action-id and treated each patch as a fresh action. The fix was to lock the action-id at the start of the operation and only echo it back paired with the status, never alone.

Phase 2 swiftletd MUST replicate the same discipline: **the action-id is set once by the controller/operator at action initiation; swiftletd echoes it via `migration-status-id` paired with each well-defined transition (`listening`, `precopy`, `stopcopy`, `running`, `complete`, `cancelled`, `failed`, `rejected`).**

### 3.3 Annotation lifecycle

```
[t=0]   controller writes:  migration-action=send, migration-action-id=A1, migration-target-url=tcp:x:y
[t=0+ε] swiftletd reads, validates, dispatches send-migration
[t=2s]  swiftletd writes:   migration-status-id=A1, migration-status=precopy
[t=20s] swiftletd writes:   migration-status-id=A1, migration-status=stopcopy
[t=23s] swiftletd writes:   migration-status-id=A1, migration-status=complete, migration-pause-window-ms=4600
[t=∞]   controller observes status-id=A1 + status=complete; advances SwiftMigration phase
```

For `cancel`, the action-id must be a **NEW** ULID (different from the in-flight `send` or `receive` action-id). Reusing the same action-id would be filtered out by swiftletd's idempotency guard (snapshot Phase 2's `decide` function — see `rust/swiftletd/src/action.rs`). Cancel uses a fresh action-id; swiftletd echoes that fresh id back via `migration-status-id` when reporting `cancelled`.

### 3.4 Progress reporting cadence (F8 + load-bearing item B)

swiftletd polls its local CH's `vm.info` HTTP API at 2 s cadence (matches the existing snapshot Phase 2 action-loop poll cadence) and emits `migration-status` transitions as the CH state changes. **Phase 2 uses info-state polling, not `--log-file` parsing**, for two reasons:

1. **F8 (spike Q2c):** per-iteration progress (`Dirty memory migration N of 5`) is log-only in CH v51.1. Log parsing is fragile to upstream log-format changes and adds an attack surface (S4 noted log-file as a guest-escape spoofing vector).
2. **Load-bearing item B:** the `info` API is local to swiftletd's own pod's CH. Even with `spec.allowVersionSkew=true` enabled in Phase 3, swiftletd polls its OWN CH (always the same image as itself), never the peer's CH. So Phase 2's poll-based reporting is not affected by `ch-remote v51.1 vs CH v50.2` API drift discovered in spike Q4 setup notes.

Phase 3+ may add log-tailing for finer-grained `iter=N/5` reporting if operator demand surfaces. Phase 2 reports at coarse granularity: `precopy → stopcopy → complete`.

---

## 4. swiftletd Extension Architecture

### 4.1 Crate boundaries

**New methods in `swift-ch-client/src/methods.rs`** (extending the existing `ApiClient impl`):

```rust
impl ApiClient {
    /// Issue PUT /api/v1/vm.send-migration to the local CH.
    /// Blocks until CH returns; long-lived (seconds-to-minutes).
    /// Caller must use a generously-timed ApiClient.
    pub fn send_migration(&self, destination_url: &str) -> Result<(), ApiError>;

    /// Issue PUT /api/v1/vm.receive-migration to the local CH.
    /// Blocks until source connects and migration completes, OR until
    /// CH's TCP layer gives up (F4 — a few seconds of network silence).
    pub fn receive_migration(&self, receiver_url: &str) -> Result<(), ApiError>;
}
```

The wire format mirrors the existing `snapshot()` method — JSON body with a single URL field. The body shape is documented in CH's OpenAPI spec; both methods take a URL string and return success/error.

**No new crate.** swift-qemu-client is unaffected. Phase 2 is Cloud-Hypervisor-only; QEMU live migration via QMP is a separate future workstream (see live-migration.md "Out of Scope").

### 4.2 Action loop integration

swiftletd's existing action loop (`rust/swiftletd/src/action.rs`) handles `kubeswift.io/snapshot-action`. Phase 2 adds parallel handling for `kubeswift.io/migration-action`. Two clean implementation options were considered:

- **(A) Extend the existing action loop** with new action verbs — single loop, single dispatcher. Adds `MigrationSend`, `MigrationReceive`, `MigrationCancel` to the `ActionKind` enum.
- **(B) Separate action loop for migration** — two parallel loops on the same pod, each watching its own annotation key prefix.

**Phase 2 takes option A** (extend the existing loop). Reasons: (1) the action-id idempotency, decision-state machine, and rejection patterns are identical; duplicating them is pure waste. (2) Snapshot and migration are mutually exclusive at the *guest* level (you cannot snapshot a guest mid-migration), so a single loop is the natural place to enforce that mutual exclusion.

The existing `decide` function in `action.rs:199-236` reads hardcoded `ACTION_KEY` / `ACTION_ID_KEY` / `ACTION_ARGS_KEY` constants. Phase 2 must parameterize this:

- The constant block at `action.rs:79-86` becomes a `KeySet` struct (one for snapshot, one for migration, distinguished by namespace prefix).
- `decide(annotations, &KeySet, last_completed_id, in_flight_id)` takes the key-set as a parameter.
- `ActionState` gains per-namespace slots: `{ snapshot: NamespaceState, migration: NamespaceState }`. A single state slot with namespace as discriminator would force the controller to coordinate across two CRDs — wrong direction.

#### 4.2.1 Composition with snapshot Phase 2's action loop

Annotation namespacing is enforced by key prefix: `kubeswift.io/snapshot-*` for snapshot, `kubeswift.io/migration-*` for migration. swiftletd's poll tick runs `decide` against both key-sets independently:

- If both `snapshot-action` and `migration-action` are set on the pod with non-empty values, swiftletd writes a `rejected` status to **both** action namespaces, with `migration-status-detail: concurrent action with snapshot rejected` (and analogous on the snapshot side). The controller learns immediately that one operation must complete before the other starts.
- If only one is set, the loop dispatches normally to the corresponding handler.
- Each namespace's `last_completed_id` and `in_flight` slots are **independent** — finishing a snapshot does not reset migration state and vice versa.

**Rejection is per-tick, not sticky.** If at t=4 the snapshot completes and at t=4+ε the migration annotation is still on the pod, the next poll tick's `decide` will return `Accept(MigrationSend)` because the conflict is gone. The Phase 3 controller MUST NOT clear migration annotations on observed `rejected` status; let swiftletd re-evaluate when the conflict clears. (The operator in Phase 2 manual path likewise leaves the migration annotation in place.)

This composition is a hard invariant: a guest in the middle of `start-send` cannot simultaneously `capture` a snapshot. The latter would race CH's pause/resume cycle with the migration's stopcopy phase.

#### 4.2.2 Tokio runtime shape for migration handlers

The existing snapshot action loop runs on `Builder::new_current_thread()` (`action.rs:509-512`), which is sufficient for snapshot because pause+snapshot+resume runs to completion in seconds and blocking the loop during dispatch is acceptable. The dispatcher functions (`dispatch_capture`, `dispatch_resume`) are `async fn` whose bodies are sync — the `async` is decorative.

**Migration's `start-send` runs for tens of seconds and MUST NOT block the loop's ability to process `cancel`.** Phase 2's source-side `MigrationSend` handler dispatches `swift_ch_client::ApiClient::send_migration(...)` on a dedicated `std::thread::spawn` worker (mirroring the existing `spawn_action_loop` thread pattern at `action.rs:507-521`). A `tokio::sync::oneshot` channel signals completion back to the loop. The loop continues polling at 2s cadence and can dispatch `cancel` against an in-flight migration.

The destination-side `MigrationReceive` handler uses the same OS-thread + oneshot pattern. The progress-poll runs as a third concurrent task that periodically calls `client.vm_info()` against the same UDS socket — `request_ok` opens fresh UDS connections per call, so concurrent calls are safe.

This avoids forcing a runtime upgrade to `Builder::new_multi_thread()`, which would change the runtime semantics for the entire swiftletd process (lease poller, snapshot path, etc.). The OS-thread approach keeps blast radius small and matches existing snapshot scaffolding.

### 4.3 Process model

#### 4.3.1 Source pod

Existing model from Phase 1 unchanged: a launcher pod with a running CH instance (created via `vm.create` + `vm.boot` at swiftletd startup). swiftletd's action loop processes the `migration-action: send` annotation by invoking the new `ApiClient::send_migration(target_url)` method against the local CH.

`send_migration` blocks the caller's tokio task for the duration of the migration (seconds to minutes). swiftletd dispatches it on a dedicated worker task so the main poll loop continues to serve other annotations (cancel, in particular). On success, the source CH process exits cleanly (Q1c); swiftletd observes the exit via its existing CH-process-supervision path and writes `migration-status: complete`. On failure, `send_migration` returns an error; swiftletd writes `migration-status: failed` with the error string.

#### 4.3.2 Destination pod

**New mode**: a launcher pod that boots `swiftletd` but does NOT call `vm.create` or `vm.boot`. The receiver-mode discriminator is the env var `KUBESWIFT_MIGRATION_ROLE=receiver` set on the swiftletd container by the pod-builder (Phase 2 manual: hand-rolled YAML; Phase 3: controller). swiftletd reads the env var at startup, threads the role into the `RuntimeIntent` (or a sibling struct), and the branch lives in `launch.rs` alongside the existing `intent.is_restore()` branch (precedent: `rust/swiftletd/src/launch.rs:40`). **The branch does NOT live in `main.rs`** — `main.rs` stays unchanged for the seed-iso / lease-poller / action-loop scaffolding which the destination pod needs anyway.

Reading the launcher pod's own annotations at startup (instead of an env var) would add an apiserver dependency before swiftletd can boot CH; the existing pattern uses env vars and intent files. The annotation `kubeswift.io/migration-role: receiver` IS still set on the destination pod for documentation / `kubectl describe` discoverability, but it is not load-bearing.

When the receiver branch fires:

1. swiftletd calls `swift_ch_client::spawn_ch_receive(api_socket)` — a new sibling of `spawn_ch_restore` (`rust/swift-ch-client/src/spawn.rs:40`). `spawn_ch` is NOT used: it requires a full `VmConfig`; receive mode has none. Reusing `spawn_ch_restore` with a sentinel `source_url` is wrong because the wire shape differs (`--restore` is absent in receive mode).
2. The new `spawn_ch_receive` invokes CH with `--api-socket <path>` only — no `--cpus`, `--memory`, `--kernel`, `--disk` (Q1c).
3. swiftletd's action loop polls; on observing `migration-action: receive`, dispatches `ApiClient::receive_migration(listen_url)` on the `std::thread::spawn` worker (per §4.2.2).
4. On success, the CH automatically transitions to `Running` state with the migrated guest restored (Q1c). swiftletd polls `vm.info`, sees `state=Running`, and transitions into its normal monitoring mode (annotation-based status reporting per the Phase 1 pattern: `kubeswift.io/guest-runtime-pid`, `guest-serial-socket`, etc).
5. On failure, swiftletd writes `migration-status: failed` and exits its own process (the launcher pod terminates; controller observes via Pod status). There is no "retry receive" path — F1 / F3 in the spike show that fresh provisioning is required after any receive-side failure.

A unit test for `spawn_ch_receive` mirrors `restore_args_does_not_include_disk_or_network_flags` at `rust/swift-ch-client/src/spawn.rs:114-135` — assert that the emitted argv contains `--api-socket=<path>` and nothing else.

#### 4.3.3 Stale-socket cleanup (load-bearing item D — W2 walkthrough finding)

**Both the source and destination CH spawn paths MUST `rm -f` the API-socket file before invoking CH.** This is non-negotiable.

Rationale: CH does not clean up its `--api-socket` file on SIGKILL exit (a Linux process cannot run cleanup hooks under SIGKILL). If a prior CH instance was killed (e.g., the dst-kill cancel primitive — Q1d-F2), the socket file persists. The next CH invocation fails with `Address in use` and exits immediately — silent failure with confusing downstream symptoms. The walkthrough W2 finding confirmed this is the most-replicated failure mode in the spike: it surfaced in Q1, Q1d, Q1e, Q2, Q4, and the walkthrough run #1.

Implementation: the cleanup belongs in `swift-ch-client/src/spawn.rs`, in `spawn_ch`, `spawn_ch_restore`, and the new `spawn_ch_receive`, immediately before the `Command::spawn`:

```rust
// Pre-flight: remove any stale API socket from a prior CH that was
// SIGKILL'd (e.g., dst-kill cancel primitive). CH cannot clean up
// its own socket on SIGKILL. Without this, CH startup fails with
// "Address in use". See live-migration-phase-2-spike.md W2 finding.
let _ = std::fs::remove_file(api_socket);
```

The `let _ =` is intentional: a missing socket is the normal startup case; a stale socket is the post-SIGKILL case. Both lead to the same desired post-condition (no file exists at the path), so any error from `remove_file` is non-actionable.

**Pre-req for `spawn_ch`**: the existing `spawn_ch(config: &VmConfig)` at `rust/swift-ch-client/src/spawn.rs:12` does not receive an `api_socket` parameter — the path is buried inside `config.to_args()`. Phase 2 must add a `pub fn api_socket(&self) -> Option<&Path>` accessor on `VmConfig` so that `spawn_ch` can extract the path before invoking the cleanup step. The accessor is a one-line read of the existing `VmConfig` field; no behaviour change.

### 4.4 Shutdown / cleanup

On normal completion (source: `send-migration` returned 0; destination: `vm.info` reports `Running`):

- **Source pod**: CH exits cleanly. swiftletd's existing CH-supervision logic detects the exit. swiftletd writes terminal `migration-status: complete` and then (Phase 2 manual path) the operator deletes the source pod. (Phase 3: the controller deletes the source pod after observing the terminal mirror.)
- **Destination pod**: swiftletd transitions to normal monitoring mode. The pod continues running indefinitely as the new home of the guest.

On `cancel`:

- **Destination pod**: swiftletd `SIGKILL`s its CH child, writes `migration-status: cancelled`, and exits its own process (the launcher pod terminates). The PVC and tap are cleaned up by Kubernetes pod-deletion logic.
- **Source pod**: no annotation change required from the operator's side. The in-flight `send-migration` on the source returns `Connection refused`; swiftletd observes the error, writes `migration-status: failed` with detail `destination cancelled`. The CH on source automatically resumes the guest (F2). The source pod stays running.

On `failed`:

- swiftletd writes terminal status. The pod stays running until the operator (Phase 2) or controller (Phase 3) deletes it.

### 4.5 Implementation conventions

These match existing KubeSwift Rust conventions; called out here so the implementer doesn't drift:

- **Error-handling split**: `swift-ch-client` returns `Result<(), ApiError>` (thiserror — library crate). swiftletd-side dispatch returns `Result<ActionOutcome, String>` (anyhow-flavored stringy detail — application crate). Match the existing `dispatch_capture` pattern at `action.rs:331-436`. Sanitize ApiError → status-detail text per §3.1's sanitization rule.
- **Logging**: `log::info!` / `log::warn!` with snake_case event names. New events: `dispatch_migration_send`, `dispatch_migration_receive`, `dispatch_migration_cancel`, `migration_accept`, `migration_reject_in_flight`, `migration_reject_concurrent_snapshot`, `migration_reject_no_ack`. Match the existing snapshot pattern.
- **`ApiClient` lifetime**: construct fresh `ApiClient::new(api_socket).with_timeout(timeout)` per dispatch (existing pattern at `action.rs:378`). Do NOT cache the client across migration actions; the underlying UDS connection is per-request anyway.
- **JSON via `serde_json::json!`**: match existing pattern. Annotation patches use the `serde_json::json!` macro (Bug 16 lesson — never `format!`).
- **Annotation writes via DynamicObject + Patch**: same shape as the existing `write_status` at `action.rs:471-503`. Phase 2 duplicates `write_status` as `write_migration_status` keyed off the migration `KeySet`; do NOT parameterize `write_status` itself (keeps Phase 2 PR blast radius small).

---

## 5. Pod Builder Extensions

### 5.1 The destination-receive pod mode

Phase 2 adds a new pod template flavor: the **destination-receive launcher pod**. It is structurally identical to a normal SwiftGuest launcher pod (same volumes, same network init container, same swiftletd container) with two differences:

1. **A new annotation**: `kubeswift.io/migration-role: receiver` is set at pod creation time. swiftletd reads this annotation at startup and enters destination-receive mode (does not call `vm.create` / `vm.boot`).
2. **A new annotation**: `kubeswift.io/migration-phase2-unsafe-plaintext: ack` is REQUIRED. Without it, swiftletd refuses any migration handler dispatch (S2 gate). This annotation must also be set on the source launcher pod for the matching `send` action.

The pod template itself is otherwise the existing disk-boot launcher template. No new container images, no new RBAC, no new init containers.

#### 5.1.1 Phase 1 isolation invariant

The destination-receive pod is a **separate template path** from the existing offline-migration / regular-disk-boot template. Phase 2 does **not** modify the existing pod-builder code path used by Phase 1's offline migration or by normal SwiftGuest boots.

In Phase 2's manual demo, the destination pod is hand-rolled YAML applied via `kubectl apply` — the operator owns the template. In Phase 3, the SwiftMigration controller's pod builder constructs this template alongside its existing offline-migration build path; the existing path is unchanged.

This isolation prevents accidental Phase 1 regressions. Reviewers MUST refuse any Phase 2 PR that modifies the existing `internal/controller/swiftguest/pod.go` or Phase 1's offline-migration pod-builder code beyond strictly additive paths.

**Drift risk — seed-iso reconstruction (tracked-follow-up):** the destination-receive pod's swiftletd needs the same `seed.iso` reconstruction the existing `main.rs:121-141` performs for restore-receive (via `swift_seed::build_nocloud_dir` and `create_seed_iso` at `main.rs:18-49`). Phase 2's hand-rolled YAML must replicate the seed-iso volume mounts and the `KUBESWIFT_INTENT_PATH` env var, or swiftletd's startup fails before reaching the migration branch. Phase 3's controller-built path will replicate Phase 1's pod-builder logic via shared helper. Phase 2 documents the required volume mounts in `test/migration/manual/dst-launcher-pod.yaml.template` (per §11 implementation checklist item 14). Drift between hand-rolled YAML and Phase 1's pod-builder logic is a known follow-up risk — flagged here so Phase 3 explicitly closes it by reusing the helper.

#### 5.1.2 RWX volumes rejected by destination-receive pod template

The Phase 2 destination-receive pod template assumes RWO PVC access mode (matching Phase 1's offline-migration model and the spike's evidence base). RWX volumes are NOT supported in Phase 2 because of the **F2 source-auto-resume** behaviour: when destination is killed mid-migration, source CH automatically resumes its guest. With RWO storage, PVC attachment serializes between source and destination — the source cannot be both attached and resumed while the destination is also attaching. With RWX, this serialization disappears, opening a split-brain window where source and destination CH could both be `Running` against the same disk file simultaneously.

Phase 2 manual demo template documents `accessModes: [ReadWriteOnce]` as required. Operators using a CSI driver that only offers RWX must either configure RWO mode for migration tests OR wait for Phase 3+ where the split-brain hazard is explicitly handled in the controller's StopAndCopy phase. The hand-rolled YAML template includes a comment block explaining this.

### 5.2 Prerequisite ordering: tap and PVC must exist before swiftletd starts (F5)

Spike F5 confirmed: when `receive-migration` fires, CH attempts to attach the migrated guest's host resources (tap interface, disk file). The tap must already exist on the destination node, with the same name the source's CH used. The PVC must already be mounted into the pod.

Phase 2 mirrors Phase 1's pod-startup ordering: the network init container creates `tap0` and (on the destination's host) the `br0` bridge **before** the swiftletd container starts. swiftletd's CH startup is gated on the init container's exit; CH's `receive-migration` is gated on the action-loop's annotation observation. Layered correctly:

```
1. Pod scheduled to destination node
2. PVC attach (kubelet — implicit)
3. init container: network-init creates tap0 + bridge attach
   (Phase 1 logic unchanged; no Phase 2 modifications)
4. init container exits 0; swiftletd container starts
5. swiftletd reads kubeswift.io/migration-role: receiver
6. swiftletd spawns CH with --api-socket only
7. swiftletd's action loop polls; observes migration-action: receive
8. swiftletd writes status: listening; calls receive_migration(...)
9. CH attaches the migrated tap0/PVC; awaits source connection
```

Step 3 must complete before step 6, or `receive-migration` fails at step 8 with `tap interface kstap0 not found` (T2 in spike Q1e).

### 5.3 What the operator applies in Phase 2 manual demo

The operator applies destination-pod YAML directly. **No SwiftMigration CR. No SwiftGuest CR mutation.** The pod manifest is constructed by hand using a template documented in the Phase 2 manual demo README. The destination pod's `metadata.annotations` carries:

- `kubeswift.io/migration-role: receiver`
- `kubeswift.io/migration-phase2-unsafe-plaintext: ack`

Once the destination pod reaches `Ready`, the operator applies the `migration-action: receive` annotation via `kubectl annotate`. This is the Phase 2 equivalent of "Phase 3 controller writes annotation."

(Phase 3's SwiftMigration controller will build this same pod template programmatically and write the same annotations. The pod shape is the contract between Phase 2 and Phase 3.)

---

## 6. Failure Modes and Operator UX

### 6.1 The completion gate (load-bearing item C, walkthrough W1 — HARD INVARIANT)

**`send-migration` exit=0 is necessary but not sufficient for a migration to be considered complete.** This is W1 from the spike walkthrough — the script printed "Findings reproduced (no contradiction)" because it didn't verify `state=Running` on the destination after `send-migration` returned. The first run of the walkthrough actually FAILED but reported success.

**Phase 2 swiftletd discipline**: the source pod's `migration-status: complete` is gated on observing the source CH process exit cleanly (which CH does on successful migration — Q1c). It does NOT gate on `send-migration` exit code alone, because there is a window where send-migration returns 0 but the destination CH crashes mid-resume (e.g., F12 CPU-feature mismatch detected post-receive). In that case:

- Source CH exits cleanly → swiftletd writes `migration-status: complete` on src
- Destination CH exits with error → swiftletd writes `migration-status: failed` on dst

The Phase 3 controller MUST gate `SwiftMigration.phase=Completed` on **both** terminal statuses being observed (src=`complete` AND dst=`running`). The status-id-match invariant (§3.2) ensures the controller is reading the latest swiftletd observation, not a stale annotation.

This is why §2's row 5 specifies the destination's "complete" gate as `vm.info` returning `state=Running`, not as `receive-migration` returning 0.

Note on terminology: `migration-pause-window-ms` reports the **vCPU-paused window** only (the time the guest is actually frozen during stop-and-copy + resume). Total operator-visible downtime — including pre-copy iteration time, when the source is still running but the destination is not yet — is a separate measurement Phase 3's controller computes from migration-start to GuestRunning (OQ3).

### 6.2 Failure mode catalog

| Failure | Spike ref | Source state | Destination state | Phase 2 swiftletd behavior | Phase 3 controller behavior |
|---|---|---|---|---|---|
| **Source crashes mid-migration** | F1 | gone | gone (CH self-terminates when source connection closes mid-transfer) | n/a — process is gone | Mark `phase=Failed`. Provision fresh destination if retry; **no retry against the same destination** (it's gone too). |
| **Destination cancelled (= dst-kill)** | F2 | Running (auto-resumed) | gone | Source: `migration-status: failed, detail: connection refused`. CH stays running on src — guest continues. | Mark `phase=Cancelled` or `phase=Failed`. Source is canonical and untouched. |
| **Network drop mid-migration** | F3, F4 | Running (after rule lifted) | gone (CH's TCP listener self-terminates after a few seconds of silence — F4) | Source: `migration-status: failed, detail: connection refused`. Destination: pod terminated. | Mark `phase=Failed`. Phase 3 controller heartbeat budget must be measured in seconds (not minutes). Provision fresh destination for retry. |
| **Destination prereqs missing (no tap, no PVC)** | F5 | Running | bound to fail at receive-migration | swiftletd writes `migration-status: failed, detail: tap kstap0 not found` (or PVC mount error). | Mark `phase=Failed`. Operator fixes pod template; controller retries. |
| **Image-tag mismatch (Phase 2 has no allowVersionSkew)** | F11 | Running | rejects send-migration before pre-copy completes | Source: `migration-status: failed, detail: <CH wire-protocol error>`. CH-level version handshake does not exist (F11) — the rejection surfaces as a wire error mid-stream, not a clean refusal. | Mark `phase=Failed`. Phase 3 enforces exact-image-tag match in the validating webhook (image-tag comparison), with `spec.allowVersionSkew=true` opt-in for operators who validated their pair. |
| **CPU feature mismatch (post-receive)** | F12 | exited cleanly (already migrated) | aborts post-receive, pre-resume | Source: `complete` (valid, source did finish migration). Destination: `failed, detail: cpu_incompat`. | Operator-visible split-state. Controller MUST gate `phase=Completed` on dst=`running`, not just src=`complete`. Pre-flight CPU check (OQ1) should make this unreachable in production. |
| **send-migration exit=0 but dst CH crashes** | (W1 case) | exited cleanly | gone | Source: `complete`. Destination: `failed`. | Same as CPU mismatch — controller must gate on both terminal statuses. |
| **Stale API socket (dst was killed in prior attempt)** | W2 | Running | swiftletd-CH startup fails with "Address in use" | swiftletd's W2 cleanup (`rm -f` socket) prevents this; if the cleanup is missing, swiftletd's CH spawn fails and the launcher pod exits. | Pod restart cycle handles it; with W2 cleanup in place, this is NOT a recurring failure. |
| **Concurrent snapshot+migration on same guest** | (composition) | Running | n/a (no migration starts) | swiftletd writes `rejected` to BOTH action namespaces. | Controller observes the rejection and does not advance phase. |

**Detail-string sanitization** (S3-related, applies to all rows above): swiftletd's `migration-status-detail` MUST capture only error category, exit code class, and CH error variant — NEVER raw stderr or memory-region offsets. F12's "cpu_incompat" is the canonical sanitized form; equivalents for other failure modes follow the same shape (`connection_refused`, `tap_not_found`, `pvc_mount_failed`, `wire_protocol_error`, etc.). This is defensive against future failure modes that could leak partial guest state through error strings — not a known leak in F12 specifically.

### 6.3 Operator UX in Phase 2 manual path

The operator-facing surface in Phase 2 is `kubectl annotate` and `kubectl get pod -o jsonpath`:

- **Trigger send**: `kubectl annotate pod src-launcher-pod kubeswift.io/migration-action=send kubeswift.io/migration-action-id=$(uuidgen) kubeswift.io/migration-target-url=tcp:<dst-ip>:<port>`
- **Observe progress**: `kubectl get pod src-launcher-pod -o jsonpath='{.metadata.annotations}'` — read `migration-status`
- **Cancel**: `kubectl annotate pod dst-launcher-pod kubeswift.io/migration-action=cancel kubeswift.io/migration-action-id=$(uuidgen)`

This is a **test surface, not an operator-facing UX**. Phase 3 wraps this into the SwiftMigration controller's lifecycle. Phase 2's README explicitly warns: "do not run this against production VMs — Phase 2 has no security guarantees and no controller-mediated safety."

---

## 7. Manual Demonstration Plan

### 7.1 Prerequisites

- Two-node cluster with both nodes labeled `kubeswift.io/launcher-node=true`
- CH v51.1 image available on both nodes (or whatever swiftletd image ships with Phase 2 — exact-image-tag match per Decision 3)
- Operator can `kubectl annotate` and `kubectl apply` against the cluster's `default` namespace
- A running source SwiftGuest (any disk-boot guest from Phase 1) on node A with a known sentinel inside the guest (e.g., a marker file in `/root/sentinel.txt`)

### 7.2 Step-by-step operator workflow

```
Step 1: Operator inspects the source SwiftGuest
  $ kubectl get swiftguest <name> -o jsonpath='{.status.podRef.name}'
  → src-launcher-pod-abc

Step 2: Operator copies destination launcher pod YAML from the template
  - Template ships at: test/migration/manual/dst-launcher-pod.yaml.template
  - Required volumes (same shape as src): root PVC (RWO), seed-iso emptyDir,
    runtime-dir emptyDir, network NAD if applicable
  - Required env on swiftletd container: KUBESWIFT_MIGRATION_ROLE=receiver,
    KUBESWIFT_INTENT_PATH=/etc/kubeswift/intent.json
  - Required annotations:
      kubeswift.io/migration-role: receiver  (documentation; env var is load-bearing)
      kubeswift.io/migration-phase2-unsafe-plaintext: ack
  - Required nodeSelector: kubernetes.io/hostname=<node-B>
  $ envsubst < dst-launcher-pod.yaml.template > dst-launcher-pod.yaml
  $ kubectl apply -f dst-launcher-pod.yaml

Step 3: Operator waits for destination pod Ready
  $ kubectl wait pod dst-launcher-pod --for=condition=Ready

Step 4: Operator annotates source pod with the unsafe-plaintext ack
  $ kubectl annotate pod src-launcher-pod-abc \
      kubeswift.io/migration-phase2-unsafe-plaintext=ack

Step 5: Operator triggers receive on destination
  $ kubectl annotate pod dst-launcher-pod \
      kubeswift.io/migration-action=receive \
      kubeswift.io/migration-action-id=$(uuidgen) \
      kubeswift.io/migration-listen-url=tcp:0.0.0.0:6789

Step 6: Operator polls for listener ready
  $ until [ "$(kubectl get pod dst-launcher-pod -o jsonpath=\
      '{.metadata.annotations.kubeswift\.io/migration-status}')" = "listening" ]; \
    do sleep 1; done

Step 7: Operator triggers send on source
  $ DST_IP=$(kubectl get pod dst-launcher-pod -o jsonpath='{.status.podIP}')
  $ kubectl annotate pod src-launcher-pod-abc \
      kubeswift.io/migration-action=send \
      kubeswift.io/migration-action-id=$(uuidgen) \
      kubeswift.io/migration-target-url=tcp:$DST_IP:6789

Step 8: Operator polls for src complete + dst running (W1 invariant — BOTH gates)
  src: migration-status=complete (src CH process exited cleanly)
  dst: migration-status=running   (dst CH state=Running)

Step 9: Operator verifies sentinel survived inside the guest
  - swiftctl ssh works because the dst pod is now running the migrated guest
  - Rely on the SwiftGuest CR's swiftctl machinery (note: the SwiftGuest CR
    is unchanged — it still points at the SOURCE pod until the operator
    swaps podRef in Step 10. Phase 2 sidesteps this by exec'ing into the
    dst pod directly via console.)
  $ DST_SERIAL=$(kubectl exec dst-launcher-pod -c swiftletd -- \
        readlink -f /var/lib/kubeswift/runtime/serial.sock)
  $ kubectl exec -i dst-launcher-pod -c swiftletd -- \
        socat - UNIX-CONNECT:$DST_SERIAL <<< 'cat /root/sentinel.txt'
  → SPIKE-SENTINEL-PRE-MIGRATION-...   (matches what was written pre-migration)

Step 10: Operator deletes source pod (Phase 3 controller does this automatically)
  $ kubectl delete pod src-launcher-pod-abc
```

This is the contract Phase 3's controller will drive. Each `kubectl annotate` step corresponds to a controller `Patch` call; each polling step corresponds to a controller `Watch` consumption.

### 7.3 Cancel-path demo

After Step 7 has begun and Step 8 has not yet observed `complete`:

```
Step 7c: Operator cancels via destination
  $ kubectl annotate pod dst-launcher-pod \
      kubeswift.io/migration-action=cancel \
      kubeswift.io/migration-action-id=$(uuidgen) \
      --overwrite

  Expect:
    dst: migration-status=cancelled (CH SIGKILL'd; pod terminates)
    src: migration-status=failed, detail="connection refused"
    src guest continues running (auto-resume via F2)
```

### 7.4 SwiftGuest CR isolation invariant restated (from §1 non-goal)

**At no step does the operator touch any `SwiftGuest` CR.** All Phase 2 operations are on launcher pods directly. This is intentional: it keeps Phase 2's swiftletd extension fully orthogonal to Phase 1's SwiftGuest controller, so Phase 1's offline-migration logic and SwiftGuest-CR-identity preservation are unaffected by Phase 2's manual path.

The Phase 2 manual-demo README documents this invariant in bold at the top.

### 7.5 Operational hygiene during the demo

**Annotation overwrite discipline.** The Step 5/7 `kubectl annotate` commands do NOT use `--overwrite` by default; this is intentional. If a prior action attempt left annotations on the pod, the second attempt will fail rather than silently re-trigger. Cancel (Step 7c) is the only step that uses `--overwrite` because cancel must succeed against an in-flight action's annotations. The action-id idempotency discipline (§3.2) protects against accidental re-trigger when the operator intentionally re-applies the same action-id.

**NetworkPolicy belt-and-braces.** Operators running this demo should apply a default-deny NetworkPolicy on the test namespace before Step 7's send fires:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-cross-namespace-migration-traffic
spec:
  podSelector:
    matchLabels:
      kubeswift.io/migration-test: "true"
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              kubeswift.io/migration-test: "true"
  egress:
    - to:
        - podSelector:
            matchLabels:
              kubeswift.io/migration-test: "true"
```

This is not a Phase 2 hard requirement (the unsafe-plaintext-ack gate is the formal gate), but it limits incidental exposure of the cleartext migration traffic to other workloads on the cluster. Phase 3's mTLS removes the need.

### 7.6 Test surface, not operator UX

Phase 2's manual path is a **demonstration that the swiftletd contract works**, not a usable migration workflow for operators. The README states explicitly:

> Phase 2 does not provide a usable migration workflow. Operators wanting to migrate VMs should use Phase 1's `SwiftMigration` CRD (offline mode) until Phase 3 ships live mode. Phase 2's manual path is for testing the swiftletd extension in isolation.

---

## 8. Security Posture

### 8.1 Threat model

Phase 2 carries **unauthenticated guest state in cleartext on the cluster network**. Anyone with access to the cluster's pod network between source and destination nodes can:

- **Eavesdrop**: read full guest memory + CPU state in cleartext as it streams from source to destination. The `tcp:host:port` migration channel has no encryption (Decision 2 — Phase 3 work).
- **Hijack the destination listener**: write a malicious `migration-listen-url` annotation that points the destination CH at an attacker-controlled port; race a `send-migration` from a malicious source (S1).
- **Exfiltrate via target-url rewrite**: write a malicious `migration-target-url` annotation on the source pod; redirect the in-flight migration to an attacker endpoint (S1).

### 8.2 Required mitigations for Phase 2 ship

These are Phase 2's **must-have-before-ship** items from the spike's checklist. Each is non-negotiable.

#### 8.2.1 The unsafe-plaintext acknowledgment annotation (S2)

swiftletd refuses any `send` or `receive` action on a pod that does not carry the annotation:

```
kubeswift.io/migration-phase2-unsafe-plaintext: ack
```

The annotation must be set to the literal string `ack`. Any other value (including absence, empty string, or `true`) is treated as not-acked and the action is rejected with `migration-status: rejected, detail: phase2_plaintext_ack_missing`.

This gate is removed in Phase 3 once mTLS lands. The check runs at the action-handler entry point — BEFORE any URL parse, BEFORE any CH dispatch, BEFORE any state mutation. It is an enforced runtime check, not a documentation-only convention.

**Mirror-write ordering for ack rejection.** swiftletd writes `migration-status-id = <attempted-action-id>` BEFORE writing `migration-status: rejected`. Without this ordering, the controller would observe a `rejected` status without an associated action-id, and the §3.2 idempotency guard would silently swallow the ack-rejection retry (the controller's next attempt would re-trigger swiftletd's `Idempotent` branch). With the ordering, the controller can correlate the rejection to its specific attempt.

#### 8.2.2 THREAT-MODEL.md banner

A new file `docs/design/THREAT-MODEL.md` (or, if a single banner suffices, an updated section in `docs/design/live-migration.md`) MUST exist before Phase 2 ships. The banner states:

> **Phase 2 swiftletd live-migration plumbing carries unauthenticated guest state in cleartext on the cluster network. Operators MUST NOT route production traffic through this path. Phase 2 is a swiftletd-extension test surface; Phase 3 adds mTLS for production use.**
>
> **Routing a production VM through Phase 2 is a security incident.** Full guest memory and CPU state, including any in-memory secrets (TLS private keys, application credentials, kernel keyrings, decrypted disk content held in page cache), is exposed in cleartext to anyone with read access to the cluster pod network for the duration of the migration.

The banner MUST appear at the top of three locations, not just one:

1. `docs/design/THREAT-MODEL.md` (or the live-migration.md banner section)
2. `docs/migration/phase-2.md` (the operator-facing Phase 2 reference)
3. `test/migration/manual/README.md` (operators reading the demo scripts must see the banner before they copy-paste)

Reachability also from `docs/migration/overview.md` and `kubeswift_context.md`'s Phase 2 entry.

#### 8.2.3 swiftletd reads URL inputs from SwiftMigration CR, not pod annotations (S1) — Phase 3 prerequisite

For Phase 2 manual path, the URL annotations (`migration-target-url`, `migration-listen-url`) ARE read from pod annotations. This is acceptable because:

- The Phase 2 manual path is operator-driven; the operator IS the writer of those annotations.
- No production traffic flows over the Phase 2 plumbing (S2's banner, S2's ack gate).
- The operator is presumed trusted (they have `pods/annotate` RBAC).

For Phase 3 — when the SwiftMigration controller orchestrates migration automatically — swiftletd MUST read the URLs from the SwiftMigration CR directly via kube-rs, NOT from pod annotations. This prevents the S1 hijack vector: a malicious patcher with `pods/patch` RBAC cannot redirect production migrations.

**Phase 2's design must not preclude this Phase 3 mitigation.** Specifically: the Phase 2 swiftletd code that parses `migration-target-url` or `migration-listen-url` from annotations must be a clearly-marked code path that Phase 3 will replace, NOT load-bearing for Phase 3's controller-driven path. The implementer MUST tag every line that reads either annotation key with a **grep-able marker tag**, not just a free-text comment:

```rust
// SECURITY-S1: URL read from operator-set pod annotation (Phase 2 ONLY).
// In Phase 3, this path is deleted; URLs are read from the SwiftMigration
// CR via kube-rs. See docs/design/live-migration-phase-2.md §8.2.3.
let target_url = annotations.get("kubeswift.io/migration-target-url")  // SECURITY-S1
    .ok_or_else(|| ...)?;
```

The tag `SECURITY-S1` is grep-able. Phase 3's PR template will include `grep -r SECURITY-S1 rust/` as a pre-merge check; if any matches remain, Phase 3's mitigation is incomplete. Free-text comments are too easy to miss in a Phase 3 PR review.

#### 8.2.4 Ties to OQ6 (Phase 3 mTLS hand-off plan)

Phase 3's mTLS adds confidentiality + authenticity to the migration channel. **mTLS does NOT subsume S1's annotation-trust-boundary mitigation** (and vice versa): mTLS protects the wire, S1 protects the destination URL. Both are required for Phase 3 production traffic. Phase 2's design is consistent with this composition — neither mitigation is wired in Phase 2; both are deferred to Phase 3 with clear hand-off notes here.

#### 8.2.5 Annotation-key deprecation contract

The following annotation keys are **scheduled for deletion** in Phase 3, NOT for repurposing or controller-driven re-use:

| Key | Phase 2 use | Phase 3 disposition |
|---|---|---|
| `kubeswift.io/migration-action` | Operator triggers action | **DELETED.** Phase 3 controller dispatches via SwiftMigration CR phase transitions; swiftletd watches its own pod's owner-reference SwiftMigration CR (not the pod's annotations) for action triggers. |
| `kubeswift.io/migration-action-id` | Operator-supplied ULID | **DELETED.** Phase 3 controller derives action-id from the SwiftMigration CR's `resourceVersion` or a controller-generated ULID; never operator-writable. |
| `kubeswift.io/migration-target-url` | Operator-supplied destination URL | **DELETED.** Phase 3 reads target from `SwiftMigration.spec.target.nodeName` resolved through controller logic; URL never appears on a pod annotation. |
| `kubeswift.io/migration-listen-url` | Operator-supplied listen URL | **DELETED.** Same shape as target-url. |
| `kubeswift.io/migration-phase2-unsafe-plaintext` | Phase 2 ack gate | **DELETED.** Phase 3 mTLS replaces the gate. |

The status-side annotations (`migration-status`, `migration-status-id`, `migration-status-detail`, `migration-pause-window-ms`) are PRESERVED in Phase 3 — they are swiftletd's report channel, not operator-writable.

This deprecation contract is the explicit Phase 3 hand-off: a Phase 3 PR that REPURPOSES (rather than DELETES) any of the five keys above is considered a regression, because it leaves the S1 annotation-trust-boundary attack surface in place even after mTLS lands.

### 8.3 Phase 3 mTLS hand-off plan (one paragraph)

Phase 3 will likely use a stunnel or socat sidecar pattern: an mTLS-terminating proxy between swiftletd and the migration channel. The controller generates ephemeral certificates per migration, signed by a controller-managed CA, mounted as Secrets into both launcher pods. swiftletd connects to the local proxy on `localhost:<some-port>`; the proxy handles mTLS and forwards to the peer. This adds two sidecar containers per migration. First-party CH support for mTLS would eliminate the sidecars but requires upstream Cloud Hypervisor work — Phase 3 design will pick the pragmatic path. **Phase 2 implementation must not bake-in any assumption about local-proxy-vs-direct-CH-tcp**; the `target-url` parameter Phase 2 swiftletd passes to CH's `send-migration` API just happens to be a `tcp:host:port` today and could be `tcp:localhost:<proxy-port>` tomorrow with no swiftletd changes.

### 8.4 Phase 2 security review verdict

Per spike Section 9 (security review): CONCERNS-DOCUMENTED, no Phase 2 blockers. The two gates above (`unsafe-plaintext: ack` annotation + THREAT-MODEL banner) are sufficient discoverability gates for a manual-demo scope. Phase 2 ships with these gates in place.

---

## 9. Testing Strategy

### 9.1 Unit tests for swift-ch-client extensions

`rust/swift-ch-client/src/methods.rs` gets new tests for `send_migration` and `receive_migration`, mirroring the existing `snapshot()` test pattern (mock UDS server, assert request shape, assert response handling). Coverage:

- Successful round-trip (returns `Ok`)
- HTTP 4xx response (returns `ApiError::Status`)
- Malformed JSON response (returns `ApiError::Malformed`)
- UDS unreachable (returns `ApiError::Connect`)

These run on every `cargo test` and are CI-runnable without a cluster.

### 9.2 Unit tests for swiftletd action loop

`rust/swiftletd/src/action.rs` gains parallel coverage for `MigrationSend`, `MigrationReceive`, and `MigrationCancel` action kinds:

- Annotation decoding + decision logic (extending the existing `decide` test surface)
- Migration / snapshot mutual rejection: when both action namespaces have non-empty values, both are rejected
- Idempotency: same action-id → no-op; new action-id while in-flight → reject
- The unsafe-plaintext-ack gate: action without ack → rejected

These are pure-function tests; no kube-rs, no I/O.

### 9.3 Integration tests on the cluster (Phase 2 manual path)

`test/migration/manual/` ships a script suite (per §11 implementation checklist) that exercises the manual-demo flow:

- `source.sh`: prepares source SwiftGuest with sentinel
- `destination.sh`: applies destination launcher pod YAML
- `run.sh`: orchestrates the annotation sequence per §7 steps 4–10
- `verify.sh`: asserts sentinel survived, both terminal statuses observed (W1 invariant)

These run against a cluster (miles + boba) and are NOT runnable in cluster-less CI. The Phase 2 PR adds a `make migration-phase2-manual` target that runs them; CI's existing `e2e-on-cluster.yaml` workflow gets a path-touch trigger for `rust/swiftletd/src/action.rs`, `rust/swift-ch-client/src/methods.rs`, and `test/migration/manual/**`.

### 9.4 What CANNOT be tested in CI

- Cross-node migration itself (requires multi-node cluster)
- The dst-kill cancel primitive (requires real CH process)
- The W2 stale-socket failure mode (requires a real prior-killed CH)
- F12 CPU-feature mismatch (requires heterogeneous CPUs)

These are validated in the Phase 2 cluster-side mini-walkthrough (per §11) at PR close.

### 9.5 Regression: Phase 1 offline migration unchanged

The Phase 2 PR MUST include a passing run of `test/migration/swiftmigration/`, the existing Phase 1 offline-migration e2e test. Phase 2 explicitly does not modify Phase 1 code paths (§5.1.1 isolation invariant), so regressions are unexpected — but the e2e test gates the PR anyway.

---

## 10. Open Questions Deferred to Phase 3+

Each of the spike's seven Open Questions, with one-sentence disposition:

| OQ | Topic | Phase 2 disposition |
|---|---|---|
| **OQ1** | Heterogeneous CPU microarch policy | Deferred to Phase 3. Phase 2's swiftletd will surface F12-style CPU-mismatch failures via `migration-status: failed`; Phase 3's SwiftMigration validating webhook gains a CPU-feature pre-flight check (mirroring Phase 1's target-node-Ready check). Tracked-follow-up. |
| **OQ2** | Destination listener timeout strategy | Deferred to Phase 3. Phase 2's `receive-migration` blocks indefinitely on swiftletd's side; CH's TCP layer is the timeout source (F4). Phase 3 adds `spec.destinationTimeout` (~30 s default) on SwiftMigration. Tracked-follow-up. |
| **OQ3** | observedDowntime → split into observedPauseWindow + observedTotalMigrationTime | Partially addressed in Phase 2: swiftletd writes `kubeswift.io/migration-pause-window-ms` (the observed vCPU-paused window). Phase 3 adds the matching SwiftMigration status field and the totalMigrationTime measurement. Tracked-follow-up. |
| **OQ4** | Progress reporting: poll-`info`-API vs tail-`--log-file` | Phase 2 chooses poll-`info`-API per §3.4 (F8 + S4 + load-bearing item B). Log-tailing reserved for Phase 3+ if operator demand surfaces. Resolved-in-this-doc. |
| **OQ5** | Source-crash recovery model | Deferred to Phase 3. Phase 2's manual demo shows source-crash makes both pods unrecoverable (F1); Phase 3's controller `phase=Failed` after source crash means "provision fresh destination." Phase 2 has no controller, so no recovery model is needed. Tracked-follow-up. |
| **OQ6** | Migration channel auth for Phase 3 (mTLS) | Deferred to Phase 3 entirely. §8.3 sketches the hand-off plan; Phase 2's design does not preclude any specific Phase 3 implementation choice. Tracked-follow-up. |
| **OQ7** | Audit logging policy | Deferred to Phase 3. Phase 2's manual demo path is operator-driven; any audit interest is satisfied by `kubectl get events` and the operator's own shell history. Phase 3's controller emits Kubernetes Events on each phase transition with target-URL, source-pod, destination-pod, and operator identity. Tracked-follow-up. |

The "Tracked-follow-up" markers feed `kubeswift_context.md`'s Phase 3 implementation checklist when Phase 3 work begins.

### 10.1 Phase 3 work surface inventory

A consolidated list of Phase 3 obligations identified across this doc, for the Phase 3 implementer's pre-flight reading:

1. **§6.1 — Completion gate as hard invariant.** Phase 3 controller MUST gate `SwiftMigration.phase=Completed` on observed `src=complete` AND `dst=running` (status-id-match invariant). Never gate on `send-migration` exit code alone.
2. **§8.2.3 — S1 annotation-trust-boundary mitigation.** swiftletd reads URLs from `SwiftMigration` CR via kube-rs, NOT from pod annotations. Phase 3 PR template adds `grep -r SECURITY-S1 rust/` as pre-merge check.
3. **§8.2.5 — Annotation-key deprecation contract.** Phase 3 DELETES the operator-writable migration annotation keys, does not repurpose them.
4. **§8.3 — mTLS migration channel.** Sidecar pattern (stunnel/socat) OR upstream CH first-party support. Trust anchors: cluster CA + per-migration ephemeral certs (likely).
5. **§4.5 — Implementation conventions.** Phase 3's controller-side code matches the same conventions Phase 2 establishes (anyhow vs thiserror split, snake_case log events, fresh ApiClient).
6. **§5.1 — Drift risk: seed-iso reconstruction.** Phase 3's controller-built destination pod MUST reuse Phase 1's pod-builder helper (`swift_seed::build_nocloud_dir` + `create_seed_iso`), not duplicate the YAML logic.
7. **OQ1–OQ7 from §10.** CPU pre-flight check (OQ1), destination listener timeout (OQ2), observedDowntime split (OQ3), source-crash recovery (OQ5), audit logging (OQ7) — all Phase 3 work.

Phase 3's design doc (`docs/design/live-migration-phase-3.md`, when written) should open with this inventory as the Goal section's input.

---

## 11. Implementation Checklist

This is the concrete TODO list for the Phase 2 implementer. Items must be completed in order; each item is approximately 1–3 days of work.

### Must-haves (the five spike items, FIRST):

1. **swiftletd CH spawn `rm -f` API socket file before invoking CH** — W2 (load-bearing item D) / §4.3.3. `rust/swift-ch-client/src/spawn.rs` — `spawn_ch`, `spawn_ch_restore`, AND the new `spawn_ch_receive`. One-line change per spawn function with a comment citing §4.3.3. **Pre-req work**: add `pub fn api_socket(&self) -> Option<&Path>` accessor on `VmConfig` so `spawn_ch` can extract the socket path before the cleanup step.

2. **swiftletd annotation-read code paths tagged with `SECURITY-S1`** — S1 (annotation-trust-boundary) / §8.2.3. Every line that reads `migration-target-url` or `migration-listen-url` from annotations carries the literal grep-able tag `SECURITY-S1`. Phase 3 PR template adds `grep -r SECURITY-S1 rust/` as a pre-merge check.

3. **`kubeswift.io/migration-phase2-unsafe-plaintext: ack` gate at the swiftletd action-handler entry point** — S2 (plaintext-ack discoverability) / §8.2.1. Action handler refuses any `send` / `receive` without the ack annotation. Returns `migration-status-id=<attempted-id>` + `migration-status: rejected, detail: phase2_plaintext_ack_missing` (mirror-write order from §8.2.1). Unit-tested.

4. **THREAT-MODEL.md banner with severity language** — S2 / §8.2.2. Banner reachable from `docs/design/THREAT-MODEL.md`, `docs/migration/phase-2.md`, AND `test/migration/manual/README.md`. Severity language MUST include "Routing a production VM through Phase 2 is a security incident" + the in-memory secrets enumeration.

5. **W1 completion-gate invariant in swiftletd code** — W1 (load-bearing item C) / §6.1. swiftletd's `migration-status: complete` (src) is gated on observed CH process exit on src; `migration-status: running` (dst) is gated on observed `vm.info` `state=Running` on dst — never on `send-migration` or `receive-migration` exit code alone.

### Core implementation:

6. **Extend `swift-ch-client` with `send_migration` and `receive_migration` methods** (§4.1). Mirror existing `snapshot()` shape — sync `request_ok` returning `Result<(), ApiError>`. Unit tests for round-trip + error paths.

7. **Add `spawn_ch_receive(api_socket: &Path)` as sibling of `spawn_ch_restore`** (§4.3.2). New function in `rust/swift-ch-client/src/spawn.rs`. Argv contains `--api-socket=<path>` only. Unit test mirrors `restore_args_does_not_include_disk_or_network_flags`.

8. **Parameterize `decide` over a `KeySet` struct** (§4.2). Existing const block at `action.rs:79-86` becomes `KeySet { action_key, action_id_key, action_args_key }`; one instance for snapshot, one for migration. `ActionState` gains `{ snapshot: NamespaceState, migration: NamespaceState }`. Existing snapshot tests rewired against `&SNAPSHOT_KEYS`.

9. **Extend swiftletd action loop with migration action kinds** (§4.2). Add `MigrationSend`, `MigrationReceive`, `MigrationCancel` to `ActionKind`. Wire dispatch through new `dispatch_migration_send/receive/cancel` functions and a new `write_migration_status` (do NOT parameterize the existing `write_status`).

10. **Tokio runtime shape for migration handlers** (§4.2.2). Source-side `MigrationSend` and destination-side `MigrationReceive` dispatch on `std::thread::spawn` workers (mirroring `spawn_action_loop`); `tokio::sync::oneshot` signals completion back to the loop. Loop continues polling and can dispatch `cancel` against in-flight migration. Do NOT upgrade the runtime to multi-thread.

11. **Snapshot/migration mutual rejection** (§4.2.1). When both annotation namespaces have non-empty values at the same poll tick, reject both with `rejected` + `concurrent action with snapshot rejected` (and analogous on snapshot side). Rejection is per-tick and recomputed; controller MUST NOT clear annotations on rejection.

12. **Status-id-paired-write discipline for migration namespace** (§3.2). swiftletd writes `migration-status-id` BEFORE `migration-status` for every transition. Unit tests against snapshot Bug 14 regression pattern.

13. **Destination-receive pod mode in swiftletd** (§4.3.2 + §5.1). swiftletd reads `KUBESWIFT_MIGRATION_ROLE=receiver` env var at startup (NOT from pod annotations); receiver branch lives in `rust/swiftletd/src/launch.rs` alongside the existing `intent.is_restore()` branch; calls new `spawn_ch_receive`; awaits action loop's receive action.

14. **swiftletd CH-process exit observation** (§5.1 + §6.1). Source path: detect CH clean exit; transition to `complete`. Destination path: poll `vm.info` for `state=Running`; transition to `running`.

15. **Progress polling at 2s cadence on src** (§3.4). swiftletd polls local CH `vm.info` while `start-send` is in flight on a third concurrent thread (independent of the action loop and the send worker); emits `migration-status` transitions. Distinct UDS connections per call (`request_ok` opens fresh sockets), so concurrent polling is safe.

16. **Cancel handler** (§2 row 7 + §4.4). Destination's `migration-action: cancel` SIGKILL's local CH child.

17. **Pod-builder destination-receive template documented** (§5.1 + §5.1.2 + §7.1). Phase 2 does NOT modify Phase 1's pod builder; the destination pod template is a hand-rolled YAML in `test/migration/manual/dst-launcher-pod.yaml.template`. Template enforces `accessModes: [ReadWriteOnce]` (RWX rejected per §5.1.2). README documents seed-iso volume-mount drift risk per §5.1.

18. **Detail-string sanitization** (§3.1 + §6.2). swiftletd writes only category strings (`connection_refused`, `cpu_incompat`, `tap_not_found`, etc.) into `migration-status-detail`, never raw stderr. Unit tests for the sanitizer.

### Manual demo:

19. **`test/migration/manual/source.sh`, `destination.sh`, `run.sh`, `verify.sh`** (§7.2 + §9.3). End-to-end orchestration scripts. README explaining the workflow + the unsafe-plaintext-ack gate + the SwiftGuest-CR isolation invariant. README carries the THREAT-MODEL banner per §8.2.2.

20. **`test/migration/manual/dst-launcher-pod.yaml.template`** (§5.1 + §7.2 step 2). Hand-rolled destination pod template. Includes RWO accessModes assertion (§5.1.2), `KUBESWIFT_MIGRATION_ROLE=receiver` env var, seed-iso volume mounts (drift risk per §5.1).

21. **`make migration-phase2-manual` target** (§9.3). Runs the scripts against a cluster; expects miles + boba.

### Test wiring:

22. **CI path-touch trigger**: `rust/swiftletd/src/action.rs`, `rust/swift-ch-client/src/methods.rs`, `rust/swift-ch-client/src/spawn.rs`, `test/migration/manual/**` added to `e2e-on-cluster.yaml` workflow.

### Documentation:

23. **kubeswift_context.md update**: move Phase 2 from "Decisions Resolved (pending implementation)" to "SHIPPED". Add new annotation keys to the Status Reporting Architecture table. Add any new bug-fix rows that surface during implementation.

24. **`docs/migration/phase-2.md` (or update `docs/migration/live-migration.md`)** with operator-facing notes: this is the test surface, here's how to run it, here's the ack gate, here's the SwiftGuest isolation invariant. Banner per §8.2.2.

### Mini-walkthrough at PR close:

25. **30–60 min cluster-side mini-walkthrough** at PR close, per the Phase 1 / spike pattern (Tracked Follow-up #4 in `kubeswift_context.md`). Run the scripts end-to-end, verify the W1 completion gate fires correctly (do not gate on `send-migration` exit code alone), verify the W2 stale-socket cleanup actually fires (kill src pod between attempts; second attempt should succeed), verify the snapshot/migration mutual rejection per §4.2.1, verify the unsafe-plaintext-ack gate rejection per §8.2.1. Findings folded back into the PR description.

---

## Appendix: Cross-references

- **Spike findings**: `docs/design/live-migration-phase-2-spike.md`
  - F1, F2, F3, F4: failure modes (§6.2)
  - F5: pod-prereq ordering (§5.2)
  - F6, F7: pre-copy iteration cap, downtime numbers (§2.1)
  - F8: log vs API progress reporting (§3.4)
  - F9: annotation surface fits (§3.1, §3.2, §3.3)
  - F11, F12: CPU + version compatibility (§6.2)
  - W1: completion-gate invariant (§6.1)
  - W2: stale-socket cleanup (§4.3.3)
  - S1: annotation-trust-boundary (§8.2.3, §8.2.5)
  - S2: plaintext-TCP gating (§8.2.1, §8.2.2)
  - S3: CPU-mismatch memory cleanup (§6.2 detail-string sanitization, §10 OQ1)
  - S4: log-parsing as guest-escape vector (§3.4); audit logging (§10 OQ7); RWX double-execution (§5.1.2)
  - OQ1–OQ7: deferred questions (§10)
  - F10 (CH v51.1↔v50.2 bidirectional compatibility): not directly used in Phase 2 (which mandates exact-image-tag match); referenced as informational input to Phase 3's `spec.allowVersionSkew` design.
- **Phase 1 design**: `docs/design/live-migration.md` — overall live-migration design; Phase 2 section there is the high-level plan
- **Phase 1 spike**: `docs/design/live-migration-phase-1-spike.md` — direct PVC reuse evidence (Approach A); precedent for "spike → design doc → implementation"
- **Snapshot Phase 2 precedent**: `rust/swiftletd/src/action.rs`, `internal/controller/swiftsnapshot/local.go`, PR #14 (Bug 14)
- **Project context**: `kubeswift_context.md` — Phase 2 must-have-before-ship checklist (§11 must-haves above mirror this)
