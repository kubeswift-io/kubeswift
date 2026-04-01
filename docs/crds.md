# CRD Reference

All KubeSwift CRDs are `v1alpha1`. This document covers every field, default value, and validation rule for each CRD.

## Mutual exclusivity rules

- `spec.imageRef` and `spec.kernelRef` on SwiftGuest are mutually exclusive.
- `spec.gpuProfileRef` and `spec.kernelRef` on SwiftGuest are mutually exclusive (GPU boot requires disk boot with UEFI).
- `spec.gpuProfileRef` can combine with `spec.imageRef`.

---

## SwiftGuest

**Group:** `swift.kubeswift.io/v1alpha1`
**Scope:** Namespaced
**Short name:** `sg`
**Subresource:** status

Represents a running virtual machine instance.

### Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `imageRef.name` | string | No | — | SwiftImage to boot from (disk boot). Mutually exclusive with `kernelRef`. |
| `kernelRef.name` | string | No | — | SwiftKernel to boot from (kernel boot). Mutually exclusive with `imageRef` and `gpuProfileRef`. |
| `kernelCmdline` | string | No | — | Per-guest kernel command line override (kernel boot only). Overrides SwiftKernel's default cmdline. |
| `guestClassRef.name` | string | Yes | — | SwiftGuestClass providing CPU, memory, and disk size. |
| `seedProfileRef.name` | string | No | — | SwiftSeedProfile for cloud-init (disk boot only). Optional. |
| `runPolicy` | enum | No | `Running` | `Running`, `Stopped`, `RestartOnFailure`, or `Always`. |
| `gpuProfileRef.name` | string | No | — | SwiftGPUProfile for GPU passthrough. Mutually exclusive with `kernelRef`. |

**RunPolicy values:**

| Value | Behavior |
|-------|----------|
| `Running` | Controller ensures VM is running. Creates pod if absent. |
| `Stopped` | Controller does not create a pod. Existing pod is left to terminate. |
| `RestartOnFailure` | Restarts on pod failure with exponential backoff. Does not restart on clean exit. |
| `Always` | Restarts on any pod exit with exponential backoff. |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | `Pending`, `Scheduling`, `Running`, `Stopped`, or `Failed`. |
| `conditions[]` | []Condition | Kubernetes conditions. See below. |
| `nodeName` | string | Node where the launcher pod is scheduled. |
| `podRef` | ObjectReference | Reference to the launcher pod. |
| `runtime.pid` | int64 | Hypervisor process PID (set by swiftletd annotation). |
| `runtime.hypervisor` | string | `cloud-hypervisor` or `qemu`. |
| `console.serialSocket` | string | Path to serial.sock inside the launcher pod. |
| `network.primaryIP` | string | Guest IP from DHCP lease. |
| `network.interfaces[]` | []GuestNetworkInterface | Interface name + IP pairs. |
| `restartCount` | int32 | Number of times the VM has been restarted. |
| `lastRestartTime` | Time | Timestamp of the last restart. |
| `gpu.devices[]` | []string | PCI addresses of allocated GPUs. |
| `gpu.partitionId` | int | Fabric Manager partition ID. -1 means none. |
| `gpu.numaNodes[]` | []int | NUMA node IDs of allocated GPUs. |
| `gpu.hypervisor` | string | Resolved hypervisor for this guest. |
| `gpu.nodeName` | string | Node where GPUs were allocated. |

**Conditions:**

| Type | Meaning |
|------|---------|
| `Resolved` | All refs (image, kernel, class, seed) have been resolved. |
| `PodScheduled` | The launcher pod has been scheduled to a node. |
| `GuestRunning` | The VM is running (set by swiftletd via kube-rs DynamicObject patch). |
| `GPUAllocated` | GPUs have been allocated by the SwiftGPU controller. |

### Example

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: sample
  namespace: default
spec:
  imageRef:
    name: ubuntu-cloud
  guestClassRef:
    name: default
  seedProfileRef:
    name: ssh
  runPolicy: Running
