# Live Migration — CH v52 send-migration observability & tuning

> Status: DESIGN / SCOPING. Activates the forward-compat `SwiftMigration` spec
> fields on Cloud Hypervisor v52, reshapes the convergence model, and finishes
> the downtime-metric semantics (TFU #11 + W28). Last updated: 2026-06-08.

## 1. Goal

CH v52's `vm.send-migration` API gained three config fields over v51.1
(confirmed in the shipped v52 binary). Wire them through, reshape the
convergence model they enable, and fix the downtime-metric semantics:

- **`downtime_ms`** — a target max vCPU-paused window. CH runs pre-copy
  iterations until the *estimated* final stop-and-copy fits under the target,
  then commits. This replaces v51.1's hardcoded **5-iteration cap** with
  classical dirty-rate convergence (supersedes Live Migration **Phase 2
  Decision 4**).
- **`connections`** — multiple parallel TCP connections for the memory stream
  (higher transfer bandwidth on fast interconnects).
- **`timeout_s`** — CH-side per-migration timeout (overlaps our `spec.timeout`).

The spec surface for the first two **already exists** — `spec.downtimeTarget`
(`*metav1.Duration`) and `spec.parallelConnections` (`int32`) were added
forward-compat and are currently documented "Ignored in Phase 1." v52 makes
them live. This work is therefore mostly **wiring + metrics**, not new CRD
design.

## 2. Current state (what's already there)

| Surface | State |
|---|---|
| `spec.downtimeTarget` | EXISTS, unused (carried for forward-compat) |
| `spec.parallelConnections` | EXISTS, unused |
| `swift-ch-client::send_migration()` | sends **only** `destination_url` |
| `status.observedTransferDuration` | EXISTS — the full `vm.send-migration` RPC elapsed (pre-copy + stop-and-copy + finalize). Read from the `kubeswift.io/migration-pause-window-ms` annotation. |
| `status.observedPauseWindow` | **deprecated alias** of the above ("will be removed in Phase 3b+1") |
| `status.observedDowntime` | the operator-visible cluster downtime (`cutoverStep2DispatchedAt → GuestRunning`) |
| swiftletd pause-window source | `action.rs` computes `pause_window_ms = elapsed_ms` of the whole send RPC (the W27b/TFU #11 mismatch) |

**The semantic gap (W28 / TFU #11):** nothing surfaces the **real vCPU
stop-the-world window** — the time the guest is actually frozen (CH's final
stop-and-copy). Today `observedTransferDuration` is the whole RPC (mostly
*not* paused) and `observedDowntime` is the cluster cutover window
(dispatch → resume, includes scheduling/pod swap). With `downtime_ms`, CH now
*bounds* the true frozen window to the target — making it both controllable and
(pending the PR 1 spike) reportable.

## 3. Convergence model change (supersedes Phase 2 Decision 4)

- **v51.1:** pre-copy hardcoded to 5 iterations, then unconditional
  stop-and-copy. High-dirty-rate guests emerge with a stop-and-copy ≈ one
  iteration window of dirty pages; downtime is *not* operator-controllable.
- **v52 with `downtime_ms`:** pre-copy iterates until `estimated_stop_copy <
  downtime_ms`, then commits — classical convergence. A guest dirtying faster
  than the link can drain **may not converge**; CH still has `timeout_s` (and
  our `spec.timeout`) as the backstop. So the webhook policy "no admission gate
  on dirty rate" stays correct, but the operator now trades **downtime vs total
  transfer time** via `downtimeTarget`.

This is a **CH-version-conditional** behavior: the doc and the field semantics
must state "downtime_ms convergence applies on CH ≥ v52; on v51.x the 5-iter
cap governs and downtimeTarget is ignored." (We ship v52, so it's live — but
the conditional matters for the record and for any future CH downgrade.)

## 4. Phased plan (full scope, per user)

### PR 1 — configurable downtime (`downtime_ms`) + on-cluster spike
- `RuntimeIntent` migration-send block carries `downtime_ms` (from
  `spec.downtimeTarget`); default a sane target (**300ms** proposed — typical
  vCPU-pause for a converged guest; operators tighten/loosen).
- `swift-ch-client::send_migration(destination_url, downtime_ms, …)` adds
  `downtime_ms` to the PUT body; swiftletd passes it through; the controller
  reads `spec.downtimeTarget` and plumbs it to the send annotation/intent.
- Webhook: bound `downtimeTarget` (e.g. 10ms–10s) for live mode; ignored for
  offline.
- Remove the "Ignored in Phase 1" caveat from the field doc.
- **On-cluster spike (doubles as validation):** run live migrations at
  `downtimeTarget` = 100/300/1000ms on a baseline and a `stress-ng`-dirtied
  guest; measure `observedTransferDuration`, `observedDowntime`, convergence
  (iteration count if CH logs it), and crucially **whether CH reports the
  achieved downtime** in the send-migration response or logs (answers W28
  feasibility for PR 2).

### PR 2 — downtime-metric semantics (TFU #11 + W28)
- **Finish TFU #11:** remove the deprecated `observedPauseWindow` alias (its
  Phase-3b+1 removal is now due); `observedTransferDuration` stands alone as
  the RPC-duration metric.
- **W28:** add the **real vCPU stop-the-world** surface. Source depends on the
  PR 1 spike:
  - *If CH v52 reports achieved downtime* (response body or a parseable log
    line): swiftletd writes a new `kubeswift.io/migration-downtime-ms`
    annotation; the controller stamps a new `status.observedStopAndCopy`
    (`*metav1.Duration`) — the honest "guest frozen" metric.
  - *If not:* surface `downtimeTarget` as the *bound* (`status.downtimeTargetMs`
    echo) and document that the achieved value is ≤ target but not directly
    measured (external-ping observer remains the only ground truth — Tracked
    Follow-up #1's multi-node L2).
- **Prometheus:** add `kubeswift_migration_stop_and_copy_seconds{mode}` (the
  real frozen window) alongside the existing transfer/downtime histograms;
  Grafana panel.

### PR 3 — multi-connection transfer (`connections`)
- Wire `spec.parallelConnections` → `connections` in the send body (and the
  matching receive-side listener accepts N connections — the v52 binary's
  "Received more than … additional migration connections" path).
- Default (1? or auto = min(cores, N))? — decide from a PR 3 cluster
  throughput sweep (1 vs 2 vs 4 connections on the Calico VXLAN pod network;
  Phase 3b Q4 measured ~902 Mbit/s single-stream — see if parallel streams
  beat it or just add overhead on a 1GbE interconnect).
- Webhook: bound `parallelConnections` (e.g. 1–8); remove "Ignored in Phase 1."

## 5. Open questions (resolved during the PRs)
1. **Does CH v52 report achieved downtime / iteration count?** (PR 1 spike) —
   determines whether W28's real metric is first-party or bound-only.
2. **Default `downtimeTarget`** — 300ms proposed; confirm against the spike's
   converged-guest stop-and-copy measurement.
3. **`connections` default** — 1 (conservative) vs auto; PR 3 throughput sweep
   on the dev cluster's 1GbE decides (parallel streams may not help below the
   NIC ceiling).
4. **`timeout_s` vs `spec.timeout`** — keep our controller-level `spec.timeout`
   as the authority; optionally pass a derived `timeout_s` to CH as a
   belt-and-suspenders. Likely leave CH's unset (avoid two timeout authorities).

## 6. Non-goals
- No change to the offline-migration path (no memory RPC; these fields are
  live-mode-only).
- No mTLS/transport changes (Phase 3c owns that; composes unchanged).
- CPU-feature pre-flight (TFU #10) is orthogonal and out of scope.

## 7. Validation
Each PR validates on the dev cluster (miles + boba, kernel-boot + RWX/Block
disk-boot live-migratable guests):
- PR 1: downtime targets sweep + convergence + achieved-downtime feasibility.
- PR 2: metric values match the spike measurements; deprecated alias gone;
  `kubectl explain` clean.
- PR 3: throughput sweep (1/2/4 connections); regression that single-connection
  (default) is unchanged.

## 8. PR 1 spike results (cluster-validated, 2026-06-08)

Ran on the dev cluster (kernel-boot faas-minimal 2Gi guest, miles↔boba,
CH v52.0, image `sha-d0b4ef1`). Migrations at `downtimeTarget` =
100/300/1000ms + baseline:

| downtimeTarget | result | observedDowntime | observedTransfer |
|---|---|---|---|
| 100ms | Completed | 2.36s | 19.39s |
| 300ms | Completed | 2.32s | 19.38s |
| 1000ms | Completed | 1.99s | 19.39s |
| (unset) | Completed | 3.67s | 19.40s |
| 5ms | **webhook-rejected** ("outside [10ms, 10s]") | — | — |

**Resolved open questions:**
1. **Does CH v52 report achieved downtime? → NO.** `vm.send-migration`
   returns 204 (empty body) and CH runs at default WARN verbosity (no INFO
   migration telemetry). **W28 is therefore BOUND-ONLY**: PR 2 echoes the
   applied `downtime_ms` ceiling into `status.appliedDowntimeMs` and
   documents that the achieved vCPU-stop is ≤ the ceiling but not directly
   measurable here (an external L2 ping observer — TFU #1 — is the only
   ground truth). No `stop_and_copy_seconds` histogram is added (no data
   source; documented, not silently skipped).
2. **`observedDowntime` is scheduling-dominated cluster-cutover time**
   (1.99–3.67s of pod-swap noise across runs), confirmed NOT the vCPU-stop —
   PR 2 does NOT relabel it "guest frozen."
3. **Default `downtimeTarget`: stays unset (opt-in).** Defaulting to 300ms is
   deferred — the convergence-under-load behaviour is unproven (below).

**Unvalidated (asset-gated, TFU #13):** the faas-minimal kernel-boot guest
does not dirty memory, so `observedTransfer` is a constant ~19.4s (one 2Gi
pass) regardless of target — the `downtime_ms` *convergence* effect (does it
shorten the frozen window for a dirtying guest, trading more pre-copy
iterations for a smaller stop-and-copy?) could not be measured. Needs a
`stress-ng` rand-set workload on a disk-boot guest. The mechanism is sound
(CH iterates until estimate < target); the empirical proof waits on that
setup.

### PR 2 shape (revised by the spike)
- Remove the deprecated `status.observedPauseWindow` alias (TFU #11 done).
- Add `status.appliedDowntimeMs` (`*int64`) — the ceiling echo, stamped by the
  controller when it sends `downtime_ms`; `swiftctl migration describe` shows
  it as "Downtime cap".
- NO new Prometheus histogram (W28 has no measured value to record).

🤖 Generated with [Claude Code](https://claude.com/claude-code)
