# GPU Discovery DaemonSet Validation Report

**Date:** 2026-04-03
**Status:** PENDING — awaiting on-cluster deployment and validation

## Cluster Info

- **Cluster version:** k0s v1.34.3
- **Nodes:** vr1 (control-plane), vr2, vr3, grogu
- **OS:** Ubuntu 24.04.3 LTS
- **Container runtime:** containerd 1.7.30
- **GPU hardware:** None (validation covers host topology discovery without GPUs)

## Prerequisites

The gpu-discovery image must be built and pushed by GitHub CI before validation.
Deploy with:

```bash
export KUBECONFIG=/home/wrkode/code/vmm-kubeswift/dev-tests/kubeswift/kubeswift-cluster.yaml

# Ensure CRDs are up to date
kubectl apply -k config/crd

# Apply RBAC and DaemonSet
kubectl apply -f config/rbac/gpu-discovery-rbac.yaml
kubectl apply -f config/daemonset/gpu-discovery.yaml

# Label a node for discovery
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
kubectl label node "$NODE" kubeswift.io/gpu-node=true --overwrite

# Wait for pod to start
kubectl get pods -n kubeswift-system -l app.kubernetes.io/component=gpu-discovery -w
```

## Validation Checks

### 1a. DaemonSet Deployment

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| Pod scheduled on labeled node | Running, 0 restarts | | PENDING |
| Pod image pulled | gpu-discovery:latest | | PENDING |

### 1b. SwiftGPUNode Creation

Wait 60-90s for first discovery cycle.

| Field | Expected | Actual | Status |
|-------|----------|--------|--------|
| `status.phase` | Ready | | PENDING |
| `status.host.cpuTopology.sockets` | >0 | | PENDING |
| `status.host.cpuTopology.coresPerSocket` | >0 | | PENDING |
| `status.host.cpuTopology.threadsPerCore` | >0 | | PENDING |
| `status.host.cpuTopology.totalCPUs` | >0 | | PENDING |
| `status.host.numaNodes` | >=1 entry | | PENDING |
| `status.host.iommuEnabled` | true or false | | PENDING |
| `status.gpuCount` | 0 (no GPUs) | | PENDING |
| `status.host.hugepages1Gi` | present (may be 0) | | PENDING |
| `kubectl get sgn` | Table with columns | | PENDING |

### 1c. DaemonSet Health

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| Pod not crash-looping | Running, 0 restarts | | PENDING |
| Logs show successful discovery | No errors | | PENDING |
| Security context hardened | privileged=false, drop ALL | | PENDING |

### 1d. Re-discovery (Idempotency)

Wait for second cycle (60s).

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| `status.lastDiscovery` updated | Newer timestamp | | PENDING |
| No errors on second pass | Clean logs | | PENDING |

### 1e. Label Removal

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| Pod terminates after label removal | Terminating/gone | | PENDING |
| SwiftGPUNode resource persists | Still exists | | PENDING |

### 1f. Cleanup

```bash
kubectl delete swiftgpunode "$NODE" 2>/dev/null || true
kubectl delete -f config/daemonset/gpu-discovery.yaml 2>/dev/null || true
kubectl delete -f config/rbac/gpu-discovery-rbac.yaml 2>/dev/null || true
```

## Pod Logs (excerpt)

```
<paste relevant log lines here after running validation>
```

## Issues Found

None yet — awaiting validation run.

## Conclusion

**Discovery validated:** PENDING

The Discovery DaemonSet was built and the validation framework is ready.
Actual on-cluster validation requires deploying the gpu-discovery image
(built by GitHub CI) and running the checks above.
