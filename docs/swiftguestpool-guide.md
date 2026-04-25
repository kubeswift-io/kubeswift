# SwiftGuestPool Operational Guide

This guide covers day-to-day operation of SwiftGuestPool, from creating your first pool to advanced topics like rolling updates, topology spread, and GPU inference fleets.

For CRD field reference, see [api/swiftguestpool.md](api/swiftguestpool.md).
For complete use-case manifests, see [swiftguestpool-use-cases.md](swiftguestpool-use-cases.md).

> **Tip — fast pool scale-up.** When the pool's `template.spec.imageRef`
> targets a SwiftImage with `cloneStrategy: snapshot`, scale-up is
> dramatically faster on snapshot-capable CSI drivers because each new
> replica's root-disk PVC is provisioned via `dataSource: VolumeSnapshot`
> instead of a per-replica Copy Job. See
> [images/clone-strategies.md](images/clone-strategies.md). The default
> `copy` strategy keeps working on any CSI driver — no migration required.

---

## 1. Getting Started

### What is SwiftGuestPool

SwiftGuestPool manages a fleet of identical SwiftGuest VMs. You provide a replica count and a template; the controller creates and maintains that many VMs. When you change the template, the controller performs a rolling update. When a VM fails, the controller replaces it.

### Prerequisites

Before creating a pool you need the same prerequisites as a standalone SwiftGuest:

- SwiftGuestClass (cluster-scoped) defining CPU, memory, root disk
- SwiftImage (`phase=Ready`) or SwiftKernel (`phase=Ready`)
- Optional: SwiftSeedProfile, SwiftGPUProfile
- RBAC applied in the target namespace (`kubectl apply -k config/rbac -n <namespace>`)

### Create your first pool

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: demo-pool
  namespace: default
spec:
  replicas: 3
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

```bash
kubectl apply -f pool.yaml
```

### Watch it come up

```bash
kubectl get sgpool demo-pool -w
kubectl get sg -l swift.kubeswift.io/pool=demo-pool
```

The controller creates `demo-pool-0`, `demo-pool-1`, and `demo-pool-2`. Each follows the normal SwiftGuest lifecycle (Pending -> Scheduling -> Running).

---

## 2. Scaling

### Scale up

```bash
kubectl scale sgpool demo-pool --replicas=5
```

New replicas `demo-pool-3` and `demo-pool-4` are created immediately. They use the current template.

### Scale down

```bash
kubectl scale sgpool demo-pool --replicas=2
```

The controller deletes the highest-index replicas first (`demo-pool-4`, then `demo-pool-3`, then `demo-pool-2`). VMs are stopped gracefully (SIGTERM, then 30s timeout).

### Scale to zero

```bash
kubectl scale sgpool demo-pool --replicas=0
```

All replicas are deleted. The pool itself remains; scale back up at any time. PVCs from `volumeClaimTemplates` are preserved.

### Capacity planning

Each replica creates one launcher pod. Resource consumption per replica:

- CPU and memory from SwiftGuestClass (host overhead is minimal beyond the guest allocation)
- One root disk PVC (from SwiftImage)
- One `/dev/kvm` device
- Optional: GPU devices, hugepages, data disk PVCs

Check node capacity before scaling:

```bash
kubectl describe nodes | grep -A5 "Allocated resources"
```

For GPU pools, check free GPUs:

```bash
kubectl get sgn -o custom-columns=NAME:.metadata.name,FREE:.status.freeGPUs
```

---

## 3. Rolling Updates

### What triggers a rolling update

Any change to `template.spec` triggers a rolling update. Changes to `template.metadata` (labels, annotations) do NOT trigger a rollout -- they are applied in-place.

### Monitor a rollout

```bash
kubectl get sgpool demo-pool -w
```

Watch the `UPDATED` column converge to `DESIRED`. The `Progressing` condition provides detail:

```bash
kubectl get sgpool demo-pool -o jsonpath='{.status.conditions}'
```

### maxUnavailable and maxSurge

These control rollout speed and capacity:

```yaml
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 2    # delete up to 2 old replicas at once
      maxSurge: 1          # create up to 1 extra replica during rollout
```

- `maxUnavailable: 1, maxSurge: 0` (default) -- safest; one replica down at a time, no extra capacity needed
- `maxUnavailable: 0, maxSurge: 1` -- zero-downtime; always at full capacity, needs headroom for one extra VM
- `maxUnavailable: "25%", maxSurge: "25%"` -- fast rollout for large fleets

### Recreate strategy

```yaml
spec:
  updateStrategy:
    type: Recreate
```

