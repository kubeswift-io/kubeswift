# SwiftGPUProfile

SwiftGPUProfile defines a **GPU passthrough request**. It describes how many GPUs, what model, which tier, and the virtual topology the guest needs. Multiple SwiftGuests can reference the same profile.

**API:** `gpu.kubeswift.io/v1alpha1` · **Short name:** `sgp` · **Scope:** Namespaced

## Operator workflow

1. Create a SwiftGPUProfile describing GPU requirements.
2. Create a SwiftGuest with `gpuProfileRef` pointing to the profile.
3. The SwiftGPU controller finds a SwiftGPUNode with enough free GPUs, allocates them, and sets `GPUAllocated=True` on the SwiftGuest.
4. The SwiftGuest controller creates the launcher pod with VFIO devices and gpu-init.

## Spec

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `count` | Yes | — | Number of GPUs (1, 2, 4, or 8) |
| `model` | No | `""` | GPU model filter (e.g. `H200-SXM`, `A100-PCIe`, `L40S`). Empty matches any. |
| `tier` | Yes | `pcie` | GPU complexity tier — determines hypervisor. See Tiers below. |
| `partitionMode` | Yes | `isolated` | GPU allocation mode: `isolated`, `shared`, or `full`. See Partition Modes. |
| `pcieTopology.rootPortPerDevice` | No | `false` | Place each GPU behind a virtual pcie-root-port (QEMU Tier 2/3). |
| `pcieTopology.gpuDirectClique` | No | `0` | `x_nv_gpudirect_clique` value for Cloud Hypervisor (Tier 1 only). |
| `pcieTopology.noMmap` | No | `false` | `x-no-mmap=true` on QEMU for GPUs with >64GB BARs (e.g. B200). |
| `numaTopology.sockets` | No | — | Virtual CPU sockets in the guest. |
| `numaTopology.coresPerSocket` | No | — | Cores per virtual socket. |
| `numaTopology.threadsPerCore` | No | `1` | SMT threads per core (usually 1). |
| `numaTopology.memoryPerSocketMi` | No | — | Memory per NUMA node in MiB. |
| `hugepages` | No | `""` | Hugepage size: `1Gi`, `2Mi`, or `""` (none). `1Gi` required for most GPU workloads. |
| `vcpuPinning` | No | `false` | 1:1 vCPU to physical CPU pinning. Critical for HGX performance. |
| `fabricManager.runInGuest` | No | `false` | `true` for Tier 3 full passthrough (FM inside guest). |
| `fabricManager.requiredVersion` | No | `""` | Host FM version must match guest nvidia-open driver. |

## Tiers

The `tier` field is the single decision point for hypervisor and topology selection:

| Tier | Hypervisor | PCIe Topology | NVSwitch | Fabric Manager | Example GPUs |
|------|------------|---------------|----------|----------------|--------------|
| `pcie` | Cloud Hypervisor | Flat (no root ports) | No | No | A100-PCIe, L40S, RTX 4090 |
| `hgx-shared` | QEMU | Root port per device | Host-managed | Host (shared partition) | H100-SXM, H200-SXM |
| `hgx-full` | QEMU | Full PCIe hierarchy | Passed to guest | Guest | 8-GPU HGX full passthrough |

Firmware is auto-selected: `CLOUDHV.fd` for Cloud Hypervisor, `OVMF` for QEMU.

## Partition Modes

| Mode | Description |
|------|-------------|
| `isolated` | No NVLink. Single GPU or multiple GPUs without fabric connectivity. |
| `shared` | GPUs share NVSwitch fabric via host Fabric Manager partition. |
| `full` | All GPUs + NVSwitches passed to one VM. Fabric Manager runs inside guest. |

## Examples

### Single PCIe GPU (Tier 1)

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: a100-pcie-single
spec:
  count: 1
  model: "A100-PCIe"
  tier: pcie
  partitionMode: isolated
  pcieTopology:
    gpuDirectClique: 0
  hugepages: "1Gi"
```

### Multi-PCIe GPU (Tier 1)

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: a100-pcie-4gpu
spec:
  count: 4
  model: "A100-PCIe"
  tier: pcie
  partitionMode: isolated
  pcieTopology:
    gpuDirectClique: 0
  numaTopology:
    sockets: 2
    coresPerSocket: 24
    memoryPerSocketMi: 524288
  hugepages: "1Gi"
  vcpuPinning: true
```

### HGX 4-GPU Shared (Tier 2)

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: h200-hgx-4gpu
spec:
  count: 4
  model: "H200-SXM"
  tier: hgx-shared
  partitionMode: shared
  pcieTopology:
    rootPortPerDevice: true
    noMmap: true
  numaTopology:
    sockets: 2
    coresPerSocket: 40
    threadsPerCore: 1
    memoryPerSocketMi: 983040
  hugepages: "1Gi"
  vcpuPinning: true
  fabricManager:
    runInGuest: false
    requiredVersion: "580.95.05"
```

## See also

[SwiftGPUNode](swiftgpunode.md) · [SwiftGuest](swiftguest.md) · [GPU Passthrough Guide](../gpu-passthrough.md)
