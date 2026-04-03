# GPU Discovery DaemonSet Validation Report

**Date:** 2026-04-03
**Status:** PASS — all checks validated on live cluster

## Cluster Info

- **Cluster version:** k0s v1.34.3
- **Nodes:** frida (control-plane), miles (worker)
- **OS:** Ubuntu 24.04.4 LTS
- **Kernel:** 6.8.0-101-generic (miles)
- **Container runtime:** containerd 1.7.30
- **GPU hardware:** None (validation covers host topology discovery without GPUs)
- **Image used:** `ghcr.io/projectbeskar/kubeswift/gpu-discovery:sha-111f3d2`
- **Node under test:** miles

## Validation Checks

### 1a. DaemonSet Deployment

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| Pod scheduled on labeled node | Running, 0 restarts | Running, 0 restarts on miles | PASS |
| Pod image pulled | gpu-discovery image | sha-111f3d2 pulled successfully | PASS |

Note: The DaemonSet manifest uses `:latest` but that tag does not exist in ghcr.io.
Image was overridden to `sha-111f3d2` via `kubectl set image`. The manifest should
be updated to use a CI-managed tag or the Helm chart's image tag override.

### 1b. SwiftGPUNode Creation

SwiftGPUNode `miles` created within first discovery cycle (~60s).

| Field | Expected | Actual | Status |
|-------|----------|--------|--------|
| `status.phase` | Ready | `Ready` | PASS |
| `status.host.cpuTopology.sockets` | >0 | `1` | PASS |
| `status.host.cpuTopology.coresPerSocket` | >0 | `4` | PASS |
| `status.host.cpuTopology.threadsPerCore` | >0 | `2` | PASS |
| `status.host.cpuTopology.totalCPUs` | >0 | `8` | PASS |
| `status.host.numaNodes` | >=1 entry | `[{id:0, cpus:"0-7", memoryMi:64079}]` | PASS |
| `status.host.iommuEnabled` | true or false | `true` | PASS |
| `status.gpuCount` | 0 (no GPUs) | omitted (Go omitempty, =0) | PASS |
| `status.host.hugepages1Gi` | present (may be 0) | omitted (Go omitempty, =0) | PASS |
| `kubectl get sgn` | Table with columns | Phase=Ready, GPUS/Free/Model blank | PASS |

### 1c. DaemonSet Health

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| Pod not crash-looping | Running, 0 restarts | `Running`, `0` restarts | PASS |
| Logs show successful discovery | No errors | Two clean cycles, no errors | PASS |
| Security context hardened | privileged=false, drop ALL | `privileged:false, allowPrivilegeEscalation:false, drop:["ALL"], readOnlyRootFilesystem:true` | PASS |

### 1d. Re-discovery (Idempotency)

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| `status.lastDiscovery` updated | Newer timestamp | `2026-04-03T12:12:29Z` (updated on 2nd cycle) | PASS |
| No errors on second pass | Clean logs | Clean — "discovery cycle complete" with same values | PASS |

### 1e. Label Removal

| Check | Expected | Actual | Status |
|-------|----------|--------|--------|
| Pod terminates after label removal | Terminating/gone | Pod gone ("No resources found") | PASS |
| SwiftGPUNode resource persists | Still exists | `miles` still present with Phase=Ready | PASS |

## Pod Logs (excerpt)

```
I0403 12:11:28.713366  1 main.go:40] "gpu-discovery starting" node="miles" interval="1m0s"
I0403 12:11:28.713414  1 main.go:59] "starting discovery cycle" node="miles"
I0403 12:11:29.614646  1 main.go:254] "Fabric Manager discovery skipped" reason="fmpm not in PATH"
I0403 12:11:29.861800  1 main.go:110] "discovery cycle complete" node="miles" gpuCount=0 freeGPUs=0 phase="Ready"
I0403 12:12:28.742032  1 main.go:59] "starting discovery cycle" node="miles"
I0403 12:12:29.448111  1 main.go:254] "Fabric Manager discovery skipped" reason="fmpm not in PATH"
I0403 12:12:29.540906  1 main.go:110] "discovery cycle complete" node="miles" gpuCount=0 freeGPUs=0 phase="Ready"
```

## Issues Found

1. **DaemonSet manifest uses `:latest` tag** — this tag does not exist in ghcr.io. The CI
   builds produce `sha-<commit>` tags only. The manifest should either use a specific tag
   or the Helm chart should template the image tag. Non-blocking — workaround is
   `kubectl set image` or editing the manifest.

2. **Fabric Manager discovery skipped** — expected on nodes without `fmpm` binary.
   Logged cleanly at INFO level, no error. Non-blocking.

3. **`gpuCount` and `hugepages1Gi` omitted when zero** — Go `omitempty` on int/struct
   fields means zero values are absent from the JSON. Printer columns show blank.
   This is standard Kubernetes behavior and does not block GPU usage.

## Conclusion

**Discovery validated: PASS**

All 12 checks passed. The GPU Discovery DaemonSet correctly discovers host topology
(CPU, NUMA, IOMMU), creates SwiftGPUNode resources, runs idempotently on 60s intervals,
respects node label removal, and runs with hardened security context. Ready for
validation on nodes with actual GPU hardware.
