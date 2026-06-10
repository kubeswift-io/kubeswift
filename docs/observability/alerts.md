# KubeSwift Alerts

The chart ships a starter `PrometheusRule` (`monitoring.prometheusRule.enabled`,
default on when `monitoring.enabled`). 12 rules, warning-biased — they flag
conditions an operator should look at, not page-at-3am criticals (the one
exception is `KubeSwiftControllerDown`). Tune thresholds to your environment;
these are conservative defaults.

Each alert below lists what it means and the first diagnostic step.

## Control plane

### KubeSwiftControllerDown
**critical · `absent(up{job=~"kubeswift-controller-manager.*"} == 1)` for 5m**
No healthy controller-manager scrape target. VM reconciliation, status
reporting, and admission are all unavailable.
→ `kubectl -n kubeswift-system get pods -l app.kubernetes.io/component=controller-manager`;
check pod events, logs, and that the metrics Service still has endpoints.

### KubeSwiftReconcileErrorsHigh
**warning · per-controller reconcile error ratio > 10% for 15m**
A specific controller is failing most of its reconciles.
→ `kubectl -n kubeswift-system logs deploy/controller-manager | grep -i error`;
the `controller` label names the failing reconciler. Common causes: RBAC gap
on a newly-watched resource (the recurring "Failed to watch" pattern — grant
`list,watch`), a CRD/Go-type drift, or a dependent API being unavailable.

### KubeSwiftWebhookRejectionSpike
**warning · webhook non-2xx rate > 1/s for 15m**
Admission webhooks are rejecting at a high rate.
→ Look at the Control Plane dashboard's "Webhook request rate by code" panel to
see which webhook + code. A loop of `403`s is usually a bad manifest being
re-applied; a stream of `429`s on the eviction path is a stalling drain (see
the Migration dashboard's drain panel).

## Guests

### KubeSwiftGuestsFailed
**warning · `kubeswift_guests{phase="Failed"} > 0` for 10m**
→ `kubectl get swiftguest -A | grep -i failed`, then `kubectl describe` one —
the conditions carry the reason. The `kubeswift_vm_failures_total{reason}` series
gives the aggregate cause.

### KubeSwiftGuestStuckPending
**warning · `kubeswift_guests{phase=~"Pending|Scheduling"} > 0` for 15m**
A guest hasn't progressed past scheduling.
→ Check the referenced SwiftImage/SwiftKernel is `Ready`, the guest's node
selector/capacity, and `kubectl describe swiftguest` conditions.

### KubeSwiftGuestCrashLooping
**warning · `rate(kubeswift_vm_failures_total[15m]) > 0` for 15m**
Repeated failures in a namespace — likely one guest crash-looping.
→ `swiftctl logs <guest>` for the launcher/CH output; `swiftctl describe` for
phase history.

### KubeSwiftPoolDegraded
**warning · pool ready < desired for 15m**
A SwiftGuestPool can't reach its desired replica count.
→ `kubectl describe swiftguestpool <pool>`; usually capacity, a failing
template, or a stuck rollout. The Fleet dashboard's pool table shows the split.

## Images & migration

### KubeSwiftImageImportStuck
**warning · image non-terminal (Importing/Validating/Preparing/Snapshotting) for 30m**
→ `kubectl describe swiftimage <name>`; check the import Job
(`kubectl get jobs -n <ns>`) and its pod logs. Stalled download, hung
conversion, or a snapshot not reaching readyToUse are the usual causes.

### KubeSwiftMigrationFailures
**warning · `increase(kubeswift_migration_total{result="failed"}[1h]) > 0`**
→ The `kubeswift_migration_failures_total{reason}` series (Migration dashboard)
breaks failures down by the bounded `failureReason` enum
(DstNeverReady/PodTerminated/Timeout/…). `kubectl describe swiftmigration` for
the per-migration detail.

## Snapshots & GPU

### KubeSwiftSnapshotFailures
**warning · a snapshot or restore failed in the last hour**
→ `kubectl get swiftsnapshot,swiftrestore -A | grep -i failed`, then
`kubectl describe`. For Tier C (s3), check the upload/download Job logs.

### KubeSwiftSnapshotScheduleStale
**warning · `time() - last_success > 24h` for 30m**
A SwiftSnapshotSchedule hasn't produced a Ready snapshot in 24h. The 24h
threshold is a generic default — **tune it to your schedule period** (e.g. a
6-hourly schedule wants a tighter bound).
→ `kubectl describe swiftsnapshotschedule <name>`; check `suspend`, the cron
expression, and whether the most recent scheduled snapshot is stuck or failing.

### KubeSwiftGPUNodeDiscoveryStale
**warning · `time() - last_discovery > 10m` for 10m**
The gpu-discovery DaemonSet pod on a node has stopped refreshing inventory;
allocation decisions would use stale data.
→ `kubectl -n kubeswift-system get pods -l app.kubernetes.io/component=gpu-discovery -o wide`;
restart the pod on the affected node and check its logs.

## What is deliberately not alerted

- **Certificate expiry** — cert-manager owns the webhook/mTLS certs; use its
  `certmanager_certificate_expiration_timestamp_seconds` alerts rather than
  duplicating them here.
- **Migration in progress / long downtime** — surfaced on the dashboard, not
  alerted (a slow migration is not an error).
