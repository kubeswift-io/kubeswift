# Snapshot Phase 5 — operational polish (metrics, dashboards, retention)

> Status: DESIGN. Anchors on the Phase 5 roadmap line
> ([`kubeswift_context.md` §Snapshot Roadmap Continuation](../../kubeswift_context.md))
> and the deferred byte-gauges + S3-object-lifecycle tracked follow-ups.
> Last updated: 2026-06-05.

## 1. Goal

Make the shipped snapshot/restore/clone machinery **observable and
self-managing** for operators running it at scale:

1. **Metrics** — Prometheus counters + histograms for snapshot capture, upload,
   restore, download, and clone, so operators can alert and capacity-plan.
2. **Byte gauges** — surface how many bytes a Tier C upload/download actually
   moved (the deferred item: `status.s3.uploadedBytes` is declared but never
   populated; restore has no download-bytes field at all).
3. **Dashboards** — a Grafana dashboard over the snapshot/restore/migration/guest
   metrics, shipped as an asset.
4. **Retention** — `deletionPolicy` (with the missing **Tier C S3 object
   cleanup**) + a **TTL** auto-expiry, so snapshots don't accumulate forever and
   deleting one actually reclaims its backend bytes.

Phase 5 adds **no new runtime/boot mechanism**. It is controller + metrics +
one small `snapshot-s3` mode + assets.

## 2. What already exists (and what Phase 5 reuses / fixes)

| Primitive | Where | Phase 5 use |
|---|---|---|
| Central metrics registry (controller-runtime `metrics.Registry`) with guest / image / **migration** metrics | `internal/metrics/metrics.go` | **Extended** — add a snapshot/restore block here |
| Terminal-transition metric recording (`recordMigrationTerminal`, fired once on non-terminal→terminal via a `freshTerminal` guard) | `swiftmigration/controller.go` | **Mirrored** — `recordSnapshotTerminal` / `recordRestoreTerminal` |
| `status.capturedAt`, `status.observedPauseWindowMs`, `status.totalSizeBytes`, `status.s3.uploadedAt` | `swiftsnapshot_types.go` | **Read** to compute capture/upload latency + size histograms |
| `status.startedAt`, `status.completedAt` on SwiftRestore | `swiftrestore_types.go` | **Read** to compute restore latency |
| `status.s3.uploadedBytes` field (declared, **unpopulated**) | `swiftsnapshot_types.go:201` | **Populated** in Piece 2 |
| Tier B hostPath cleanup finalizer + cleanup pod | `swiftsnapshot/cleanup.go` (`HostPathFinalizer`) | **Pattern reused** for the Tier C S3 cleanup finalizer |
| `minioStore.remove(ctx, key)` (S3 object delete) | `cmd/snapshot-s3/store.go:99` | **Wired** to a new `--mode=delete` in Piece 4 |
| `snapshot-s3` manifest (`TotalBytes`, per-artifact `Bytes`) | `cmd/snapshot-s3/manifest.go` | **Read** for the byte report + the delete key-list |

What is **missing today** (the gaps Phase 5 closes):

- **No snapshot/restore metrics at all.** Migrations have them; snapshots/restores
  do not.
- **`uploadedBytes` is never assigned** — no code path populates it, and there is
  no download-bytes equivalent.
- **Tier C snapshots leak.** Deleting an `s3`-backend SwiftSnapshot removes the
  CR but **nothing purges the bucket objects** — `HostPathFinalizer` is Tier B
  only. (Today, name-reuse relies on PR #118's checksum re-upload to overwrite
  stale artifacts; that is not deletion.)
- **No retention.** There is no `deletionPolicy` and no TTL — snapshots live until
  hand-deleted.

## 3. Piece 1 — Metrics

Add a snapshot/restore block to `internal/metrics/metrics.go`, registered in
`init()` alongside the existing metrics. Record on the **non-terminal→terminal
transition** in each controller, mirroring `recordMigrationTerminal` exactly
(idempotent via the existing `freshTerminal`/once guard so reconcile retries do
not double-count).

