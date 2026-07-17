# SwiftGPUNode

SwiftGPUNode represents the **GPU inventory on a single Kubernetes node**. It is auto-populated by the GPU Discovery DaemonSet — operators do not create it manually.

**API:** `gpu.kubeswift.io/v1alpha1` · **Short name:** `sgn` · **Scope:** Cluster

## Operator workflow

1. Label a node: `kubectl label node <name> kubeswift.io/gpu-node=true`
2. Deploy the GPU Discovery DaemonSet (config/daemonset/gpu-discovery.yaml + config/rbac/gpu-discovery-rbac.yaml).
3. Wait ~60s for the first discovery cycle.
4. Inspect: `kubectl get sgn` shows per-node GPU inventory.
5. The SwiftGPU controller reads SwiftGPUNode to allocate GPUs for SwiftGuests.

## Status

The entire resource is status-only (no user-editable spec).

### Top-level fields

| Field | Description |
|-------|-------------|
| `phase` | `Discovering`, `Ready`, or `Error` |
| `lastDiscovery` | Timestamp of last successful discovery cycle |
| `gpuCount` | Total number of GPUs on this node |
| `freeGPUs` | Number of unallocated GPUs |
| `gpuModel` | GPU model (assumes homogeneous node) |
| `gpuVendor` | GPU vendor (assumes homogeneous node): `"NVIDIA"`, `"AMD"`, `"Intel"` |
| `vfioReady` | `true` when the `vfio-pci` driver is loaded on this node (`/sys/bus/pci/drivers/vfio-pci` exists). GPU allocation and the migration GPU target pre-flight refuse a node that isn't `vfioReady`. Loading `vfio-pci` is a host responsibility (e.g. `/etc/modules-load.d`) — the minimal-capability discovery DaemonSet only detects and reports it, it cannot `modprobe`. Printcolumn `VfioReady`. |

### Host topology

| Field | Description |
|-------|-------------|
| `host.cpuTopology.sockets` | Physical CPU sockets |
| `host.cpuTopology.coresPerSocket` | Cores per socket |
| `host.cpuTopology.threadsPerCore` | SMT threads per core |
| `host.cpuTopology.totalCPUs` | Total logical CPUs |
| `host.numaNodes[].id` | NUMA node ID |
| `host.numaNodes[].cpus` | CPU list (e.g. `"0-47,96-143"`) |
| `host.numaNodes[].memoryMi` | Memory in MiB |
| `host.iommuEnabled` | Whether IOMMU is active |
| `host.hugepages1Gi.total` | Total 1GiB hugepages |
| `host.hugepages1Gi.free` | Free 1GiB hugepages |

### GPU devices

Each entry in `gpus[]`:

| Field | Description |
|-------|-------------|
| `index` | GPU index on this node (0-7) |
| `pciAddress` | Full PCI BDF (e.g. `"0000:17:00.0"`) |
| `vendor` | GPU manufacturer: `"NVIDIA"`, `"AMD"`, `"Intel"`, or `"Unknown (<vendor-id>)"` |
| `model` | Human-readable model (e.g. `"NVIDIA H200 SXM"`) |
| `deviceId` | PCI vendor:device ID (e.g. `"10de:2336"`) |
| `numaNode` | Physical NUMA node this GPU is attached to |
| `iommuGroup` | IOMMU group number |
| `driver` | Currently bound kernel driver (`vfio-pci`, `nvidia`, `nouveau`) |
| `barSizes[].region` | PCI BAR region number |
| `barSizes[].sizeMi` | BAR size in MiB (>64GB needs `x-no-mmap=true`) |
| `allocated` | `true` if assigned to a SwiftGuest |
| `allocatedTo` | `"namespace/name"` of the owning SwiftGuest |

### NVSwitch devices

Each entry in `nvSwitches[]`:

| Field | Description |
|-------|-------------|
| `pciAddress` | NVSwitch PCI BDF |
| `deviceId` | PCI vendor:device ID |
| `numaNode` | Physical NUMA node |

### Fabric Manager

| Field | Description |
|-------|-------------|
| `fabricManager.installed` | Whether FM is installed on host |
| `fabricManager.version` | FM version string |
| `fabricManager.running` | Whether FM service is active |
| `fabricManager.partitions[].id` | Partition ID |
| `fabricManager.partitions[].gpuIndices` | GPU indices in this partition |
| `fabricManager.partitions[].active` | Whether partition is activated |
| `fabricManager.partitions[].allocatedTo` | Owning SwiftGuest (if any) |

## Field ownership

| Fields | Written by |
|--------|-----------|
| `phase`, `lastDiscovery`, `host`, `gpuVendor`, `vfioReady`, `gpus[].index` through `gpus[].barSizes` (incl. `gpus[].vendor`), `nvSwitches`, `fabricManager.installed/version/running/partitions[].id/gpuIndices/active` | GPU Discovery DaemonSet |
| `gpus[].allocated`, `gpus[].allocatedTo`, `fabricManager.partitions[].allocatedTo`, `gpuCount`, `freeGPUs`, `gpuModel` | SwiftGPU Controller |

`fabricManager.partitions[].active` is written by discovery (it reflects the
host `fmpm` partition state, not a KubeSwift allocation decision) — `gpu-init`
activates the partition via `fmpm -a` before swiftletd starts, and the next
discovery cycle picks up the change.

The discovery DaemonSet preserves controller-owned fields during status patches.

## Inspecting allocation state

```bash
# List all GPU nodes
kubectl get sgn

# Check which GPUs are allocated on a node
kubectl get sgn <node> -o jsonpath='{range .status.gpus[*]}{.index} {.pciAddress} {.allocated} {.allocatedTo}{"\n"}{end}'

# Check free GPU count
kubectl get sgn <node> -o jsonpath='{.status.freeGPUs}'
```

## See also

[SwiftGPUProfile](swiftgpuprofile.md) · [SwiftGuest](swiftguest.md) · [GPU Passthrough Guide](../gpu-passthrough.md)