```

---

## SwiftGuestClass

**Group:** `swift.kubeswift.io/v1alpha1`
**Scope:** Cluster
**Short name:** `sgc`

Cluster-scoped template defining default CPU, memory, and disk resources for VMs.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `cpu` | Quantity | Yes | Number of vCPUs (e.g. `"2"`). |
| `memory` | Quantity | Yes | Memory size (e.g. `"2Gi"`). |
| `rootDisk.size` | Quantity | Yes | Root disk PVC size (e.g. `"40Gi"`). Should match SwiftImage's `rootDisk.size`. |
| `rootDisk.format` | enum | Yes | `raw` only at runtime. |

### Example

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: default
spec:
  cpu: "2"
  memory: "2Gi"
  rootDisk:
    size: "40Gi"
    format: raw
```

---

## SwiftImage

**Group:** `image.kubeswift.io/v1alpha1`
**Scope:** Namespaced
**Short name:** `si`
**Subresource:** status

Represents a VM disk image. The controller imports, converts, and patches the image into a PVC.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source.http.url` | string | No* | HTTP(S) URL to download the image from. |
| `source.pvcClone.name` | string | No* | PVC to clone from. |
| `source.pvcClone.namespace` | string | No | Source namespace (defaults to same namespace). |
| `format` | enum | Yes | Source format: `raw` or `qcow2`. Ubuntu cloud images are `qcow2`. |
| `rootDisk.size` | Quantity | No | PVC size for the imported image (defaults to 10Gi). Should match SwiftGuestClass `rootDisk.size`. |

*Exactly one source must be set.

### Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | `Pending`, `Importing`, `Validating`, `Preparing`, `Ready`, or `Failed`. |
| `sourceFormat` | string | Original format of the source image. |
| `preparedFormat` | string | Runtime format — always `raw`. |
| `preparedArtifact.pvcRef.name` | string | PVC containing image.raw. |
| `preparedArtifact.size` | Quantity | Measured size of image.raw. |
| `conditions[]` | []Condition | Import progress conditions. |

### Example

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-cloud
  namespace: default
spec:
  format: qcow2
  rootDisk:
    size: "40Gi"
  source:
    http:
      url: https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img
```

**Note:** Ubuntu Focal (20.04) is the verified working guest OS. Ubuntu Noble (24.04) is incompatible with rust-hypervisor-firmware.

---

## SwiftSeedProfile

**Group:** `seed.kubeswift.io/v1alpha1`
**Scope:** Namespaced
**Short name:** `ssp`

cloud-init NoCloud datasource configuration. Rendered to a seed ISO and mounted into the VM at boot.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `datasource` | enum | Yes | Only `NoCloud` is supported. |
| `userData` | string | No | Inline cloud-config YAML. |
| `userDataFrom` | SeedDataValueFrom | No | Reference to Secret or ConfigMap key. |
| `metaData` | string | No | Inline instance metadata YAML. |
| `metaDataFrom` | SeedDataValueFrom | No | Reference to Secret or ConfigMap key. |
| `networkData` | string | No | Inline network configuration. |
| `networkDataFrom` | SeedDataValueFrom | No | Reference to Secret or ConfigMap key. |

For each data field, use either the inline `value` variant or the `valueFrom` reference variant, not both.

**SeedDataValueFrom fields:**

| Field | Description |
|-------|-------------|
| `secretKeyRef.name` | Secret name |
| `secretKeyRef.key` | Key within the Secret |
| `configMapKeyRef.name` | ConfigMap name |
| `configMapKeyRef.key` | Key within the ConfigMap |

### Example

```yaml
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: ssh
  namespace: default
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: kubeswift-guest
    users:
      - name: kubeswift
        sudo: ALL=(ALL) NOPASSWD:ALL
        lock_passwd: false
        ssh_authorized_keys:
          - ssh-ed25519 AAAA...your-public-key...
    runcmd:
      - systemctl enable --now getty@ttyS0.service
  metaData: |
    instance-id: kubeswift-001
    local-hostname: kubeswift-guest
```

