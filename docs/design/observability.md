# KubeSwift Observability — Architecture & Phased Plan

> Staff-architect gap analysis and phased plan for a complete operator
> observability experience across every shipped feature arc.
> Reviewed at v0.3.1 (main @ 581f936).
> Status: **SHIPPED** — phases O1–O4 complete + cluster-validated (PRs
> #199, #200, #201, #202, #203, #204, #205, #206). O5 deferred to v2.
> Operator entry point: [`docs/observability/README.md`](../observability/README.md).

## Shipped summary (O1–O4)

| Phase | PRs | What landed |
|---|---|---|
| O1 | #199, #200 | Provisioning-native dashboards + `make verify-dashboards` lint; Helm `monitoring.*` gate (ServiceMonitor + dashboard ConfigMaps) |
| O2 | #201 | Cache-backed `StateCollector` (11 gauge families); fixed `guest_running_total` restart-drift + `vm_failures_total` cardinality |
| O3 | #202, #203 | GPU alloc/release + drain counters; image-import outcome counter + migration failure-reason breakdown |
| O4 | #204, #205, #206 | Six-dashboard taxonomy (Fleet, VM Lifecycle, GPU, Snapshots, Migration, Control Plane); 12-rule PrometheusRule pack; observability runbook |

Cluster-validated on the lab kube-prometheus-stack throughout: the O2
restart-drift repro (gauges re-converge after `rollout restart`), the O3
counters on a real GTX 1080 alloc/release cycle, and the O4 alert pipeline
(`KubeSwiftImageImportStuck` → pending on a real stuck import). Operator
finding captured in the runbook: kube-prometheus-stack selects
PrometheusRules by the `release` label (not relaxed by
`serviceMonitorSelectorNilUsesHelmValues=false`) — set
`monitoring.prometheusRule.additionalLabels.release`.

## Goal

Users of KubeSwift get a complete observability experience for all
implemented features: every feature arc answerable from metrics, a coherent
dashboard set that works out-of-the-box, a starter alert pack, and packaging
that ships with the Helm chart — honoring the project's design principles
(minimalism, Kubernetes-native, no silent failures).

---

## 1. Inventory: the existing metrics surface

### 1.1 Custom controller metrics (`internal/metrics/metrics.go`)

All metrics register on the controller-runtime registry in `init()` and are
served from the controller-manager's `/metrics` on `:8080`
(`cmd/controller-manager/main.go`, `--metrics-bind-address`).

| # | Metric | Type | Labels | Recorded at |
|---|---|---|---|---|
| 1 | `kubeswift_guest_running_total` | GaugeVec | `namespace` | swiftguest controller, Inc/Dec on Running transitions — **DEFECT, see §1.5** |
| 2 | `kubeswift_vm_boot_seconds` | HistogramVec (5–180s) | `namespace` | pod creation → GuestRunning=True, deduped in-memory |
| 3 | `kubeswift_vm_failures_total` | CounterVec | `namespace`, `reason` | transition to Failed — **DEFECT (cardinality), see §1.5** |
| 4 | `kubeswift_image_import_seconds` | HistogramVec (30–900s) | `namespace` | SwiftImage Ready — success-latency only, **no failure counter** |
| 5 | `kubeswift_migration_total` | CounterVec | `mode`, `result` | terminal transition |
| 6 | `kubeswift_migration_downtime_seconds` | HistogramVec (0.5–120s) | `mode` | from `status.observedDowntime` |
| 7 | `kubeswift_migration_transfer_seconds` | HistogramVec (5–600s) | `mode` | from `status.observedTransferDuration` |
| 8 | `kubeswift_snapshot_total` | CounterVec | `backend`, `result` | terminal transition |
| 9 | `kubeswift_snapshot_capture_seconds` | HistogramVec (1–900s) | `backend` | capturedAt − creation |
| 10 | `kubeswift_snapshot_pause_window_seconds` | HistogramVec (0.1–60s) | `backend` | from `observedPauseWindowMs` |
| 11 | `kubeswift_snapshot_upload_seconds` | Histogram | — | Tier C upload |
| 12 | `kubeswift_snapshot_size_bytes` | HistogramVec (exp 64MiB–~128GiB) | `backend` | |
| 13 | `kubeswift_restore_total` | CounterVec | `result` | terminal transition |
| 14 | `kubeswift_restore_seconds` | Histogram (5–300s) | — | |
| 15 | `kubeswift_clone_total` | CounterVec | `result` | cloneFromSnapshot guest running/failed |
| 16 | `kubeswift_snapshot_upload_bytes_total` | Counter | — | via Job termination-message report |
| 17 | `kubeswift_restore_download_bytes_total` | Counter | — | via Job termination-message report |
| 18 | `kubeswift_snapshot_schedule_pruned_total` | Counter | — | keep-N GC — **orphaned: on no dashboard** |

### 1.2 Free controller-runtime metrics (exported today, unscraped by any dashboard)

The manager is standard controller-runtime; `/metrics` already serves:

- `controller_runtime_reconcile_total{controller,result}`, `_reconcile_errors_total`,
  `_reconcile_time_seconds`, `_max_concurrent_reconciles` — for **all** reconcilers,
  including the ones with zero custom metrics (swiftgpu, swiftguestpool,
  swiftkernel, swiftdrain, migrationcert).
- `workqueue_depth{name}`, `_queue_duration_seconds`, `_work_duration_seconds`,
  `_retries_total`, `_unfinished_work_seconds`, `_longest_running_processor_seconds`.
- `controller_runtime_webhook_requests_total{webhook,code}` +
  `_webhook_latency_seconds{webhook}` — **already covers webhook rejection rates**
  for all 7 CRD validating webhooks AND the `veviction` pods/eviction webhook
  (drain denials surface as 429s). No custom webhook metrics needed —
  only dashboards/alerts.
- `rest_client_requests_total`, `leader_election_master_status`, Go/process metrics.

### 1.3 Components that expose nothing (and the v1 verdict)

| Component | Finding |
|---|---|
| **swiftletd (Rust)** | Zero metrics, no HTTP listener of any kind; reporting surface is pod annotations. `swift-ch-client` has no `vm.counters` method today (CH exposes one). **Verdict: stays metric-less in v1 — see D2.** |
| **gpu-discovery** | No listener; inventory lives only in SwiftGPUNode status. Covered by the state collector (D1). |
| **swiftgpu / swiftguestpool / swiftkernel / swiftdrain controllers** | Zero custom recordings; only free reconcile metrics. Gap-filled in O2/O3. |
| **snapshot-s3 / snapshot-stager Jobs** | Correctly metric-less (short-lived); bytes flow via termination message → controller counters. Keep this pattern. |
| **Launcher pods** | Not scraped, but every launcher pod carries the `swift.kubeswift.io/guest` label — the join key for cAdvisor/kube-state-metrics (see D2). |

### 1.4 Existing dashboards / packaging

- `config/grafana/kubeswift-migrations.json` (6 panels), `kubeswift-snapshots.json`
  (11 panels) — **both are import-style, not provisioning-native**: they carry
  `__inputs` + `${DS_PROMETHEUS}` references that break under sidecar/ConfigMap
  provisioning (confirmed live on the lab: dashboards loaded but panels showed
  no data until the inputs were stripped).
- `config/grafana/servicemonitor.yaml` — apply-by-hand sample.
- Helm chart: metrics Service exists; **no** `monitoring.*` values, no
  ServiceMonitor, no dashboard ConfigMaps, no PrometheusRule.
- Neither dashboard covers `kubeswift_snapshot_schedule_pruned_total`,
  any `controller_runtime_*` series, or any guest/image/GPU metric.

### 1.5 Two latent defects found during inventory (fixed in Phase O2)

1. **`kubeswift_guest_running_total` drifts on controller restart.** It is an
   Inc/Dec gauge driven purely by phase *transitions*. After a restart the gauge
   resets to 0; already-Running guests produce no transition, so it stays 0 —
   or goes *negative* when one of them later stops. Violates Design Principle
   #6 (no silent failures). Fix is structural: state gauges must be computed
   from cluster state at scrape time (D1), not event-accumulated.
2. **`kubeswift_vm_failures_total{reason=…}` uses the condition `Message` as a
   label value.** Messages are free text (pod names, node names, error strings)
   — unbounded series cardinality. Fix: use the bounded condition `Reason`.

---

## 2. Feature → coverage matrix

| Feature arc | Metrics today | Dashboard today | Missing |
|---|---|---|---|
| VM lifecycle (SwiftGuest) | #1 (buggy), #2, #3 (hazardous) | none | guests **by phase** / **by node** / by hypervisor / by boot source; stuck-state detection |
| Images (SwiftImage) | #4 (success only) | none | import **failure** counter; images-by-phase (stuck imports) |
| Kernels (SwiftKernel) | nothing | none | kernels-by-phase state gauge (low priority) |
| Fleets (SwiftGuestPool) | nothing | none | desired vs ready vs failed vs updated per pool (pure state export — `status.*Replicas` already exist) |
| GPU (SwiftGPUNode/Profile) | nothing | none | per-node total/free, vfioReady, FM partitions, allocation/release counters, discovery staleness |
| Snapshots (Phases 0–6) | 8 metrics — best covered | snapshots dashboard | schedule panels (#18 orphaned), schedule staleness gauge, snapshots-by-phase |
| Restore/clone | #13–#15, #17 | covered | fine for v1 |
| Migration (Phases 1–5) | #5–#7 | migrations dashboard | failed-by-reason (bounded enum label), drain activity, active in-flight gauge |
| Windows guests | n/a | n/a | optional bounded `osType` label on guests gauge |
| vhost-user devices | nothing | none | guests-with-backends count via state collector spec inspection (no runtime device metrics in v1) |
| Webhooks (7 CRD + eviction) | free `controller_runtime_webhook_*` | none | dashboards/alerts only — no code |
| Control plane | free reconcile/workqueue | none | dashboards/alerts only — no code |
| Per-guest CPU/mem/net/disk | none custom | none | **answerable without KubeSwift code**: CH runs inside the launcher container's cgroup, so cAdvisor `container_*` series on launcher pods ARE the VM's host-side usage. Join on the `swift.kubeswift.io/guest` pod label (needs kube-state-metrics `--metric-labels-allowlist`). **Never join on pod name** — post-live-migration pods are `<guest>-mig-<uid>` (the W26 canonical-name lesson applies to dashboards too). |

---

## 3. Architectural decisions

### D1 — CR-state metrics: in-controller cache-backed collector (primary); KSM CRS config as documented optional add-on

Implement a custom `prometheus.Collector` in the controller-manager that reads
the **informer cache at scrape time**, replacing the broken Inc/Dec gauge.

Why not kube-state-metrics `CustomResourceStateMetrics` as the primary: it
requires the operator to reconfigure *their* KSM deployment (extraArgs +
config + ClusterRole for 7 API groups) — the KubeSwift chart cannot deliver
it, so dashboards would depend on metrics that may not exist on a given
cluster. The in-controller collector is ~80–120 LOC, zero new deps, immune to
restart drift, and ships on the `/metrics` endpoint everyone already scrapes.
KSM CRS remains a genuinely good zero-code add-on for per-CR-name series —
ship a sample under `config/samples/monitoring/` and document it; depend on
nothing from it.

Collector series (all gauges, bounded labels, aggregate only — no per-CR-name):

```
kubeswift_guests{namespace, phase}                      # Pending|Scheduling|Running|Failed|Stopped
kubeswift_guests_by_node{node, hypervisor}
kubeswift_guests_by_boot_source{source}                 # image|kernel|clone
kubeswift_pool_replicas{namespace, pool, state}         # desired|ready|available|failed|updated
kubeswift_images{namespace, phase}
kubeswift_kernels{phase}
kubeswift_snapshots{namespace, phase}                   # catches stuck Capturing/Uploading
kubeswift_migrations_active{mode}                       # in-flight (non-terminal)
kubeswift_gpu_node_gpus{node, state}                    # total|free
kubeswift_gpu_node_info{node, model, vfio_ready}        # info-style =1
kubeswift_snapshot_schedule_last_success_timestamp_seconds{namespace, schedule}
```

Deprecation: keep `kubeswift_guest_running_total` for one release, emitted by
the collector with correct semantics (= Running count per namespace), note in
CHANGELOG, remove after. Same PR fixes `vm_failures_total` reason→`Reason`.

### D2 — swiftletd metrics endpoint: NO for v1

Per-VM host-side usage is already correct via cAdvisor (CH lives in the
launcher container's cgroup). A swiftletd `/metrics` endpoint means: the first
HTTP listener in swiftletd (new attack surface), a Rust prometheus dep, a
PodMonitor across all workload namespaces, scrape churn for short-lived pods —
for marginal value (CH `vm.counters` guest-internal queue counters). Defer per
Principles #1/#7. Door kept open: `swift-ch-client` would gain a `vm.counters`
method mirroring `vm.info`; decide in a v2 spike only if operators need
guest-internal counters cAdvisor can't show. The cAdvisor join recipe (label
allowlist; never pod-name joins) is documented in the runbook now (Phase O4).

### D3 — Dashboard taxonomy: six dashboards

1. **Fleet Overview** (new; the front door): guests by phase/node/hypervisor,
   pool desired-vs-ready, boot p95, failure rate, active migrations/snapshots,
   GPU free/total headline, controller up/leader.
2. **VM Lifecycle & Images** (new): boot quantiles, failure reasons, images by
   phase + import latency + stuck imports, kernels, top-N launcher pods by
   CPU/mem/net (cAdvisor join; prerequisite documented on the dashboard).
3. **GPU** (new): per-node total/free, utilization, vfioReady, FM partitions,
   allocation/release counters, discovery staleness.
4. **Snapshots & Restore** (exists; extend): + schedule panels, + by-phase
   stuck panel.
5. **Migration** (exists; extend): + failed-by-reason, + drain activity
   (eviction-webhook 429 rate), + active migrations.
6. **Control Plane** (new): reconcile rate/errors/duration per controller,
   workqueue depth/latency, webhook rate/latency/rejections, rest_client
   errors, leader election, manager process CPU/mem.

All tagged `kubeswift`, stable `uid`s, provisioning-native (D6).

### D4 — Alerting: ship a starter PrometheusRule (Helm-gated, warning-biased)

~12 rules, each with a `runbook_url` into docs: ControllerDown (critical),
ReconcileErrorsHigh, WebhookRejectionSpike, WebhookLatencyHigh, GuestsFailed,
GuestStuckPending, GuestCrashLooping, ImageImportStuck, PoolDegraded,
MigrationFailures, SnapshotFailures, SnapshotScheduleStale,
GPUNodeDiscoveryStale. Certificate expiry is deliberately NOT duplicated —
cert-manager owns `certmanager_certificate_expiration_timestamp_seconds`;
the runbook points there.

### D5 — Packaging: Helm `monitoring.*` gate; `config/grafana/` stays canonical

```yaml
monitoring:
  enabled: false                  # master switch (needs monitoring.coreos.com CRDs + Grafana sidecar)
  serviceMonitor:
    enabled: true
    interval: 30s
    additionalLabels: {}          # e.g. release: kube-prometheus-stack
  dashboards:
    enabled: true                 # ConfigMaps labeled grafana_dashboard: "1"
    label: grafana_dashboard
    annotations: {}               # e.g. grafana_folder
  prometheusRule:
    enabled: true
    additionalLabels: {}
```

- Dashboard ConfigMaps embed JSON via `.Files.Get`; a Makefile copy step from
  `config/grafana/` makes drift impossible (the CRD-sync lesson applied to
  dashboards).
- Default **off**, consistent with `webhook.enabled` / `migration.mtls.enabled`.
- TFU-16 lesson (opt-in must be discoverable): document
  `--set monitoring.enabled=true` in `make help` / add a
  `make deploy-with-monitoring` target.
- `config/grafana/servicemonitor.yaml` stays for non-Helm installs.

### D6 — Dashboard JSON hygiene: provisioning-native, CI-enforced

Rules (encoded as `tools/lint-dashboards.sh` + CI job — the PR #22 lesson:
a convention without CI rots):

1. No `__inputs` / `__requires` blocks.
2. No `${DS_*}` import-time variables — reference the datasource directly by
   a stable uid or omit it (Grafana default).
3. Stable `uid` per dashboard, `editable: true`, tags `["kubeswift"]`,
   pinned `schemaVersion`.

The two existing dashboards are converted in the same PR that adds the lint.

---

## 4. Phased plan

Cadence: small PRs, each cluster-validated on the lab kube-prometheus-stack.

### Phase O1 — Hygiene + packaging rails (2 PRs)

- **PR 1 — dashboard provisioning fix + lint**: convert both dashboards per
  D6; add `tools/lint-dashboards.sh` + CI wiring.
  *Validation:* mount as `grafana_dashboard`-labeled ConfigMaps; panels render
  with data, no datasource errors.
- **PR 2 — Helm `monitoring.*` gate**: ServiceMonitor + dashboard ConfigMaps
  (`.Files.Get` + Makefile copy) + values per D5.
  *Validation:* `helm upgrade --set monitoring.enabled=true` → target up,
  dashboards auto-appear; `=false` leaves zero monitoring.coreos.com resources.

### Phase O2 — CR-state collector + metric defect fixes (1 PR)

- **PR 3 — `internal/metrics/state_collector.go`**: cache-backed
  `prometheus.Collector` with the D1 series; fix `vm_failures_total`
  reason→Reason; alias-then-deprecate `guest_running_total`.
  *Validation (headline repro):* guests Running → `kubectl rollout restart`
  the controller → gauges re-converge to true counts (current code provably
  reports 0 after restart). Scale a pool; stop a guest; watch phases move.

### Phase O3 — Gap-filling feature metrics (2 PRs)

- **PR 4 — GPU + drain counters**: `kubeswift_gpu_allocations_total{result}` /
  `kubeswift_gpu_releases_total` in swiftgpu allocate/deallocate;
  `kubeswift_drain_migrations_total{policy,result}` in swiftdrain.
  *Validation:* GTX 1080 alloc/release cycle on boba; drain walkthrough rerun.
- **PR 5 — image/schedule/migration completeness**:
  `kubeswift_image_import_total{result}` (failures are invisible today);
  schedule last-success gauge (if not already in PR 3); `reason` label on
  failed migrations (bounded enum — remember the Phase 3c CRD-enum/metric
  sync lesson).
  *Validation:* bad-URL SwiftImage increments the failure counter; suspended
  schedule's staleness grows.

### Phase O4 — Dashboards + alerts (3 PRs)

- **PR 6 — Fleet Overview + Control Plane dashboards.**
- **PR 7 — VM Lifecycle & Images + GPU dashboards; extend Snapshots
  (+schedule) and Migration (+drain/reason).**
- **PR 8 — PrometheusRule starter set (D4) + `docs/observability/` runbook**
  (alert table, KSM CRS sample, cAdvisor recipe, scrape architecture).
- *Validation:* operator mini-walkthrough — induce a failed import, a failed
  migration, a degraded pool, controller down; confirm each alert fires and
  each dashboard answers its question without edits.

### Phase O5 — deferred (v2): per-VM guest telemetry spike

swiftletd `/metrics` + CH `vm.counters` + PodMonitor model. Spike-first;
go/no-go on whether operators need guest-internal counters cAdvisor can't
provide. Not scheduled.

---

## 5. Findings summary

1. 18 custom metrics exist, all controller-side, skewed to snapshots (8) and
   migration (3); **zero** for GPU, pools, kernels, drain, vhost-user; image
   imports track success-latency only.
2. Two latent defects: `guest_running_total` silently drifts to 0/negative
   across controller restarts, and `vm_failures_total` uses free-text Messages
   as a label (unbounded cardinality). Both fixed structurally in O2.
3. A large free surface (controller-runtime reconcile/workqueue/webhook
   metrics) already answers control-plane and webhook-rejection questions —
   needs only dashboards, not code.
4. Rust side and gpu-discovery export nothing — and shouldn't for v1: cAdvisor
   on launcher pods + CR-state gauges cover per-VM usage (join on the
   `swift.kubeswift.io/guest` label, never pod name).
5. Both shipped dashboards were import-style and broke under provisioning —
   fix + CI lint is Phase O1.
6. Architecture: in-controller cache-backed state collector (primary), KSM CRS
   as optional add-on, 6-dashboard taxonomy, Helm-gated `monitoring.*`
   packaging with a discoverable opt-in, ~12-rule warning-biased starter
   alert set.
