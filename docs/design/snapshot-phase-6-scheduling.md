# Snapshot Phase 6 — scheduling (CronSnapshot) + keep-N retention

> Status: DESIGN. Anchors on the Phase 5 deferred follow-up
> ([`docs/design/snapshot-phase-5.md` §9](snapshot-phase-5.md)) — scheduling +
> keep-N "only pay off with a scheduler, so they're a coherent future phase".
> Last updated: 2026-06-06.

## 1. Goal

Take periodic snapshots of a SwiftGuest automatically, and keep only the most
recent N. Two surfaces:

1. **`SwiftSnapshotSchedule`** — a cron schedule + a SwiftSnapshot template; the
   controller creates a SwiftSnapshot each time the schedule fires.
2. **keep-N retention** — the schedule deletes its oldest snapshots beyond
   `spec.retention.keepLast`, complementing the per-snapshot `spec.ttl`
   (max-age) shipped in Phase 5.

This is the last open snapshot item. It adds **no new capture/runtime
mechanism** — it instantiates the existing SwiftSnapshot machinery on a timer
and GCs by count.

## 2. What already exists (and what Phase 6 reuses)

| Primitive | Where | Phase 6 use |
|---|---|---|
| SwiftSnapshot (capture → Ready, all backends) | `swiftsnapshot` controller | **Instantiated per schedule tick** — the schedule creates SwiftSnapshots, it does not capture directly |
| Per-snapshot `spec.ttl` + reference-aware GC | `swiftsnapshot/retention.go` (Phase 5) | **Composes** — a scheduled snapshot can carry a ttl AND be keep-N-bounded; whichever expires first deletes it |
| `spec.deletionPolicy: Delete\|Retain` | `swiftsnapshot/cleanup.go` (Phase 5) | **Inherited via the template** — keep-N delete honors it (Delete purges artifacts, Retain keeps them) |
| `retentionBlocker()` (don't delete a referenced snapshot) | `swiftsnapshot/retention.go` | **Reused** — keep-N must not delete a snapshot a cloneFromSnapshot guest / in-flight restore still uses |
| Template-of-spec + ownerRef GC | `SwiftGuestPool` (`spec.template.spec`, pool owns replicas) | **Pattern mirrored** — `SwiftSnapshotSchedule.spec.template.spec` is a `SwiftSnapshotSpec`; the schedule owns its snapshots |
| Terminal-transition metrics | `internal/metrics` (Phase 5) | scheduled snapshots increment the same `kubeswift_snapshot_total{backend,result}` for free; a small schedule-level counter is additive |

## 3. CRD — `SwiftSnapshotSchedule` (`snapshot.kubeswift.io/v1alpha1`)

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshotSchedule
metadata:
  name: nightly-db
spec:
  schedule: "0 2 * * *"            # standard 5-field cron (controller TZ = UTC)
  suspend: false                  # pause without deleting the schedule
  concurrencyPolicy: Forbid       # Forbid (default) | Skip | Allow
  startingDeadlineSeconds: 300    # skip a tick missed by more than this (optional)
  retention:
    keepLast: 7                   # keep the 7 most recent READY snapshots; GC older
  template:
    metadata:
      labels: {tier: db}          # merged onto each created SwiftSnapshot
    spec:                         # a SwiftSnapshotSpec (same shape as a hand-written SwiftSnapshot)
      guestRef: {name: db-guest}
      backend: {type: s3, s3: {...}}
      includeMemory: true
      deletionPolicy: Delete      # inherited by each snapshot; keep-N delete honors it
      ttl: 30d                    # optional per-snapshot max-age; composes with keepLast
status:
  lastScheduleTime: "..."         # when the controller last fired a tick
  lastSuccessfulTime: "..."       # last snapshot to reach Ready
  active: [{name: nightly-db-1717... }]   # in-flight (non-terminal) snapshots
  conditions: [Ready]
```

**Field rationale (mirrors CronJob where sensible, diverges where snapshots differ):**

- **`schedule`** — 5-field cron, parsed by `robfig/cron/v3` (the de-facto
  controller-ecosystem parser; CronJob-compatible). UTC, like the controller.
- **`concurrencyPolicy`** — `Forbid` (default): if the previous scheduled
  snapshot is still non-terminal (Capturing/Uploading), **skip** this tick
  (don't stack 2GiB captures). `Skip` is an alias for clarity; `Allow` lets them
  overlap (rarely wanted for snapshots). No `Replace` — you can't "replace" an
  in-flight immutable capture.
- **`startingDeadlineSeconds`** — if the controller was down and a tick is older
  than this, skip it (don't stampede catch-up captures on restart). Mirrors
  CronJob.
- **`suspend`** — stop firing without deleting history.
- **`retention.keepLast`** — count-based GC (the new primitive). `ttl` is
  age-based (Phase 5). A schedule may set either or both.

### 3.1 Created-snapshot naming + ownership

- Name: `<schedule>-<unix-ts>` (sortable, collision-free per tick) — mirrors
  CronJob's `<name>-<timestamp>`.
- Each SwiftSnapshot carries a **controller ownerRef to the schedule** (cascade
  GC on schedule delete) + a label `snapshot.kubeswift.io/schedule=<name>`
  (the keep-N grouping key, and a cheap `kubectl get ssnap -l` filter).

## 4. Controller behaviour

A new `internal/controller/swiftsnapshotschedule` reconciler:

1. **Suspend / parse** — if `suspend`, requeue far out; else parse `schedule`.
2. **Fire due ticks** — compute the most recent due time ≤ now from
   `lastScheduleTime`. If a tick is due (and not older than
   `startingDeadlineSeconds`), and `concurrencyPolicy` permits (no non-terminal
   schedule-owned snapshot for `Forbid`), **create** a SwiftSnapshot from the
   template (ownerRef + label + `<schedule>-<ts>` name). Stamp
   `status.lastScheduleTime`.
3. **keep-N GC** — list schedule-owned snapshots; of the **Ready** ones sorted
   newest-first, delete those beyond `keepLast` — but **skip any the
   `retentionBlocker` check flags** (referenced by a cloneFromSnapshot guest /
   in-flight restore) and **never delete a non-terminal** snapshot (it may still
   be capturing). Delete honors the snapshot's own `deletionPolicy` (the Phase 5
   cleanup path runs on the cascade).
4. **Requeue** to the next cron time (`RequeueAfter`), capped (e.g. ≤1h, like the
   ttl re-check) so a far-future tick still re-checks keep-N periodically.

`Owns(&SwiftSnapshot{})` so a child snapshot reaching Ready/Failed re-triggers
the schedule (updates `lastSuccessfulTime`, runs keep-N promptly).

### 4.1 keep-N + ttl + reference-block interaction

- **keep-N** deletes the oldest-beyond-N Ready snapshots.
- **ttl** (per snapshot, Phase 5) independently deletes any snapshot past its
  age — the swiftsnapshot controller already does this; the schedule doesn't
  re-implement it.
- **reference-block** (Phase 5 `retentionBlocker`) gates BOTH: a snapshot a clone
  pool / restore still uses is skipped by keep-N (and by ttl). It is exported /
  shared so the schedule controller calls the same check (lives in a neutral
  spot or is duplicated minimally — see OQ4).
- A Failed scheduled snapshot does NOT count toward `keepLast` (it's not a usable
  restore point) and is left for operator inspection (not auto-deleted in Phase
  6 — see OQ1).

## 5. Validation webhook (`vswiftsnapshotschedule`)

- `schedule` parses as valid cron (reject at admission with the parse error).
- `retention.keepLast >= 1` when set.
- `template.spec` passes the **same** SwiftSnapshot shape validation
  (`validateShape`) — reuse it so a bad template is caught at schedule-create,
  not at first tick.
- `concurrencyPolicy` enum; `startingDeadlineSeconds >= 0`.
- Per-operation discipline (Principle #10): ValidateCreate full; ValidateUpdate
  shape-only (schedule/template/retention are mutable — an operator may retune
  cadence/retention); ValidateDelete pass-through.

## 6. PR breakdown

| PR | Scope | Risk |
|---|---|---|
| **1** | This design doc. | — |
| **2** | `SwiftSnapshotSchedule` CRD + types (+ `robfig/cron/v3` dep) + `make generate` + chart. | Low |
| **3** | Schedule controller: cron eval + snapshot creation + concurrencyPolicy + status (`lastScheduleTime`/`lastSuccessfulTime`). | Med |
| **4** | keep-N GC (reference-aware, deletionPolicy-honoring) + a `kubeswift_snapshot_schedule_*` metric or two. | Med |
| **5** | Validation webhook (`vswiftsnapshotschedule`) + RBAC. | Low |
| **6** | `swiftctl schedule` (create/list/describe/delete) + samples + `docs/snapshots/scheduled-snapshots.md`. | Low |
| **7** | Cluster mini-walkthrough (short cron, watch ticks fire + keep-N prune; reference-block a scheduled snapshot via a clone). | — |

(2 + 3 may merge; 4 may merge into 3. Final shape decided as it's built.)

## 7. Open questions

- **OQ1 — Failed scheduled snapshots.** Proposed: don't count toward `keepLast`,
  don't auto-delete (operator inspects). Alternative: GC failed ones after a
  grace period. **Lean: leave them**, surface count in status; revisit if they
  pile up.
- **OQ2 — `concurrencyPolicy` default.** Proposed **Forbid** (skip a tick if the
  prior capture is still running — snapshots are heavy). Confirm.
- **OQ3 — cron parser.** Proposed `github.com/robfig/cron/v3` (new dep). It's the
  standard; small, well-tested, CronJob-compatible 5-field syntax. Alternative:
  hand-roll a 5-field parser to avoid the dep (more code, more bugs). **Lean
  robfig.**
- **OQ4 — where `retentionBlocker` lives.** It's currently unexported in
  `swiftsnapshot`. Options: (a) export it; (b) move it to a small shared helper;
  (c) duplicate the ~15-line list-and-check in the schedule controller. **Lean
  (a)** export `ReferenceBlocker(ctx, reader, snap)`.
- **OQ5 — keep-N scope key.** Group by the schedule ownerRef/label (this
  schedule's snapshots only) — NOT by source guest globally, so two schedules of
  the same guest keep independent budgets. Confirm.
- **OQ6 — time zone.** Controller-local UTC only in Phase 6 (no `spec.timeZone`).
  CronJob added TZ later; defer unless asked.

## 8. Non-goals

- New capture mechanism — Phase 6 only orchestrates existing SwiftSnapshots.
- Backup verification / restore-testing automation.
- Cross-cluster schedule replication.
- `spec.timeZone` (UTC only for now — OQ6).

## 9. Risks / W5 watch-items

- **Cron misfire on controller restart** — use `startingDeadlineSeconds` +
  `lastScheduleTime` to avoid a catch-up stampede; cluster-validate a restart
  mid-window.
- **keep-N races the in-flight capture** — never delete non-terminal snapshots;
  count only Ready toward the budget. Cluster-validate keepLast=1 with a tick
  firing while the previous is still Uploading.
- **Reference-block** — a scheduled snapshot cloned by a pool must survive keep-N
  eviction; reuse the Phase 5 check, cluster-validate.
- **CRD drift** — new CRD + `make generate` + chart sync (the recurring trap).

🤖 Generated with [Claude Code](https://claude.com/claude-code)