### 3.1 Metric inventory

| Metric | Type | Labels | Source |
|---|---|---|---|
| `kubeswift_snapshot_total` | CounterVec | `backend`, `result` (`ready`/`failed`) | swiftsnapshot terminal transition |
| `kubeswift_snapshot_capture_seconds` | HistogramVec | `backend` | `capturedAt − creationTimestamp` (capture latency) |
| `kubeswift_snapshot_pause_window_seconds` | HistogramVec | `backend` | `observedPauseWindowMs` (Tier B/s3 only) |
| `kubeswift_snapshot_upload_seconds` | HistogramVec | — | `s3.uploadedAt − capturedAt` (Tier C only) |
| `kubeswift_snapshot_size_bytes` | HistogramVec | `backend` | `totalSizeBytes` observed once at Ready |
| `kubeswift_restore_total` | CounterVec | `backend`, `result` (`ready`/`failed`) | swiftrestore terminal transition |
| `kubeswift_restore_seconds` | HistogramVec | `backend` | `completedAt − startedAt` |
| `kubeswift_clone_total` | CounterVec | `result` (`running`/`failed`) | swiftguest cloneFromSnapshot reaches Running/Failed (once per guest) |

`backend` ∈ {`csi-volume-snapshot`, `local`, `s3`}. Bucket choices follow the
existing histograms (boot/migration/import) — capture/restore in the
5s–900s range; pause-window 0.5s–60s; size in log-spaced byte buckets.

> **No `namespace` label on the counters with a `result` cardinality.** The
> migration metrics deliberately omit `namespace` (bounded cardinality). Keep the
> same discipline: `backend`×`result` is small and stable; add `namespace` only
> to the gauges where it is naturally bounded. (W5/observability hygiene: a
> per-namespace × per-result histogram is an unbounded-cardinality trap.)

### 3.2 Recording sites

- `swiftsnapshot/controller.go` — at the point the phase first becomes
  `Ready`/`Failed` (the same place `ensureFinalizer` fires for Ready), behind a
  fresh-terminal guard. Capture/upload/pause/size observed there from `status`.
- `swiftrestore/controller.go` — at the first `Ready`/`Failed` transition.
- `swiftguest/clone.go`/`controller.go` — a clone guest reaching Running (the
  `resumeCloneIfNeeded` site already gates on Running) increments
  `kubeswift_clone_total{result="running"}` **once** (guard with an in-memory
  observed-set like `MarkVMBootObserved`, or a status stamp).

Unit tests mirror `controller_metrics_test.go` (`testutil.ToFloat64` /
`CollectAndCount`, assert +1 on transition, no double-count on re-reconcile).

## 4. Piece 2 — Byte gauges (the deferred item)

### 4.1 The gap and the cheap-vs-precise tradeoff

`status.s3.uploadedBytes` exists but is never set; restore has no download-bytes
field. Two fidelities:

