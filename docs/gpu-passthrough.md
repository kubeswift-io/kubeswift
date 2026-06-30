# GPU Passthrough

KubeSwift supports GPU passthrough using VFIO. GPUs appear as PCI devices inside the VM. The approach differs based on GPU hardware class — the `tier` field in SwiftGPUProfile is the single selection point.

> This page covers the **native** allocation backend (`spec.gpuProfileRef`), where the SwiftGPU controller picks node + devices from its own inventory. KubeSwift can alternatively let the **kube-scheduler** allocate GPUs through standard Kubernetes ResourceClaims — see [GPU allocation via DRA](gpu/dra-allocation.md) (`spec.gpuResourceClaim`). The runtime passthrough path is identical for both.

## Prerequisites

### Host requirements

- IOMMU enabled in BIOS and kernel (`intel_iommu=on` or `amd_iommu=on` in kernel command line)
- `vfio-pci` kernel module loaded
- GPUs bound to the `vfio-pci` driver before guest creation (the `gpu-init` init container handles this automatically)
- Node labeled `kubeswift.io/gpu-node=true`

Verify IOMMU:

```bash
dmesg | grep -e DMAR -e IOMMU | head -5
# Expected: "DMAR: IOMMU enabled" or "AMD-Vi: AMD IOMMUv2 loaded and initialized"
```

Verify vfio-pci:

```bash
lsmod | grep vfio
# Expected: vfio_pci, vfio_iommu_type1, vfio listed
```

### For HGX SXM nodes (Tier 2/3 only)

- NVIDIA Fabric Manager installed and running
- Host FM version must exactly match the `nvidia-open` driver version in the guest image
- 1GiB hugepages configured (`/proc/sys/vm/nr_hugepages`)
- OVMF firmware available at `/usr/share/OVMF/OVMF_CODE.fd` (included in swiftletd image)

## GPU compatibility tiers

| GPU class | Tier | Hypervisor | PCIe root ports | Fabric Manager | NUMA | Hugepages |
|-----------|------|------------|----------------|---------------|------|-----------|
| T4, RTX 4090 | `pcie` | Cloud Hypervisor | No | No | Optional | Optional |
| L40S | `pcie` | Cloud Hypervisor | No | No | Optional | Recommended |
| A100-PCIe | `pcie` | Cloud Hypervisor | No | No | Recommended | Recommended |
| A100-SXM (isolated) | `hgx-shared` | QEMU | Yes | No | Recommended | Required |
| H100-SXM (2-4 GPU) | `hgx-shared` | QEMU | Yes | Yes (host) | Required | Required |
| H200-SXM (2-4 GPU) | `hgx-shared` | QEMU | Yes | Yes (host) | Required | Required |
| B200-SXM (2-4 GPU) | `hgx-shared` | QEMU | Yes + noMmap | Yes (host) | Required | Required |
| Any HGX (8 GPU full) | `hgx-full` | QEMU | Full hierarchy | Yes (guest) | Required | Required |

**Tier 1 (`pcie`)**: Cloud Hypervisor is used. Each GPU is passed with `--device path=<sysfs>,x_nv_gpudirect_clique=0`. Flat PCI topology is sufficient — CUDA initializes correctly for PCIe-attached GPUs.

**Tier 2 (`hgx-shared`)**: QEMU is required. CUDA refuses to initialize on a flat PCI topology for HGX SXM GPUs. Each GPU is placed behind its own `pcie-root-port`. The host Fabric Manager manages NVSwitch partitions. OVMF firmware is used for UEFI boot.

**Tier 3 (`hgx-full`)**: QEMU with full PCIe hierarchy including expander buses, root ports, and switches. NVSwitches are passed to the guest. Fabric Manager runs inside the guest. This is Phase 4 — not yet implemented.

## GPU Discovery DaemonSet

The discovery DaemonSet (`gpu-discovery`) auto-detects GPU hardware on labeled nodes and populates the SwiftGPUNode status. It runs in a loop (default: 60 seconds) reading sysfs, lspci, lscpu, and Fabric Manager state.