All replicas are deleted, then all are recreated. Use this when the workload cannot tolerate mixed versions (e.g. a clustered database that requires all nodes on the same image).

### Rollback

To rollback, revert `template.spec` to the previous version. The controller treats this as a new rolling update and converges all replicas to the reverted template.

```bash
# Edit the pool to revert the image
kubectl edit sgpool demo-pool
# Or apply the previous manifest
kubectl apply -f pool-v1.yaml
```

---

## 4. High Availability

### Spread policy

Set `spreadPolicy: Spread` to distribute replicas across nodes:

```yaml
spec:
  replicas: 6
  spreadPolicy: Spread
  template:
    spec:
      imageRef:
        name: ubuntu-noble
      guestClassRef:
        name: default
```

This adds a `topologySpreadConstraint` with `topologyKey: kubernetes.io/hostname` and `maxSkew: 1` to each launcher pod. The scheduler distributes pods as evenly as possible across nodes.

### Custom topology spread

For zone-aware spread or multi-key constraints, use `topologySpreadConstraints` directly:

```yaml
spec:
  replicas: 6
  topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        swift.kubeswift.io/pool: ha-pool
  - maxSkew: 2
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        swift.kubeswift.io/pool: ha-pool
```

When `topologySpreadConstraints` is set, `spreadPolicy` is ignored.

### Node failure handling

If a node goes down, the launcher pods on that node enter `Terminating` state. Once Kubernetes marks the pods as terminated (controlled by the node's `pod-eviction-timeout`, default 5 minutes), the SwiftGuestPool controller detects the missing replicas and creates replacements on healthy nodes.

---

## 5. Persistent Data

### volumeClaimTemplates

To give each replica its own persistent volume:

```yaml
spec:
  replicas: 4
  volumeClaimTemplates:
  - metadata:
      name: home
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: longhorn
      resources:
        requests:
          storage: 100Gi
  template:
    spec:
      imageRef:
        name: ubuntu-noble
      dataDiskRef:
        name: home
      guestClassRef:
        name: default
```

This creates PVCs named `stateful-pool-home-0`, `stateful-pool-home-1`, etc.

### PVC lifecycle

- PVCs are created when a replica is created.
- PVCs are NOT deleted when a replica is deleted (scale-down, rollout, or pool deletion).
- To reclaim storage, delete the PVCs manually: `kubectl delete pvc -l swift.kubeswift.io/pool=<pool-name>`.
- When scaling back up, existing PVCs are reattached to replicas with matching indices.

### Multiple volume claim templates

```yaml
spec:
  volumeClaimTemplates:
  - metadata:
      name: home
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 50Gi
  - metadata:
      name: scratch
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 200Gi
```

Each replica gets two PVCs: `<pool>-home-<index>` and `<pool>-scratch-<index>`.

---

## 6. GPU Inference Fleet

A complete example: 4 VMs each with one A100-PCIe GPU running an inference workload.

### Prerequisites

```bash
# Label GPU nodes
kubectl label node gpu-node-1 kubeswift.io/gpu-node=true
kubectl label node gpu-node-2 kubeswift.io/gpu-node=true

# Deploy GPU discovery
helm upgrade --install kubeswift charts/kubeswift/ --set gpuDiscovery.enabled=true

# Wait for discovery
kubectl get sgn
```

### Supporting resources

```yaml
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: gpu-large
spec:
  cpu: "16"
  memory: "32Gi"
  rootDisk:
    size: "80Gi"
    format: raw
---
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: a100-pcie-single
  namespace: ml-inference
spec:
  count: 1
  model: "A100-PCIe"
  tier: pcie
  partitionMode: isolated
  pcieTopology:
    gpuDirectClique: 0
  hugepages: "1Gi"
```

### Pool manifest

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: inference-fleet
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
        model: resnet50
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

### Verify

```bash
kubectl get sgpool inference-fleet -n ml-inference
kubectl get sg -n ml-inference -l workload=inference
# Check GPUs inside a guest
swiftctl ssh inference-fleet-0 -n ml-inference -- nvidia-smi
```

---

## 7. CI/CD Runner Pool

Ephemeral VMs for CI/CD pipelines. Recreate strategy ensures all runners update simultaneously.

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestPool
metadata:
  name: ci-runners
  namespace: ci
spec:
  replicas: 8
  updateStrategy:
    type: Recreate
  template:
    metadata:
      labels:
        role: ci-runner
    spec:
      imageRef:
        name: ubuntu-noble-ci
      guestClassRef:
        name: default
      seedProfileRef:
        name: ci-runner-seed
      runPolicy: Running
```

Scale up during peak hours, scale down at night:

```bash
# Peak hours
kubectl scale sgpool ci-runners -n ci --replicas=16
# Off hours
kubectl scale sgpool ci-runners -n ci --replicas=2
```

---

## 8. Monitoring

### kubectl commands

```bash
# Pool overview
kubectl get sgpool

# Detailed status
kubectl describe sgpool <name>

# List replicas with status
kubectl get sg -l swift.kubeswift.io/pool=<name> \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,IP:.status.network.primaryIP,NODE:.status.nodeName

# Watch rollout
kubectl get sgpool <name> -w
```

### Conditions

| Condition | Meaning |
|-----------|---------|
| `Available` | At least `replicas - maxUnavailable` replicas are running. |
| `Progressing` | A rollout is in progress (updated replicas < desired replicas). |
| `Updated` | All replicas match the current template hash. |

Check conditions:

```bash
kubectl get sgpool <name> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}'
```

### Prometheus metrics

The controller exposes these metrics (when observability is enabled):

- `kubeswift_pool_replicas_desired` -- desired replica count
- `kubeswift_pool_replicas_ready` -- ready replica count
- `kubeswift_pool_replicas_failed` -- failed replica count
- `kubeswift_pool_rollout_in_progress` -- 1 if a rollout is active, 0 otherwise

---

## 9. Troubleshooting

### Stuck rollout

Symptom: `Progressing=True` for a long time, `UPDATED` column not converging.

```bash
# Check which replicas are outdated
kubectl get sg -l swift.kubeswift.io/pool=<name> \
  -o custom-columns=NAME:.metadata.name,HASH:.metadata.annotations.swift\.kubeswift\.io/template-hash,PHASE:.status.phase