- **Cheap (no binary change):** the controller stamps bytes = the snapshot's
  `totalSizeBytes` when the Job completes. Correct **except** it overcounts on a
  resumed transfer (PR #118 skips already-present, checksum-matching artifacts).
- **Precise:** the `snapshot-s3` binary reports the bytes it **actually
  transferred** (excluding skips).

The roadmap scoped the precise path ("the snapshot-s3 binary reports counts via a
pod annotation the controller reads"). **This design improves on that: report via
the container *termination message*, not a self-annotation.**

### 4.2 Mechanism — terminationMessage (no new RBAC)

The `snapshot-s3` Job container writes a tiny JSON to its
`terminationMessagePath` (default `/dev/termination-log`) on exit:

```json
{"transferredBytes": 4326154986, "skippedBytes": 0, "totalBytes": 4326154986}
```

Kubernetes copies that into
`pod.status.containerStatuses[0].state.terminated.message`. The controller —
which already watches the Job — lists the Job's pod (by the `job-name` label) on
completion and reads the message. **Zero new RBAC, zero downward API, zero kube
client in the binary** — strictly better than the binary patching its own pod
(which would need `pods` patch on a deliberately minimal-cap Job). The store
already tracks per-artifact byte counts and skip decisions
(`transfer.go`), so emitting the JSON is a few lines at the end of
`runUpload`/`runDownload`.

### 4.3 Surfaces — footprint (status) vs wire traffic (metric)

The report carries `transferredBytes` (actual wire bytes, excluding
resume-skips), `skippedBytes`, and `totalBytes` (the snapshot's full artifact
footprint). The two surfaces split cleanly:

- **Status fields = footprint (`totalBytes`)** — stable, operator-meaningful
  ("how big is this snapshot in S3 / how much was materialized"):
  - `status.s3.uploadedBytes` (existing field) — stamped by the swiftsnapshot
    controller at `Uploading→Ready`.
  - **New** `status.downloadedBytes` on SwiftRestore — stamped at the download
    Job's completion (guarded on the already-set field so a Tier-B-tail requeue
    doesn't re-read). Clones have no status field — metric only.
- **Metrics = wire traffic (`transferredBytes`)** — bandwidth/cost, excludes
  resumed skips: `kubeswift_snapshot_upload_bytes_total` and
  `kubeswift_restore_download_bytes_total` (the clone download increments the
  latter, deduped per `(node, snapshot)` so the shared Job counts once).
- Read via `clonecommon.JobTransferReport` (lists the Job's pod by the
  `job-name` label, parses the terminated container's message). Missing/garbled
  → bytes stay 0, no metric, never a failure.

> Defensive: termination message missing/garbled → leave the byte field nil and
> the metric un-incremented (Design Principle #6: no fabricated status). The
> snapshot/restore is **not** failed on a missing byte report — it is a metrics
> surface only.

## 5. Piece 3 — Dashboards

Ship a Grafana dashboard JSON under `dashboards/` (new dir) + an operator doc
section. Panels: snapshot/restore rate & success ratio (by backend), capture /
upload / restore latency p50/p95 (histogram_quantile), pause-window p95, bytes
moved (upload/download rate), snapshot size distribution, alongside the existing
migration/guest panels. A `ServiceMonitor`/`PodMonitor` sample (commented,
Prometheus-Operator-gated) referencing the controller-manager metrics endpoint.
No code — assets + `docs/snapshots/observability.md`. Sequenced **after** Pieces 1–2
so every panel has a live series.

## 6. Piece 4 — Retention (the design-heavy piece)

### 6.1 Decisions (settled here, per "design-doc first")

1. **`spec.deletionPolicy: Delete | Retain`** on SwiftSnapshot, `+kubebuilder:default=Delete`.
   - `Delete` (default): on CR deletion, **purge the backend artifacts** — Tier B
     hostPath (already done) **and Tier C S3 objects (NEW)** — then drop the
     finalizer.
   - `Retain`: drop the finalizer **without** purging (operator keeps the bucket
     objects / hostPath for out-of-band archival).
   - csi-volume-snapshot is unaffected (VolumeSnapshot lifecycle is its own
     `deletionPolicy` on the VolumeSnapshotClass; we do not double-manage it).
2. **Tier C S3 object cleanup** is a **prerequisite** and ships as its own PR
   (closes the existing "S3 object lifecycle on snapshot deletion" follow-up):
   - New `snapshot-s3 --mode=delete --bucket --key-prefix` that lists the
     snapshot's key prefix and `remove()`s every object (the `remove` primitive
     already exists; `--mode=delete` is the only new wiring + a manifest-driven or
     prefix-list key set).
   - New `S3ObjectFinalizer = "kubeswift.io/snapshot-s3-cleanup"` added to `s3`
     snapshots at Ready (mirrors `HostPathFinalizer`). `handleDeletion` forks on
     backend: Tier B → cleanup pod (existing); Tier C → a node-agnostic delete
     **Job** running `snapshot-s3 --mode=delete`. `Retain` skips the purge and
     drops the finalizer immediately.
3. **`spec.ttl` (`metav1.Duration`, optional)** on SwiftSnapshot. When set, the
   controller deletes the SwiftSnapshot once `now ≥ capturedAt + ttl`
   (`RequeueAfter` the remaining TTL while Ready; issue `Delete(self)` at expiry,
   which then runs the `deletionPolicy` purge via the finalizer). TTL on a
   not-yet-Ready snapshot is dormant (no `capturedAt` to anchor on).
4. **Keep-N is DEFERRED.** Keep-last-N-per-source only pays off with a snapshot
   *scheduler* (a `CronSnapshot`/`SwiftSnapshotSchedule`), which KubeSwift does
   not have — operators create snapshots by hand today. Keep-N + scheduling is a
   coherent **future** phase; Phase 5 ships TTL + deletionPolicy, which covers the
   "don't accumulate forever / reclaim bytes on delete" need without a new CRD.

### 6.2 Retention must not delete a referenced snapshot (lifetime-guard overlap)

TTL-driven GC could try to delete a snapshot a **cloneFromSnapshot guest/pool**
still depends on (a clone needs the live snapshot to (re)boot), or one an
**in-flight SwiftRestore** is reading. Phase 4's snapshot-lifetime-guard
follow-up (OQ2) is unbuilt, so Phase 5 retention ships a **reference-aware skip**:

- Before issuing the TTL `Delete`, the controller lists referencing
  `SwiftGuest`s (`spec.cloneFromSnapshot.snapshotRef.name == snap.name`) and
  non-terminal `SwiftRestore`s (`spec.snapshotRef.name == snap.name`). If any
  exist, **skip the delete**, set a `RetentionBlocked` condition (reason
  `ReferencedBySwiftGuest`/`ReferencedBySwiftRestore`), emit an Event, and
  requeue. TTL expiry on a referenced snapshot is a no-op until the references
  clear.
- This is a **lightweight, retention-scoped** form of the lifetime guard. It does
  **not** block an *operator-initiated* `kubectl delete` (that stays the
  operator's call); a stronger admission-time guard remains the OQ2 follow-up.
  Document the distinction.

### 6.3 GC loop placement

In the swiftsnapshot reconciler's Ready branch (today just `ensureFinalizer` +
return): if `spec.ttl` is set, compute `expiry = capturedAt + ttl`; if `now ≥
expiry` run the reference check then `Delete(self)`; else
`RequeueAfter(expiry − now)` (capped so a very long TTL still re-checks
periodically, e.g. `min(remaining, 1h)`). The existing 5s deletion-handler
requeue is unchanged.

## 7. PR breakdown / sequencing

| PR | Scope | Risk |
|---|---|---|
| **1** | **Metrics** — `internal/metrics` block + `recordSnapshotTerminal`/`recordRestoreTerminal`/clone counter + unit tests. No CRD change. | Low; foundational |
| **2** | **Byte gauges** — `snapshot-s3` writes the termination-message JSON; controllers read it → `s3.uploadedBytes` (existing) + new `SwiftRestore.status.downloadedBytes` + the two byte counters. CRD change (restore status field) → `make generate` + chart sync. | Low-med |
| **3** | **Tier C S3 object cleanup** — `snapshot-s3 --mode=delete` + `S3ObjectFinalizer` + `handleDeletion` Tier C fork + delete Job. (Closes the S3-lifecycle follow-up. Independent of `deletionPolicy`: default-Delete behavior.) | Med (deletion path) |
| **4** | **`deletionPolicy: Delete \| Retain`** — CRD field gating PR 3's purge; webhook default; honor on both Tier B and Tier C. | Low-med |
| **5** | **`spec.ttl` + reference-aware GC loop** + `RetentionBlocked` condition. | Med (GC safety) |
| **6** | **Dashboards** — Grafana JSON + `docs/snapshots/observability.md` + ServiceMonitor sample. | Low; assets |

Cluster mini-walkthrough after PR 5 (the retention path is the one with real
multi-controller surface — the W5 pattern lives here: a TTL-GC that deletes a
pool-referenced snapshot, or an S3 delete Job that 403s on a missing-cred path,
won't show up in fake-client tests).

## 8. Open questions — RESOLVED (2026-06-05)

- **OQ1 — delete-Job key set → (b) prefix-scoped list-and-remove**, with an
  empty/short-prefix guard. The prefix is per-`<namespace>/<snapshot>` (blast
  radius = one snapshot) and (b) also reaps orphans a manifest-driven delete
  would miss (a partial upload that never reached `manifest.json`, PR #118's
  stale same-size objects). Requires `s3:ListBucket` on the prefix alongside
  delete — standard for creds that already get/put there.
- **OQ2 — TTL re-check cap → 1h.** Bounds worst-case staleness after a controller
  restart / clock skew to ≤1h past expiry without pinning a reconcile for a
  30-day TTL. Matches the project's other backstop intervals (30m migration
  timeout, SyncPeriod defense-in-depth).
- **OQ3 — `deletionPolicy` on csi-volume-snapshot → ignored + a webhook
  Warning.** The VolumeSnapshotClass `deletionPolicy` + ownerRefs govern the
  VolumeSnapshot; our field is a no-op there. A non-blocking `admission.Warnings`
  from `vswiftsnapshot` makes that visible in `kubectl` (no silent surprise,
  Principle #6) without rejecting a valid spec.
- **OQ4 — bytes on a *failed* transfer → leave nil, no `…_failed_bytes`
  metric.** A partial count isn't operationally meaningful; the failure is already
  captured by `kubeswift_snapshot_total{result="failed"}`.

### 8.1 PR 1 implementation note (metrics)

`kubeswift_restore_total` / `kubeswift_restore_seconds` carry **no `backend`
label** (the §3.1 inventory listed one). A `SwiftRestore` does not store its
source snapshot's backend, and a status-path `Get` purely to recover it for a
label isn't worth the cost. Snapshot metrics keep `backend` (the
`SwiftSnapshot` object has it). If a backend split on restore is wanted later,
PR 2 (which already touches restore status for `downloadedBytes`) can stamp a
`status.backend` to label off.

## 9. Non-goals

- Snapshot **scheduling** (CronSnapshot) and **keep-N** retention — a future
  phase; Phase 5 ships TTL + deletionPolicy.
- Admission-time snapshot-lifetime guard (OQ2 follow-up) — Phase 5 ships only the
  retention-scoped reference-aware skip, not a hard delete-block.
- Orphan **hostPath** janitor (already explicitly out of scope in `cleanup.go`).

## 10. Risks / W5 watch-items

- **Cardinality.** `result` × `backend` only on counters; no `namespace` on
  `result`-bearing series. (§3.1.)
- **S3 delete Job RBAC/creds.** The delete Job needs the same creds Secret + the
  same run-as-root/minimal-cap posture as the upload/download Jobs; a missing-cred
  or HTTP-vs-HTTPS misconfig 403s only on cluster, not in fake-client tests
  (the exact PR #117 trap). Cluster-validate.
- **Reference-aware GC race.** A clone created *between* the reference list and
  the `Delete` could still be orphaned; the clone boot path already fails cleanly
  if the snapshot is gone (it requires a Ready snapshot), so the window is a
  transient clone-failure, not data loss. Note in the runbook.
- **CRD drift.** PRs 2/4/5 add status/spec fields → `make generate` + copy to
  `charts/kubeswift/crds/` or the apiserver silently strips them (the recurring
  trap).

🤖 Generated with [Claude Code](https://claude.com/claude-code)
