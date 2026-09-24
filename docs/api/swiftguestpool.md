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
| `updateStrategy.rollingUpdate.maxUnavailable` | No | Max replicas not serving during a rolling update (integer). Default `1`. |
| `updateStrategy.rollingUpdate.maxSurge` | No | Max extra replicas above the desired count during a rolling update (integer). Default `0`. `maxUnavailable` and `maxSurge` cannot both be `0`. |
| `spreadPolicy` | No | `Spread` (prefer distinct nodes) or `Pack` (default, no spread preference). |
| `topologySpreadConstraints` | No | List of Kubernetes topology spread constraints applied to each replica's launcher pod. Overrides `spreadPolicy` when set. |
| `volumeClaimTemplates` | No | List of PVC templates. One PVC per template per replica, named `<template-name>-<pool-name>-<index>`. |

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

If a replica is deleted (manually or during rollout), the controller recreates it with the same index to maintain stable identity. With `maxSurge > 0`, a rolling update first brings up temporary surge replicas at indices `replicas` .. `replicas + maxSurge - 1`, and removes them once the rollout is done and every replica is serving. A surge replica with `volumeClaimTemplates` gets its own per-index PVCs, which are retained like those of a scaled-down replica.

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
3. Outdated replicas that are not serving (not `Running` with `GuestRunning=True`) are replaced first. They add no unavailability, and replacing them lets a rollback of a rollout that never became ready go through.
4. With `maxSurge > 0`, up to `maxSurge` surge replicas are created from the updated template above the desired count.
5. Serving outdated replicas are deleted only while at least `replicas - maxUnavailable` replicas, surge replicas included, stay serving. A replica that was just created or is still booting or terminating counts as unavailable. The deleted index is recreated from the updated template once the old replica is gone.
6. Surge replicas are removed once no outdated replica remains and every replica is serving.
7. The `Progressing` condition is set to `True` during rollout and `False` when complete.

When `updateStrategy.type=Recreate`:

1. All existing replicas are deleted simultaneously.
2. New replicas are created once all old replicas are gone.

## Topology spread

The `spreadPolicy` field provides a simple toggle:

- `Pack` (default): no topology constraints; the scheduler places pods freely.
- `Spread`: the controller adds a `topologySpreadConstraint` with `topologyKey: kubernetes.io/hostname` and `maxSkew: 1` to each replica's launcher pod.

For advanced use cases, set `topologySpreadConstraints` directly. This overrides `spreadPolicy`.

## PVC per replica

The `volumeClaimTemplates` field creates a unique PVC for each replica. PVC names follow the pattern `<template-name>-<pool-name>-<index>`, and each PVC is labelled `swift.kubeswift.io/pool=<pool-name>`. PVCs are NOT deleted when a replica is deleted, when the pool is scaled down, or when the pool itself is deleted -- this preserves data across restarts and updates. The pool has no owner reference on them, so garbage collection leaves them alone.

A replica reuses an existing PVC of its name only if the PVC carries that pool label. Names alone can collide (template `data-web` in pool `x` and template `data` in pool `web-x` both give `data-web-x-0`). A PVC labelled for another pool is refused, and the replica is not created.

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
