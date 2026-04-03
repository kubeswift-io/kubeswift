# SwiftGuest

A SwiftGuest is **one VM instance**. It boots via one of two paths: disk boot (using SwiftImage) or kernel boot (using SwiftKernel). The `imageRef` and `kernelRef` fields are mutually exclusive. GPU passthrough is available via `gpuProfileRef` (combinable with `imageRef` but not `kernelRef`).

**API:** `swift.kubeswift.io/v1alpha1` · **Short name:** `sg`

## Operator workflow

### Disk boot

1. Ensure SwiftImage exists and is `phase=Ready` (import can take 5–15 min).
2. Apply `config/rbac/` in the namespace (required for swiftletd status patching).
3. Create SwiftGuest with `imageRef`; controller creates pod; swiftletd launches Cloud Hypervisor.

### Kernel boot

1. Label nodes with `kubeswift.io/kernel-node=true`.
2. Ensure SwiftKernel exists and is `phase=Ready`.
3. Apply `config/rbac/` in the namespace.
4. Create SwiftGuest with `kernelRef`; controller creates pod with hostPath volume and nodeSelector.

### GPU boot

1. Label GPU nodes with `kubeswift.io/gpu-node=true` and deploy Discovery DaemonSet.
2. Create SwiftGPUProfile describing GPU requirements.
3. Create SwiftGuest with `imageRef` + `gpuProfileRef`.
4. SwiftGPU controller allocates GPUs (sets `GPUAllocated=True`), then SwiftGuest controller creates pod with VFIO devices.

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `imageRef.name` | Yes* | SwiftImage name (same namespace) — disk boot |
| `kernelRef.name` | Yes* | SwiftKernel name (same namespace) — kernel boot |
| `kernelCmdline` | No | Kernel command line override (kernel boot only); overrides SwiftKernel default |
| `guestClassRef.name` | Yes | SwiftGuestClass name (cluster-scoped) |
| `seedProfileRef.name` | No | SwiftSeedProfile for cloud-init (disk boot only) |
| `gpuProfileRef.name` | No | SwiftGPUProfile for GPU passthrough. Valid with `imageRef` only (not `kernelRef`). |
| `dataDiskRef.name` | No | SwiftImage to attach as secondary data disk (`/dev/vdb`). Works with all boot paths. |
| `runPolicy` | No | `Running` (default), `Stopped`, `RestartOnFailure`, `Always` |

*Exactly one of `imageRef` or `kernelRef` must be set. `gpuProfileRef` can combine with `imageRef` but not `kernelRef`. `dataDiskRef` is independent and works with any boot path.

### Disk boot example

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: my-guest
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  runPolicy: Running
```

### Kernel boot example

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: faas-test
  namespace: default
spec:
  kernelRef:
    name: faas-minimal
  guestClassRef:
    name: default
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"
  runPolicy: Running
```

### GPU boot example

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: gpu-test
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble-qemu
  gpuProfileRef:
    name: a100-pcie-single
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  runPolicy: Running
```

### Data disk example

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: datadisk-test
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble
  dataDiskRef:
    name: data-disk
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  runPolicy: Running
```

## Run Policies

| Policy | Behavior |
|--------|----------|
| `Running` | Start VM and keep running. Default. |
| `Stopped` | Do not start VM. If running, stop it. |
| `RestartOnFailure` | Restart VM if it exits with a non-zero exit code. Uses exponential backoff: 10s → 20s → 40s → 80s → 160s → max 300s. |
| `Always` | Restart VM on any exit (success or failure). Same backoff as RestartOnFailure. |

## Status

| Field | Description |
|-------|-------------|
| `phase` | Pending, Scheduling, Running, Stopped, Failed |
| `conditions` | Resolved, PodScheduled, GuestRunning, GPUAllocated |
| `nodeName` | Node where the guest pod runs |
| `podRef` | Reference to the guest pod |
| `runtime.pid` | Hypervisor process PID |
| `runtime.hypervisor` | `cloud-hypervisor` or `qemu` |
| `console.serialSocket` | Path to serial socket for console access |
| `network.primaryIP` | Guest IP discovered from DHCP lease (disk boot only) |
| `network.interfaces` | List of {name, ip} for all guest interfaces |
| `gpu.devices` | List of allocated GPU PCI addresses (when `gpuProfileRef` set) |
| `gpu.partitionId` | Fabric Manager partition ID (-1 = none) |
| `gpu.numaNodes` | NUMA node IDs the GPUs are attached to |
| `gpu.hypervisor` | Resolved hypervisor for GPU path |
| `gpu.nodeName` | Node where GPUs were allocated |
| `restartCount` | Number of times the guest has been restarted |
| `lastRestartTime` | Timestamp of last restart |

**Phase meanings:** `Pending` = resolution failed or unschedulable; `Scheduling` = pod pending; `Running` = VM up; `Stopped` = VM stopped; `Failed` = resolution, pod, or VM error.

## Example

```bash
kubectl apply -f config/samples/disk-boot/swiftguest-sample.yaml
kubectl get swiftguest sample -w
```

## Prerequisites

- For disk boot: SwiftImage `phase=Ready` before creating SwiftGuest
- For kernel boot: SwiftKernel `phase=Ready` and nodes labeled `kubeswift.io/kernel-node=true`
- `kubectl apply -k config/rbac -n <namespace>`
- Worker nodes with KVM; run [preflight](../operator/worker-node-preflight.md)

[SwiftGuestClass](swiftguestclass.md) · [SwiftImage](swiftimage.md) · [SwiftSeedProfile](swiftseedprofile.md) · [SwiftKernel](swiftkernel.md) · [SwiftGPUProfile](swiftgpuprofile.md) · [SwiftGPUNode](swiftgpunode.md) · [Lifecycle](../architecture/lifecycle.md)
