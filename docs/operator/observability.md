# KubeSwift Observability

This guide covers metrics, Prometheus integration, and log collection for KubeSwift.

## Overview

- **Metrics:** KubeSwift exposes Prometheus metrics via the controller-manager at `:8080/metrics`
- **Scope:** Metrics include both controller-runtime built-ins and custom KubeSwift metrics
- **Logs:** controller-manager uses klog; swiftletd uses structured env_logger

## Metrics Endpoint

- **URL:** `http://<controller-manager-pod>:8080/metrics`
- **Service:** `kubeswift-controller-manager-metrics` (port 8080) in `kubeswift-system`

**Verify manually:**

```bash
kubectl port-forward -n kubeswift-system deployment/kubeswift-controller-manager 8080:8080
curl http://localhost:8080/metrics | grep kubeswift
```

## Custom KubeSwift Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `kubeswift_guest_running_total` | Gauge | `namespace` | Number of SwiftGuest instances currently in Running phase |
| `kubeswift_vm_boot_seconds` | Histogram | `namespace` | Time from pod creation to GuestRunning=True. Buckets: 5, 10, 20, 30, 60, 90, 120, 180s |
| `kubeswift_vm_failures_total` | Counter | `namespace`, `reason` | Total VM failures; reason matches condition message |
| `kubeswift_image_import_seconds` | Histogram | `namespace` | Time for SwiftImage import to reach Ready. Buckets: 30, 60, 120, 300, 600, 900s |

## Built-in controller-runtime Metrics

- `controller_runtime_reconcile_total` — reconcile counts by controller and result
- `controller_runtime_reconcile_errors_total` — reconcile error counts
- `controller_runtime_webhook_requests_total` — webhook request counts
- Standard Go runtime metrics (`go_goroutines`, `go_memstats`, etc.)

## Prometheus Integration

### Option 1: PodMonitor (recommended)

Requires [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator).

```bash
kubectl apply -f - <<EOF
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: kubeswift-controller-manager
  namespace: kubeswift-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kubeswift
      app.kubernetes.io/component: controller-manager
  podMetricsEndpoints:
    - port: metrics
      path: /metrics
      interval: 30s
EOF
```

### Option 2: ServiceMonitor

Requires Prometheus Operator.

```bash
kubectl apply -f - <<EOF
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: kubeswift-controller-manager
  namespace: kubeswift-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kubeswift
      app.kubernetes.io/component: controller-manager
  endpoints:
    - port: metrics
      path: /metrics
      interval: 30s
EOF
```

**Note:** ServiceMonitor selects Services. Ensure the `kubeswift-controller-manager-metrics` Service has labels matching the selector, or adjust the selector to match the Service labels.

### Option 3: Static scrape config (no Prometheus Operator)

Add to `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: kubeswift
    kubernetes_sd_configs:
      - role: endpoints
        namespaces:
          names: [kubeswift-system]
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_name]
        action: keep
        regex: kubeswift-controller-manager-metrics
```

## Useful PromQL Queries

| Query | Description |
|-------|-------------|
| `kubeswift_guest_running_total` | Running guests by namespace |
| `histogram_quantile(0.95, rate(kubeswift_vm_boot_seconds_bucket[5m]))` | VM boot time p95 |
| `rate(kubeswift_vm_failures_total[5m])` | VM failure rate |
| `histogram_quantile(0.95, rate(kubeswift_image_import_seconds_bucket[5m]))` | Image import time p95 |
| `rate(controller_runtime_reconcile_errors_total{controller="swiftguest"}[5m])` | Reconcile error rate |

## Log Collection

### controller-manager logs

Uses klog structured logging. Collect with standard kubectl or a log aggregator:

```bash
kubectl logs -n kubeswift-system deployment/kubeswift-controller-manager
kubectl logs -n kubeswift-system deployment/kubeswift-controller-manager --follow
```

### swiftletd logs (per guest)

Uses env_logger structured logging with timestamps. Access via swiftctl or kubectl:

```bash
swiftctl logs <guest>
swiftctl logs <guest> --follow
kubectl logs <pod> -c launcher
```

**Log level:** Controlled by `RUST_LOG` env var (default: `info`). Levels: `error`, `warn`, `info`, `debug`, `trace`.

To increase log level for a guest pod, add env var `RUST_LOG=debug` to the launcher container via the pod spec.

## Grafana Dashboard (suggested panels)

- **Stat:** `kubeswift_guest_running_total` (sum by namespace)
- **Histogram:** `kubeswift_vm_boot_seconds` quantiles (p50, p95, p99)
- **Time series:** `rate(kubeswift_vm_failures_total[5m])`
- **Time series:** `rate(controller_runtime_reconcile_total[5m])` by controller
- **Histogram:** `kubeswift_image_import_seconds` quantiles