The `getty@ttyS0.service` runcmd is required for `swiftctl console` to work on cloud images. Without it, the serial console is silent after boot.

---

## SwiftKernel

**Group:** `kernel.kubeswift.io/v1alpha1`
**Scope:** Namespaced
**Short name:** `sk`
**Subresource:** status

Manages a kernel + initramfs OCI artifact. The controller pulls artifacts to labeled nodes.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `ociRef.image` | string | Yes | OCI artifact reference (e.g. `ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.1`). |
| `ociRef.pullSecret` | string | No | Image pull secret name for private registries. |
| `kernelCmdline` | string | No | Default kernel command line. Can be overridden per-guest via `spec.kernelCmdline`. |
| `profile` | string | No | Informational label for the kernel profile (e.g. `faas-minimal`). |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | `Pending`, `Pulling`, `Ready`, or `Failed`. |
| `conditions[]` | []Condition | `Ready`, `Failed`, `NoKernelNodes` conditions. |
| `nodeStatuses[]` | []NodeKernelStatus | Per-node pull progress. |
| `nodeStatuses[].nodeName` | string | Node name. |
| `nodeStatuses[].phase` | string | Pull phase for this node. |
| `kernelDigest` | string | Content digest of the bzImage layer. |
| `initramfsDigest` | string | Content digest of the rootfs.cpio.gz layer. |

The artifact path on each node is deterministic:
`/var/lib/kubeswift/kernels/<namespace>-<name>/bzImage`
`/var/lib/kubeswift/kernels/<namespace>-<name>/rootfs.cpio.gz`

This path is computed at runtime and never stored in status.

### Node labeling

The controller only pulls to nodes labeled `kubeswift.io/kernel-node=true`. If no nodes carry this label, `status.phase=Pending` with condition `NoKernelNodes`.

```bash
kubectl label node <node-name> kubeswift.io/kernel-node=true
```

### Example

```yaml
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata:
  name: faas-minimal
  namespace: default
spec:
  ociRef:
    image: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.1
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"
  profile: faas-minimal
```

**Note:** Use tag `6.6.1`. Tag `6.6.0` has broken networking (missing `CONFIG_PCI`).

---

## SwiftGPUProfile

**Group:** `gpu.kubeswift.io/v1alpha1`
**Scope:** Namespaced
**Short name:** `sgp`

Defines a GPU passthrough request. Multiple SwiftGuests can reference the same profile.

### Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `count` | int | Yes | — | Number of GPUs requested. Allowed values: 1, 2, 4, 8. |
| `model` | string | No | `""` | Optional GPU model filter (e.g. `"H200-SXM"`, `"A100-PCIe"`). Empty matches any. |
| `tier` | enum | Yes | `pcie` | `pcie`, `hgx-shared`, or `hgx-full`. Determines hypervisor. See tier table above. |
| `partitionMode` | enum | Yes | `isolated` | `isolated`, `shared`, or `full`. |
| `pcieTopology.rootPortPerDevice` | bool | No | `false` | Place each GPU behind a pcie-root-port (QEMU). Required for Tier 2/3. |
| `pcieTopology.gpuDirectClique` | int | No | `0` | `x_nv_gpudirect_clique` value for Cloud Hypervisor Tier 1. |
| `pcieTopology.noMmap` | bool | No | `false` | Add `x-no-mmap=true` to QEMU for GPUs with >64GB BARs. Required for B200. |
| `numaTopology.sockets` | int | No | — | Virtual CPU socket count. |
| `numaTopology.coresPerSocket` | int | No | — | Cores per virtual socket. |
| `numaTopology.threadsPerCore` | int | No | `1` | SMT threads per core. |
| `numaTopology.memoryPerSocketMi` | int64 | No | — | Memory per NUMA node in MiB. |
| `hugepages` | string | No | `""` | Hugepage size: `"1Gi"`, `"2Mi"`, or `""` (none). Required for Tier 2/3. |
| `vcpuPinning` | bool | No | `false` | Enable 1:1 vCPU-to-pCPU pinning. Requires NUMA topology. |
| `fabricManager.runInGuest` | bool | No | `false` | True for Tier 3 (FM in guest). False for Tier 2 (FM on host). |
| `fabricManager.requiredVersion` | string | No | `""` | Required nvidia-open driver version in guest. Must match host FM version. |

