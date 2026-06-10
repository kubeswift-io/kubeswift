# KubeSwift Observability

KubeSwift exports Prometheus metrics from the controller-manager and ships a
ServiceMonitor, six Grafana dashboards, and a starter alert pack — all behind
one Helm flag.

## Enable it

The chart packages everything behind `monitoring.enabled` (default **off**, so
default installs incur no Prometheus-Operator CRD dependency):

```bash
helm upgrade --install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  --set monitoring.enabled=true \
  --set monitoring.dashboards.namespace=<your-grafana-namespace>
```

Requirements when enabled:

- The Prometheus Operator CRDs (`monitoring.coreos.com`) — e.g.
  kube-prometheus-stack — for the ServiceMonitor and PrometheusRule.
- A Grafana with the dashboard sidecar (the kube-prometheus-stack default) for
  the dashboard ConfigMaps. Set `monitoring.dashboards.namespace` to the
  namespace that Grafana's sidecar watches (often Grafana's own namespace).

Sub-toggles: `monitoring.serviceMonitor.enabled`, `monitoring.dashboards.enabled`,
`monitoring.prometheusRule.enabled` (all default true once `monitoring.enabled`).

> **Selector labels — the common gotcha.** kube-prometheus-stack's Prometheus
> selects `ServiceMonitor`s and `PrometheusRule`s **by label**. Its
> `ruleSelector` defaults to `release: <your-kps-release>`, so unless you set
> that label the alert rules are created but never loaded (the ServiceMonitor
> may still be picked up if `serviceMonitorSelectorNilUsesHelmValues=false`, a
> common kps setting — the rule selector is *not* relaxed by that flag). Match
> your Prometheus's selectors:
>
> ```bash
> --set monitoring.prometheusRule.additionalLabels.release=<your-kps-release> \
> --set monitoring.serviceMonitor.additionalLabels.release=<your-kps-release>
> ```
>
> Check what your Prometheus wants with
> `kubectl get prometheus -A -o jsonpath='{.items[0].spec.ruleSelector}'`.

Non-Helm installs can apply `config/grafana/servicemonitor.yaml` and load the
dashboards under `config/grafana/*.json` as ConfigMaps labeled
`grafana_dashboard: "1"`.

## Dashboards

| Dashboard | uid | Answers |
|---|---|---|
| Fleet Overview | `kubeswift-fleet` | how many guests, where, how healthy; the front door |
| VM Lifecycle & Images | `kubeswift-vm-lifecycle` | boot latency, failure reasons, image-import health, kernels, per-VM usage recipe |
| GPU | `kubeswift-gpu` | per-node capacity/free/allocated, alloc/release rate, vfio-ready, discovery staleness |
| Snapshots & Restore | `kubeswift-snapshots` | snapshot/restore/clone results + latencies, schedule prune/staleness |
| Live Migration | `kubeswift-migrations` | results, downtime + transfer quantiles, failures-by-reason, drain activity |
| Control Plane | `kubeswift-control-plane` | reconcile/workqueue/webhook health, leader, manager memory |

## Metrics

### Feature metrics (controller-recorded)

| Metric | Type | Labels |
|---|---|---|
| `kubeswift_vm_boot_seconds` | histogram | namespace |
| `kubeswift_vm_failures_total` | counter | namespace, reason (bounded condition reason) |
| `kubeswift_image_import_seconds` | histogram | namespace |
| `kubeswift_image_import_total` | counter | namespace, result |
| `kubeswift_migration_total` | counter | mode, result |
| `kubeswift_migration_failures_total` | counter | mode, reason |
| `kubeswift_migration_downtime_seconds` / `_transfer_seconds` | histogram | mode |
| `kubeswift_snapshot_total` / `_capture_seconds` / `_pause_window_seconds` / `_size_bytes` | counter/histogram | backend |
| `kubeswift_restore_total` / `_seconds`, `kubeswift_clone_total` | counter/histogram | result |
| `kubeswift_snapshot_upload_bytes_total` / `kubeswift_restore_download_bytes_total` | counter | — |
| `kubeswift_snapshot_schedule_pruned_total` | counter | — |
| `kubeswift_gpu_allocations_total` | counter | result (allocated\|no_capacity) |
| `kubeswift_gpu_releases_total` | counter | — |
| `kubeswift_drain_migrations_total` | counter | policy, result |

### State gauges (computed from cluster state at scrape time)

Exported by an in-controller cache-backed collector — immune to controller
restart drift (an event-accumulated gauge would reset to 0 on restart):

`kubeswift_guests{namespace,phase}`, `kubeswift_guests_by_node{node,hypervisor}`,
`kubeswift_guests_by_boot_source{source}`,
`kubeswift_pool_replicas{namespace,pool,state}`,
`kubeswift_images{namespace,phase}`, `kubeswift_kernels{phase}`,
`kubeswift_snapshots{namespace,phase}`, `kubeswift_migrations_active{mode}`,
`kubeswift_gpu_node_gpus{node,state}`, `kubeswift_gpu_node_info{...}`,
`kubeswift_gpu_node_last_discovery_timestamp_seconds{node}`,
`kubeswift_snapshot_schedule_last_success_timestamp_seconds{namespace,schedule}`.

`kubeswift_guest_running_total{namespace}` is **deprecated** — use
`kubeswift_guests{phase="Running"}`. It will be removed a release after v0.4.

### Free controller-runtime metrics

The controller-manager also exports the standard controller-runtime surface,
which the Control Plane dashboard consumes — no KubeSwift code needed:
`controller_runtime_reconcile_total` / `_errors_total` / `_time_seconds`,
`workqueue_*`, `controller_runtime_webhook_requests_total{webhook,code}` +
`_webhook_latency_seconds` (covers all 7 CRD webhooks + the eviction webhook —
drain denials are `code="429"`), `leader_election_master_status`, Go/process
metrics.

## Per-VM resource usage (cAdvisor)

There is **no per-VM metrics endpoint** in v1, and it isn't needed: Cloud
Hypervisor runs inside the launcher container's cgroup, so cAdvisor's
`container_*` series on launcher pods already report the VM's host-side
CPU/memory/network/disk. Join on the launcher pod label `swift.kubeswift.io/guest`:

```promql
sum by (guest) (
  rate(container_cpu_usage_seconds_total[5m])
  * on (pod) group_left(guest)
    label_replace(
      kube_pod_labels{label_swift_kubeswift_io_guest!=""},
      "guest", "$1", "label_swift_kubeswift_io_guest", "(.+)")
)
```

This needs kube-state-metrics started with
`--metric-labels-allowlist=pods=[swift.kubeswift.io/guest]` so the label reaches
`kube_pod_labels`.

**Never join on pod name** — after a live migration the launcher pod is renamed
`<guest>-mig-<uid>`, so a pod-name join silently loses the migrated guest. The
guest label is stable across migration.

## Per-CR series (optional, no KubeSwift code)

The state gauges are aggregates (by namespace/phase/node) to keep cardinality
bounded. For per-CR-name series, layer kube-state-metrics
`CustomResourceStateMetrics` on top — a zero-code add-on. A starter config is in
[`config/samples/monitoring/kube-state-metrics-crs.yaml`](../../config/samples/monitoring/kube-state-metrics-crs.yaml).

## Alerts

See [alerts.md](alerts.md) for the 12-rule starter pack, what each alert means,
and the first diagnostic step. Certificate expiry is intentionally **not**
covered here — cert-manager owns
`certmanager_certificate_expiration_timestamp_seconds`.
