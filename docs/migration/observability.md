# Live Migration — Observability (Operator Guide)

> Live Migration Phase 5. Metrics, live progress, a Grafana dashboard, and
> retention guidance for SwiftMigration.

## Live progress in `kubectl`

A live migration's pre-copy progress is surfaced on the SwiftMigration:

```bash
kubectl get swiftmigration
# NAME        GUEST  FROM   TO    MODE  PHASE        PROGRESS  DOWNTIME  TRANSFER  AGE
# web-1-mig   web-1  miles  boba  live  StopAndCopy  52        <none>    <none>    14s
```

`status.transferProgress` (the **Progress** column) is an integer percentage,
read from the swiftletd-emitted `kubeswift.io/migration-progress-estimate`
annotation during the transferring substate and pinned to `100` once the source
reports transfer complete.

**It is a bandwidth heuristic** (Phase 3b design §5.4, calibrated on
Calico-VXLAN at ~107 MB/s), **not a byte-exact counter** — treat it as
approximate, and expect it to drift on CNIs far from that baseline. It is
populated for **live** migrations only; offline migration has no memory-transfer
RPC, so `transferProgress` stays unset there.

## Metrics

The controller-manager exposes Prometheus metrics on its metrics endpoint
(`/metrics`, default `:8443`/`:8080` per your deploy). Migration metrics:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kubeswift_migration_total` | counter | `mode` (live/offline), `result` (completed/failed/cancelled) | SwiftMigrations that reached a terminal phase. Recorded once per migration. |
| `kubeswift_migration_downtime_seconds` | histogram | `mode` | `status.observedDowntime` for **completed** migrations (sub-second live; tens of seconds offline). |

(These join the existing `kubeswift_guest_running_total`,
`kubeswift_vm_boot_seconds`, `kubeswift_vm_failures_total`, and
`kubeswift_image_import_seconds`.)

Useful queries:

```promql
# Migration success ratio over the last day
sum(increase(kubeswift_migration_total{result="completed"}[1d]))
  / clamp_min(sum(increase(kubeswift_migration_total[1d])), 1)

# p95 live-migration downtime
histogram_quantile(0.95,
  sum by (le) (rate(kubeswift_migration_downtime_seconds_bucket{mode="live"}[1h])))

# Failures by mode
sum by (mode) (increase(kubeswift_migration_total{result="failed"}[1h]))
```

### Scraping (Prometheus Operator)

If you run the Prometheus Operator, point a `ServiceMonitor`/`PodMonitor` at the
controller-manager metrics service. The controller-runtime metrics registry
(which these metrics register with) is served on the manager's metrics bind
address — match it to your deploy's `--metrics-bind-address`.

## Grafana dashboard

Import [`config/grafana/kubeswift-migrations.json`](../../config/grafana/kubeswift-migrations.json)
(Dashboards → Import → upload JSON) and select your Prometheus data source. It
shows: migrations by result, success ratio, mode/result breakdown, downtime
quantiles (p50/p95/p99) by mode, and a downtime heatmap — all from the two
metrics above.

## Retention

SwiftMigrations are not auto-deleted when they reach a terminal phase — the
object is kept so operators (and `kubectl get swiftmigration`) can see the
outcome.

- **Drain-created migrations** (`reason: node-drain`, named `<guest>-drain-<hash>`)
  are **owned by the SwiftGuest** (ownerReference) and are garbage-collected
  automatically when the guest is deleted — no operator action needed.
- **Operator-created migrations** (via `swiftctl migrate` or a hand-applied
  SwiftMigration) persist until deleted. Clean them up by age/label, e.g.:

  ```bash
  # delete all terminal migrations older than a day in a namespace
  kubectl get swiftmigration -n <ns> -o json \
    | jq -r '.items[] | select(.status.phase=="Completed" or .status.phase=="Failed" or .status.phase=="Cancelled")
             | .metadata.name' \
    | xargs -r kubectl delete swiftmigration -n <ns>
  ```

> A controller-side TTL auto-GC for terminal migrations is a possible future
> opt-in (a `--migration-ttl` flag, default disabled). It is intentionally not
> enabled by default — auto-deleting resources is a behavioral choice operators
> should opt into.

## See also

- [Phase 4 drain runbook](phase-4.md) — `kubectl drain` auto-evacuation.
- [Phase 3a reference](phase-3a.md) — live migration internals.