### Example

See [docs/gpu-passthrough.md](gpu-passthrough.md) for complete examples for each tier.

---

## SwiftGPUNode

**Group:** `gpu.kubeswift.io/v1alpha1`
**Scope:** Cluster
**Short name:** `sgn`
**Subresource:** status

Represents the GPU inventory on one node. Created and updated by the discovery DaemonSet. The `spec` is intentionally empty — all data is in `status`.

### Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | `Discovering`, `Ready`, or `Error`. |
| `lastDiscovery` | Time | Timestamp of last successful discovery run. |
| `gpuCount` | int | Total number of GPUs on this node. |
| `freeGPUs` | int | Unallocated GPUs. |
| `gpuModel` | string | GPU model (assumes homogeneous node). |
| `host.cpuTopology.sockets` | int | Physical CPU sockets. |
| `host.cpuTopology.coresPerSocket` | int | Cores per socket. |
| `host.cpuTopology.threadsPerCore` | int | SMT threads per core. |
| `host.cpuTopology.totalCPUs` | int | Total logical CPUs. |
| `host.numaNodes[].id` | int | NUMA node ID. |
| `host.numaNodes[].cpus` | string | CPU mask (e.g. `"0-47,96-143"`). |
| `host.numaNodes[].memoryMi` | int64 | Memory in MiB. |
| `host.iommuEnabled` | bool | Whether IOMMU is active. |
| `host.hugepages1Gi.total` | int | Total 1GiB hugepages. |
| `host.hugepages1Gi.free` | int | Free 1GiB hugepages. |
| `gpus[].index` | int | GPU index on this node (0-7). |
| `gpus[].pciAddress` | string | Full BDF (e.g. `"0000:17:00.0"`). |
| `gpus[].model` | string | Human-readable model string. |
| `gpus[].deviceId` | string | PCI vendor:device ID (e.g. `"10de:2336"`). |
| `gpus[].numaNode` | int | NUMA node this GPU is attached to. |
| `gpus[].iommuGroup` | int | IOMMU group number. |
| `gpus[].driver` | string | Bound kernel driver: `"vfio-pci"` or `"nvidia"`. |
| `gpus[].barSizes[].region` | int | BAR region index. |
| `gpus[].barSizes[].sizeMi` | int64 | BAR size in MiB. |
| `gpus[].allocated` | bool | True if allocated to a SwiftGuest. |
| `gpus[].allocatedTo` | string | `"namespace/name"` of the guest using this GPU. |
| `nvSwitches[].pciAddress` | string | NVSwitch PCI BDF. |
| `nvSwitches[].deviceId` | string | PCI vendor:device ID. |
| `nvSwitches[].numaNode` | int | NUMA node. |
| `fabricManager.installed` | bool | Whether FM is installed. |
| `fabricManager.version` | string | FM version string. |
| `fabricManager.running` | bool | Whether FM process is running. |
| `fabricManager.partitions[].id` | int | Partition ID. |
| `fabricManager.partitions[].gpuIndices` | []int | GPU indices in this partition. |
| `fabricManager.partitions[].active` | bool | Whether partition is activated. |
| `fabricManager.partitions[].allocatedTo` | string | Guest using this partition. |

### Node label

SwiftGPUNode objects are named after their nodes. The discovery DaemonSet only runs on nodes labeled:

```bash
kubectl label node <node-name> kubeswift.io/gpu-node=true
```
