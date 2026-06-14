# SwiftGuestPool

SwiftGuestPool manages a **fleet of identical SwiftGuest replicas**. It maintains a desired number of VM instances from a common template, handles rolling updates when the template changes, and supports topology spread for high availability. Think of it as a Deployment for VMs.

**API:** `swift.kubeswift.io/v1alpha1` · **Short name:** `sgpool` · **Subresources:** status, scale

## Operator workflow

1. Create prerequisite resources (SwiftGuestClass, SwiftImage or SwiftKernel, optional SwiftSeedProfile and SwiftGPUProfile).
2. Create a SwiftGuestPool with `replicas` and a `template` describing the desired SwiftGuest spec.
3. The controller creates `<pool-name>-0` through `<pool-name>-<N-1>` SwiftGuests.
4. Monitor rollout via `kubectl get sgpool` or conditions.
5. Scale with `kubectl scale sgpool <name> --replicas=N`.
6. Update the template to trigger a rolling update of all replicas.

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `replicas` | Yes | Desired number of SwiftGuest replicas. Minimum 0. |
| `template.metadata.labels` | No | Labels applied to each created SwiftGuest. |
| `template.metadata.annotations` | No | Annotations applied to each created SwiftGuest. |
| `template.spec` | Yes | SwiftGuestSpec used to create each replica. Supports all SwiftGuest fields (`imageRef`, `kernelRef`, `guestClassRef`, `seedProfileRef`, `gpuProfileRef`, `dataDiskRef`, `dataDiskRefs` (incl. blank disks), `runPolicy`, `interfaces`). |
| `updateStrategy.type` | No | `RollingUpdate` (default) or `Recreate`. |
| `updateStrategy.rollingUpdate.maxUnavailable` | No | Max replicas unavailable during rolling update. Integer or percentage. Default `1`. |
| `updateStrategy.rollingUpdate.maxSurge` | No | Max replicas above desired count during rolling update. Integer or percentage. Default `0`. |
| `spreadPolicy` | No | `Spread` (prefer distinct nodes) or `Pack` (default, no spread preference). |
| `topologySpreadConstraints` | No | List of Kubernetes topology spread constraints applied to each replica's launcher pod. Overrides `spreadPolicy` when set. |
| `volumeClaimTemplates` | No | List of PVC templates. One PVC per template per replica, named `<pool-name>-<template-name>-<index>`. |

## Status

| Field | Description |
|-------|-------------|
| `replicas` | Total number of SwiftGuest replicas owned by this pool. |
| `readyReplicas` | Number of replicas with `GuestRunning=True`. |
| `availableReplicas` | Number of replicas that have been ready for at least the minimum ready duration. |
| `failedReplicas` | Number of replicas in `Failed` phase. |
| `updatedReplicas` | Number of replicas matching the current template hash. |
| `currentTemplateHash` | Hash of the current `template.spec` used to detect drift. |
| `conditions` | `Available` (minimum replicas running), `Progressing` (rollout in progress), `Updated` (all replicas match current template). |

## Naming convention

Each replica is named `<pool-name>-<index>`, where index is zero-based:

```
inference-pool-0
inference-pool-1
inference-pool-2
```

If a replica is deleted (manually or during rollout), the controller recreates it with the same index to maintain stable identity. Indices are never reused across different generations -- a rolling update creates new replicas before deleting old ones when `maxSurge > 0`.

## Labels and annotations

The controller applies these labels to each SwiftGuest:

| Label | Value | Description |
|-------|-------|-------------|
| `swift.kubeswift.io/pool` | Pool name | Identifies pool membership. |
| `swift.kubeswift.io/pool-index` | `"0"`, `"1"`, ... | Replica index within the pool. |

The controller applies this annotation to each SwiftGuest:

| Annotation | Value | Description |
|------------|-------|-------------|
| `swift.kubeswift.io/template-hash` | Hash string | Tracks which template version the replica was created from. |

## Scale subresource

SwiftGuestPool implements the Kubernetes scale subresource, so standard scaling commands work:

```bash
kubectl scale sgpool inference-pool --replicas=8
kubectl autoscale sgpool inference-pool --min=2 --max=16  # with an HPA
```

## Rolling update behavior

When `updateStrategy.type=RollingUpdate` and the `template.spec` changes:

1. The controller computes a new `currentTemplateHash`.
2. Replicas whose `swift.kubeswift.io/template-hash` annotation differs from the current hash are considered outdated.
3. The controller deletes outdated replicas up to `maxUnavailable` at a time.
4. New replicas are created with the updated template (up to `maxSurge` above desired count).
5. The controller waits for new replicas to reach `GuestRunning=True` before continuing.
6. The `Progressing` condition is set to `True` during rollout and `False` when complete.

When `updateStrategy.type=Recreate`:

1. All existing replicas are deleted simultaneously.
2. New replicas are created once all old replicas are gone.

## Topology spread

The `spreadPolicy` field provides a simple toggle:

- `Pack` (default): no topology constraints; the scheduler places pods freely.
- `Spread`: the controller adds a `topologySpreadConstraint` with `topologyKey: kubernetes.io/hostname` and `maxSkew: 1` to each replica's launcher pod.

For advanced use cases, set `topologySpreadConstraints` directly. This overrides `spreadPolicy`.

## PVC per replica

The `volumeClaimTemplates` field creates a unique PVC for each replica. PVC names follow the pattern `<pool-name>-<template-name>-<index>`. PVCs are NOT deleted when a replica is deleted or the pool is scaled down -- this preserves data across restarts and updates.

To reference the PVC inside the guest template, use `dataDiskRef` or a seed profile that mounts the PVC.

## Printer columns

```
NAME              DESIRED   READY   UPDATED   AVAILABLE   FAILED   AGE
inference-pool    8         8       8         8           0        2h
ci-runners        4         3       4         3           0        15m
```

## Examples

### Basic pool

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: web-pool
  namespace: default
spec:
  replicas: 3
  template:
    metadata:
      labels:
        app: web
    spec:
      imageRef:
        name: ubuntu-noble
      guestClassRef:
        name: default
      seedProfileRef:
        name: minimal
      runPolicy: Running
```

### Rolling update with surge

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: inference-pool
  namespace: default
spec:
  replicas: 8
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 2
  template:
    spec:
      imageRef:
        name: ubuntu-noble-cuda
      gpuProfileRef:
        name: a100-pcie-single
      guestClassRef:
        name: gpu-large
      seedProfileRef:
        name: gpu-seed
      runPolicy: Running
```

### Spread across nodes

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: ha-pool
  namespace: default
spec:
  replicas: 6
  spreadPolicy: Spread
  template:
    spec:
      imageRef:
        name: ubuntu-noble
      guestClassRef:
        name: default
      seedProfileRef:
        name: minimal
      runPolicy: Running
```

### Stateful pool with PVCs

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: stateful-pool
  namespace: default
spec:
  replicas: 4
  volumeClaimTemplates:
  - metadata:
      name: home
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 50Gi
  template:
    spec:
      imageRef:
        name: ubuntu-noble
      dataDiskRef:
        name: home
      guestClassRef:
        name: default
      seedProfileRef:
        name: vdi-seed
      runPolicy: Running
```

### GPU inference fleet

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: gpu-fleet
  namespace: ml-inference
spec:
  replicas: 4
  spreadPolicy: Spread
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 0
  template:
    metadata:
      labels:
        workload: inference
    spec:
      imageRef:
        name: ubuntu-noble-cuda
      gpuProfileRef:
        name: a100-pcie-single
      guestClassRef:
        name: gpu-large
      seedProfileRef:
        name: inference-seed
      runPolicy: Running
```

## See also

[SwiftGuest](swiftguest.md) · [SwiftGuestClass](swiftguestclass.md) · [SwiftImage](swiftimage.md) · [SwiftGPUProfile](swiftgpuprofile.md) · [SwiftGuestPool Guide](../swiftguestpool-guide.md) · [SwiftGuestPool Use Cases](../swiftguestpool-use-cases.md)
