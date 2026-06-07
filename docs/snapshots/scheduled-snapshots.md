# Scheduled Snapshots (`SwiftSnapshotSchedule`) + keep-N Retention

> Snapshot Phase 6. Snapshot a SwiftGuest on a cron schedule and keep only the
> most recent N. Builds entirely on the existing SwiftSnapshot machinery — it
> adds no new capture mechanism.

## At a glance

```yaml
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshotSchedule
metadata:
  name: nightly-db
spec:
  schedule: "0 2 * * *"        # 5-field cron, UTC
  concurrencyPolicy: Forbid    # Forbid (default) | Allow
  retention:
    keepLast: 7                # keep the 7 most recent Ready snapshots
  template:
    spec:                      # a SwiftSnapshotSpec — same fields as a hand-written SwiftSnapshot
      guestRef: {name: db}
      backend: {type: csi-volume-snapshot}
```

Each tick the controller creates a SwiftSnapshot named `<schedule>-<unix>`,
owned by the schedule and labelled `snapshot.kubeswift.io/schedule=<name>`.

## swiftctl

```bash
swiftctl schedule create nightly-db --guest db --schedule "0 2 * * *" --keep-last 7
swiftctl schedule list
swiftctl schedule describe nightly-db
swiftctl schedule delete nightly-db          # cascade-deletes the schedule's snapshots
```

For the **s3** backend, apply a YAML manifest (it needs bucket/endpoint/
credentials) — see [`config/samples/snapshot-schedule/02-schedule-s3-ttl.yaml`](../../config/samples/snapshot-schedule/02-schedule-s3-ttl.yaml).

## Semantics

- **Cron** is standard 5-field, evaluated in **UTC**. After a controller outage
  the schedule fires **at most one** catch-up snapshot (the most recent missed
  tick), never a backlog.
- **`concurrencyPolicy: Forbid`** (default) skips a tick while a prior scheduled
  snapshot is still capturing/uploading — captures are heavy; this prevents them
  stacking. `Allow` lets them overlap.
- **`startingDeadlineSeconds`** skips a tick missed by more than this (e.g. after
  an outage) instead of firing it late.
- **`suspend: true`** pauses firing without deleting the schedule or its snapshots.

## Retention — keep-N vs ttl

Two independent bounds that **compose** (whichever a snapshot crosses first
deletes it):

| | What | Where |
|---|---|---|
| **keep-N** | Keep the most recent N **Ready** snapshots of this schedule; prune older. | `SwiftSnapshotSchedule.spec.retention.keepLast` |
| **ttl** | Delete any snapshot older than its age. | `template.spec.ttl` (per-snapshot, Phase 5) |

keep-N safety:
- Only **Ready** snapshots count toward the budget and are eligible for pruning.
  A still-capturing snapshot is never deleted; a **Failed** one is left for
  inspection (it doesn't count toward `keepLast`).
- A snapshot still referenced by a `cloneFromSnapshot` SwiftGuest or an
  in-flight `SwiftRestore` is **skipped** by keep-N (and ttl) — pruned later once
  the reference clears (the shared reference-block gate).
- A pruned snapshot is deleted like any other, so its **`deletionPolicy`** runs:
  `Delete` purges the backend artifacts (hostPath / S3 objects), `Retain` keeps
  them.

## Backend guidance

- **csi-volume-snapshot** (recommended for scheduling): per-tick VolumeSnapshot,
  no shared state, crash-consistent, no VM pause.
- **s3**: per-tick object-store export; great for periodic offsite backups. Pair
  with `keepLast` and/or `ttl` + `deletionPolicy: Delete` to bound bucket growth.
- **local**: ⚠️ writes a **fixed `hostPath`**, so scheduled local snapshots
  overwrite each other — **don't schedule the local backend**; use csi or s3.

## Observability

`kubeswift_snapshot_schedule_pruned_total` counts keep-N deletions. Scheduled
snapshots also feed the standard `kubeswift_snapshot_total{backend,result}` and
latency/size metrics (see [`observability.md`](observability.md)).

## Constraints

- **`metadata.name` ≤ 40 characters** — the webhook enforces this so the derived
  snapshot and Job names (`<schedule>-<unix>-s3-delete`, …) stay within
  Kubernetes' 63-character limit.
- The cron expression and the `template.spec` are validated at schedule-create
  by the admission webhook (a bad template is rejected up front, not at the
  first tick).