### Enable via Helm

```bash
helm upgrade kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift \
  --set gpuDiscovery.enabled=true
```

Or apply manifests directly:

```bash
kubectl apply -f config/rbac/gpu-discovery-rbac.yaml
kubectl apply -f config/daemonset/gpu-discovery.yaml
```

### What it discovers

- **GPUs**: PCI address, model, device ID, NUMA node, IOMMU group, driver binding, BAR sizes
- **Host topology**: CPU sockets/cores/threads, NUMA nodes with CPU masks and memory, IOMMU status, 1GiB hugepage counts
- **NVSwitches**: PCI addresses and device IDs (HGX nodes)
- **Fabric Manager**: installed/version/running, partition IDs, GPU indices per partition, active state

### Field ownership

The discovery DaemonSet and SwiftGPU controller both write to SwiftGPUNode status but own different fields:

| Field | Owner |
|-------|-------|
| `phase`, `host`, `gpus[].model/pciAddress/driver/barSizes/numaNode/iommuGroup/deviceId`, `nvSwitches`, `fabricManager.installed/version/running`, `fabricManager.partitions[].id/gpuIndices/active` | Discovery DaemonSet |
| `gpus[].allocated`, `gpus[].allocatedTo`, `fabricManager.partitions[].allocatedTo`, `freeGPUs`, `gpuCount`, `gpuModel` | SwiftGPU controller |

Discovery preserves controller-owned fields during status patches by reading the existing status first and merging.

### Security

- **No privileged containers**: drops ALL capabilities, readOnlyRootFilesystem
- **/sys and /dev mounted read-only**: sysfs and lspci reads only, no host state modification
- **hostPID=false, hostNetwork=false**
- **Separate RBAC**: only `swiftgpunodes` (get/list/create/patch) + `swiftgpunodes/status` (get/patch) + `nodes` (get)

## Workflow

### Step 1: Label a GPU node

```bash
kubectl label node <node-name> kubeswift.io/gpu-node=true
```

### Step 2: Wait for GPU discovery

The discovery DaemonSet runs on labeled nodes and populates SwiftGPUNode status:

```bash
kubectl get swiftgpunode <node-name> -o yaml
```

Expected output includes GPU list, NUMA topology, and Fabric Manager state:

```bash
kubectl get sgn
# NAME        PHASE   GPUS   FREE   MODEL
# gpu-node1   Ready   4      4      NVIDIA H200 SXM
```

### Step 3: Create a SwiftGPUProfile

See examples below. Create the profile that matches your hardware tier.

### Step 4: Create a SwiftImage

GPU VMs boot from a disk image. Use Ubuntu Noble with the correct NVIDIA driver version installed.

```bash
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml
kubectl get swiftimage ubuntu-noble -w
```

### Step 5: Create a SwiftGuest with gpuProfileRef

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: gpu-test
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble
  gpuProfileRef:
    name: a100-pcie-single
  guestClassRef:
    name: default
  seedProfileRef:
    name: ssh
  runPolicy: Running
```

```bash
kubectl apply -f swiftguest-gpu.yaml
kubectl get swiftguest gpu-test -w
```

The SwiftGPU controller allocates GPUs and sets `GPUAllocated=True`. The SwiftGuest controller then creates the launcher pod.

### Step 6: Verify inside the guest

```bash
swiftctl ssh gpu-test -- nvidia-smi
```

## SwiftGPUProfile examples

### Tier 1: Single PCIe GPU (Cloud Hypervisor)

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: a100-pcie-single
  namespace: default
spec:
  count: 1
  model: "A100-PCIe"
  tier: pcie
  partitionMode: isolated
  pcieTopology:
    rootPortPerDevice: false
    gpuDirectClique: 0
    noMmap: false
  hugepages: "1Gi"
  vcpuPinning: false
```

No QEMU, no Fabric Manager, no NUMA required. The controller adds `--device path=<sysfs>,x_nv_gpudirect_clique=0` to Cloud Hypervisor.

