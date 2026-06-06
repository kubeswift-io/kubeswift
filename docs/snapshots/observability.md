# Snapshot & Restore Observability

> Snapshot Phase 5. The KubeSwift controller-manager exports Prometheus metrics
> for the snapshot / restore / clone machinery. This page lists the metrics, the
> shipped Grafana dashboard, how to scrape them, and a few example queries.

## Where the metrics live

The controller-manager exposes `/metrics` on the
`kubeswift-controller-manager-metrics` Service (port **8080**, named `metrics`).
All snapshot metrics are registered on the controller-runtime registry alongside
the existing guest / image / migration metrics.

## Metric reference

Recorded once per resource on the **non-terminal → terminal transition**
(mirroring the migration metrics). Labels are deliberately low-cardinality —
`backend` × `result` only; no per-namespace label on the `result`-bearing series.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kubeswift_snapshot_total` | counter | `backend`, `result` (`ready`/`failed`) | SwiftSnapshots that reached a terminal phase |
| `kubeswift_snapshot_capture_seconds` | histogram | `backend` | capture latency (`capturedAt − creationTimestamp`) |
| `kubeswift_snapshot_pause_window_seconds` | histogram | `backend` | source-VM pause window during a memory capture (`observedPauseWindowMs`; local/s3 only) |
| `kubeswift_snapshot_upload_seconds` | histogram | — | Tier C upload latency (`s3.uploadedAt − capturedAt`) |
| `kubeswift_snapshot_size_bytes` | histogram | `backend` | total snapshot size at Ready (`totalSizeBytes`) |
| `kubeswift_snapshot_upload_bytes_total` | counter | — | artifact bytes pushed to S3 (wire traffic, excludes resume-skips) |
| `kubeswift_restore_total` | counter | `result` (`ready`/`failed`) | SwiftRestores that reached a terminal phase |
| `kubeswift_restore_seconds` | histogram | — | restore latency (`completedAt − startedAt`) |
| `kubeswift_restore_download_bytes_total` | counter | — | artifact bytes pulled from S3 by Tier C restore/clone downloads |
| `kubeswift_clone_total` | counter | `result` (`running`/`failed`) | cloneFromSnapshot SwiftGuests by terminal result |

> **Status vs metric for bytes.** `status.s3.uploadedBytes` /
> `SwiftRestore.status.downloadedBytes` carry the snapshot's S3 **footprint**
> (the full artifact size, stable across resumed transfers). The
> `…_bytes_total` counters carry the **wire traffic** (bytes actually moved,
> excluding objects skipped on a resumed Job). They differ on a resumed
> transfer.

## Grafana dashboard

[`config/grafana/kubeswift-snapshots.json`](../../config/grafana/kubeswift-snapshots.json)
— import it into Grafana and pick your Prometheus data source. Panels: snapshot
result rate + success ratio, restores by result, snapshots by backend, clones by
result, capture / restore / upload latency quantiles, memory pause-window p95,
Tier C bytes-moved rate, and a snapshot-size heatmap. (The companion live-
migration dashboard is
[`config/grafana/kubeswift-migrations.json`](../../config/grafana/kubeswift-migrations.json).)

## Scraping

Most clusters scrape the metrics Service via their existing Prometheus config.
If you run the **Prometheus Operator**, a ready-made (operator-gated, NOT part of
`make deploy`) ServiceMonitor is at
[`config/grafana/servicemonitor.yaml`](../../config/grafana/servicemonitor.yaml):

```bash
kubectl apply -f config/grafana/servicemonitor.yaml   # requires the monitoring.coreos.com CRDs
```

## Example queries

```promql
# Snapshot success ratio over the dashboard range
sum(increase(kubeswift_snapshot_total{result="ready"}[$__range]))
  / clamp_min(sum(increase(kubeswift_snapshot_total[$__range])), 1)

# p95 capture latency by backend (last hour)
histogram_quantile(0.95, sum by (le, backend) (rate(kubeswift_snapshot_capture_seconds_bucket[1h])))

# Tier C egress (upload) bandwidth
rate(kubeswift_snapshot_upload_bytes_total[5m])
```

### Suggested alerts

```promql
# Snapshots failing
sum(increase(kubeswift_snapshot_total{result="failed"}[15m])) > 0

# Restores failing
sum(increase(kubeswift_restore_total{result="failed"}[15m])) > 0
```

## Retention visibility

TTL-driven retention (`spec.ttl`) surfaces a **`RetentionBlocked`** condition on
the SwiftSnapshot when a TTL has elapsed but the snapshot is still referenced by
a `cloneFromSnapshot` SwiftGuest or an in-flight SwiftRestore:

```bash
kubectl get swiftsnapshot <name> -o jsonpath='{.status.conditions[?(@.type=="RetentionBlocked")]}'
```

A `RetentionBlocked=True` snapshot is not deleted until the references clear
(an operator-initiated `kubectl delete` is never blocked).