# Check the stuck replica
kubectl describe sg <replica-name>
swiftctl logs <replica-name>
```

Common causes:
- The new template references an image that is not Ready.
- The guest class does not have enough resources for the new spec.
- Node capacity exhausted -- no room for new replicas.

### PVC binding failures

Symptom: replicas stuck in Pending, events show `FailedBinding`.

```bash
kubectl get pvc -l swift.kubeswift.io/pool=<name>
kubectl describe pvc <pool>-<template>-<index>
```

Check that the StorageClass exists and has available capacity. For local-path provisioners, ensure the target node has disk space.

### GPU exhaustion

Symptom: replicas stuck waiting for `GPUAllocated=True`.

```bash
kubectl get sgn -o custom-columns=NAME:.metadata.name,FREE:.status.freeGPUs
kubectl get sg -l swift.kubeswift.io/pool=<name> -o jsonpath='{range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="GPUAllocated")].status}{"\n"}{end}'
```

Scale down the pool to match available GPUs, or add more GPU nodes.

### Topology spread violations

Symptom: replicas stuck in Pending, events show `TopologySpreadConstraint`.

```bash
kubectl describe pod <replica-launcher-pod>
```

When using `Spread` or `topologySpreadConstraints`, the scheduler may be unable to place pods if there are fewer nodes than replicas. Either add nodes, reduce replicas, or switch to `ScheduleAnyway`.

---

## 10. Best Practices

1. **One pool per workload type.** Do not mix inference and training VMs in the same pool. Use separate pools with distinct SwiftGPUProfiles.

2. **Use conservative maxUnavailable.** Start with `maxUnavailable: 1` and increase only when rollout speed matters more than availability.

3. **Always set spreadPolicy for production pools.** `Spread` protects against single-node failures at no cost.

4. **Test template changes on a single SwiftGuest first.** Before updating a pool's template, create a standalone SwiftGuest with the new spec and verify it boots and runs correctly.

5. **Label your pools.** Use `template.metadata.labels` to tag replicas with workload type, team, environment. This enables targeted monitoring and RBAC.

6. **Use volumeClaimTemplates for stateful workloads.** Do not rely on ephemeral root disks for data that must survive updates. PVCs persist across rollouts and scale events.

7. **Monitor failed replicas.** A non-zero `failedReplicas` count indicates a systemic problem (bad image, resource exhaustion, node issue). Investigate promptly.

8. **Clean up PVCs after pool deletion.** PVCs from `volumeClaimTemplates` are not garbage-collected. Delete them manually when the pool is permanently removed.

---

## See also

- [SwiftGuestPool API Reference](api/swiftguestpool.md)
- [SwiftGuestPool Use Cases](swiftguestpool-use-cases.md)
- [SwiftGuest](api/swiftguest.md)
- [GPU Passthrough Guide](gpu-passthrough.md)