### Tier 1: Multi-PCIe GPU with GPUDirect (Cloud Hypervisor)

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: a100-pcie-4gpu
  namespace: default
spec:
  count: 4
  model: "A100-PCIe"
  tier: pcie
  partitionMode: isolated
  pcieTopology:
    rootPortPerDevice: false
    gpuDirectClique: 0
  numaTopology:
    sockets: 2
    coresPerSocket: 24
    threadsPerCore: 1
    memoryPerSocketMi: 524288
  hugepages: "1Gi"
  vcpuPinning: true
```

All four GPUs share `gpuDirectClique=0` to enable PCIe P2P DMA between them.

### Tier 2: HGX SXM with shared NVSwitch (QEMU)

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: h200-hgx-4gpu
  namespace: default
spec:
  count: 4
  model: "H200-SXM"
  tier: hgx-shared
  partitionMode: shared
  pcieTopology:
    rootPortPerDevice: true
    gpuDirectClique: 0
    noMmap: true        # H200 has >64GB BARs — required to prevent boot stall
  numaTopology:
    sockets: 2
    coresPerSocket: 40
    threadsPerCore: 1
    memoryPerSocketMi: 983040   # 960 GiB per socket
  hugepages: "1Gi"
  vcpuPinning: true
  fabricManager:
    runInGuest: false
    requiredVersion: "580.95.05"  # must match host FM version exactly
```

This triggers the QEMU path. Each GPU gets its own `pcie-root-port`. NUMA topology is mapped to `-numa node` definitions in QEMU. vCPU pinning is computed from the SwiftGPUNode host topology.

### Tier 2: Single HGX SXM GPU (isolated, no NVLink)

```yaml
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUProfile
metadata:
  name: h200-single-isolated
  namespace: default
spec:
  count: 1
  model: "H200-SXM"
  tier: hgx-shared     # QEMU is still required for SXM GPUs
  partitionMode: isolated
  pcieTopology:
    rootPortPerDevice: true
    noMmap: true
  hugepages: "1Gi"
```

Even for a single SXM GPU in isolated mode, QEMU is required because CUDA needs the GPU behind a root port.

## SwiftGPUNode reference

SwiftGPUNode objects are created and updated by the GPU discovery DaemonSet. They are cluster-scoped with one object per GPU node.

```bash
kubectl get sgn
kubectl describe sgn <node-name>
kubectl get sgn <node-name> -o jsonpath='{.status.gpus[*].pciAddress}'
kubectl get sgn <node-name> -o jsonpath='{.status.fabricManager.version}'
```

Key fields in `status`:

| Field | Description |
|-------|-------------|
| `phase` | Discovering, Ready, or Error |
| `gpuCount` | Total GPUs on this node |
| `freeGPUs` | Unallocated GPUs |
| `gpuModel` | GPU model (assumes homogeneous node) |
| `host.iommuEnabled` | Whether IOMMU is active |
| `host.hugepages1Gi.free` | Available 1GiB hugepages |
| `gpus[].pciAddress` | Full BDF (e.g. "0000:17:00.0") |
| `gpus[].driver` | Bound driver: "vfio-pci" or "nvidia" |
| `gpus[].allocated` | Whether this GPU is in use |
| `gpus[].allocatedTo` | "namespace/name" of guest using this GPU |
| `gpus[].barSizes` | PCI BAR sizes (used for noMmap decision) |
| `fabricManager.version` | Host FM version |
| `fabricManager.partitions[].active` | Whether partition is activated |

## Fabric Manager

Fabric Manager (FM) is required for HGX SXM GPUs in shared or full passthrough mode. It manages the NVSwitch fabric that connects GPUs.

**Version matching**: The FM version on the host must exactly match the `nvidia-open` driver version in the guest image. Mismatches cause CUDA initialization failures that appear as "no devices found" in `nvidia-smi`.

**Partition lifecycle**:

1. The SwiftGPU controller selects an available FM partition based on `count` and `gpuIndices`
2. The `gpu-init` init container activates the partition via `fmpm -a <partition-id>` before swiftletd starts
3. The partition ID is passed to swiftletd via the RuntimeIntent (`gpu.fabricManagerPartitionId`)
4. On pod termination, the partition is deactivated

**Partition mode**:

- `isolated` — No Fabric Manager involvement. GPUs have no NVLink connectivity.
- `shared` — Host FM manages the partition. Guest uses NVLink through the NVSwitch fabric.
- `full` — All GPUs + NVSwitches passed to a single VM. FM runs inside the guest. (Phase 4)

## Network Separation for GPU Workloads

GPU training workloads benefit from separating management traffic (SSH, monitoring)
from GPU data plane traffic (NCCL collectives, GPUDirect RDMA). KubeSwift supports
this via multi-NIC — add a secondary interface for GPU traffic:

```yaml
spec:
  gpuProfileRef:
    name: h200-hgx-4gpu
  interfaces:
  - name: mgmt        # Management: SSH, cloud-init
  - name: gpu-data    # GPU: NCCL, GPUDirect
    networkRef:
      name: gpu-data-net
```

For OVN-Kubernetes secondary networks (Layer 2, localnet, CUDN), see the
[OVN-Kubernetes Integration Guide](networking/ovn-kubernetes.md#2-gpu-data-plane-separation).

For general multi-NIC configuration, see [Multi-NIC Support](multi-nic.md).

> For true RDMA line-rate performance, SR-IOV NIC passthrough is recommended
> over overlay networks. See [SR-IOV NIC Passthrough](networking/sriov.md)
> for setup, including the GPUDirect RDMA guide.

## IOMMU Group Peer Handling

VFIO requires all devices in an IOMMU group to be isolated (bound to vfio-pci or
unbound). Consumer NVIDIA GPUs (GTX, RTX) share their IOMMU group with a companion
HD Audio controller. HGX GPUs may share with NVSwitch or other devices.

The `gpu-init` init container handles this automatically:
1. For each GPU PCI address, it discovers the IOMMU group via sysfs
2. It enumerates all devices in the group
3. PCIe bridges (class `0x0604xx`) are skipped — VFIO handles them internally
4. All other peer devices (audio controllers, USB, etc.) are bound to vfio-pci
5. Only the GPU itself is passed to the VM — peers are bound solely for isolation

This means consumer GPUs (GTX 1080, RTX 4090, etc.) work out of the box without
manual pre-binding of companion devices.

## Troubleshooting

**CUDA initialization fails ("no devices found")**

- Check `tier` field. SXM GPUs require `tier: hgx-shared`, not `tier: pcie`.
- Verify IOMMU is enabled: `dmesg | grep IOMMU`.
- Check driver binding: `kubectl get sgn <node> -o jsonpath='{.status.gpus[*].driver}'` — must show `vfio-pci`.
- Check the gpu-init container log: `kubectl logs <pod> -c gpu-init`.

**QEMU boot stall (many minutes before guest appears)**

- The GPU has a large BAR (>64GB). Add `noMmap: true` to `spec.pcieTopology` in the SwiftGPUProfile.
- B200 GPUs have 256GB BARs and always require `noMmap: true`.

**Driver version mismatch**

- The guest nvidia-open driver version must exactly match the host FM version.
- Check host FM version: `kubectl get sgn <node> -o jsonpath='{.status.fabricManager.version}'`.
- Set `spec.fabricManager.requiredVersion` in the SwiftGPUProfile to enforce the match at allocation time.
- Rebuild the guest image with the matching driver version.

**GPUAllocated condition never becomes True**

- Check if any SwiftGPUNode has enough free GPUs of the requested model.
- Check the SwiftGPU controller logs: `kubectl logs -n kubeswift-system deployment/kubeswift-controller-manager`.
- Verify `freeGPUs` in `kubectl get sgn`.

**Guest boots but nvidia-smi shows wrong topology**

- For HGX SXM, verify `numaTopology` in the SwiftGPUProfile matches the physical NUMA layout in SwiftGPUNode.
- Verify vCPU pinning is set correctly. Run `swiftctl debug <guest>` and inspect QEMU command line.
