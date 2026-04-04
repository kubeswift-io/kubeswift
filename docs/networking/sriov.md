# SR-IOV NIC Passthrough

KubeSwift supports passing SR-IOV Virtual Functions (VFs) directly to VMs via VFIO,
bypassing the tap+bridge model entirely. The guest sees a real hardware NIC with
native performance — no virtio-net overhead, no encapsulation.

This is the recommended path for:
- **GPUDirect RDMA**: NCCL over InfiniBand/RoCE with zero-copy GPU memory transfers
- **NFV/DPDK**: Line-rate packet processing with userspace drivers
- **Low-latency networking**: Sub-microsecond latency for HPC and financial workloads

## How It Works

SR-IOV NIC passthrough reuses the same VFIO mechanism as GPU passthrough. An SR-IOV
VF is a PCI device — the same unbind-from-host + bind-to-vfio-pci + pass-/dev/vfio
pattern applies.

1. The **SR-IOV device plugin** discovers VFs on the node and advertises them as
   extended resources (e.g., `intel.com/sriov_netdevice`)
2. The SwiftGuest controller adds the resource to the launcher pod's `resources.limits`
3. The device plugin allocates a VF, binds it to `vfio-pci`, and exposes `/dev/vfio/<group>`
4. `network-init.sh` skips SR-IOV interfaces (no tap, no bridge)
5. **swiftletd** discovers the VF's PCI address from the `PCIDEVICE_*` environment variable
   and passes it directly to the hypervisor:
   - Cloud Hypervisor: `--device path=/sys/bus/pci/devices/<vf-address>/`
   - QEMU: `-device vfio-pci,host=<vf-address>`
6. The guest sees a hardware NIC (e.g., ConnectX-7 VF) and needs the appropriate driver

## Prerequisites

- **SR-IOV capable NIC**: Mellanox ConnectX-6/7, Intel E810, Broadcom P2100G, etc.
- **VFs configured on the Physical Function (PF)**: `echo N > /sys/class/net/<pf>/device/sriov_numvfs`
- **SR-IOV Network Operator** or device plugin installed in the cluster
- **Multus CNI** installed
- **IOMMU enabled** in BIOS and kernel (`intel_iommu=on` or `amd_iommu=on`)
- **Guest image** with the NIC driver (`mlx5_core` for Mellanox, `i40e`/`ice` for Intel)

## SwiftGuest Interface Spec

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: sriov-vm
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  interfaces:
  - name: mgmt
    # Primary NIC — KubeSwift tap+bridge+dnsmasq (default type: bridge)
  - name: rdma
    type: sriov
    resourceName: intel.com/sriov_netdevice
    networkRef:
      name: sriov-rdma-net
  runPolicy: Running
```

### Field Reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | No | `bridge` (default) or `sriov` |
| `resourceName` | string | Yes (for sriov) | SR-IOV device plugin resource name |
| `networkRef.name` | string | Yes (for sriov) | NAD name (must have `deviceType: vfio-pci`) |

### What happens differently for SR-IOV NICs

| Aspect | Bridge NIC | SR-IOV NIC |
|--------|-----------|------------|
| Pod network device | tap + bridge | VFIO passthrough |
| Guest NIC type | virtio-net | Hardware VF (e.g., mlx5) |
| MAC address | Controller-generated | Hardware MAC from VF |
| IP assignment | dnsmasq DHCP or cloud-init | cloud-init or guest DHCP |
| network-init.sh | Creates bridge + tap | Skipped |
| Pod resource limits | None | `resourceName: "1"` |
| /dev/vfio mount | Not needed | Required |
| Guest driver | virtio (built-in) | NIC vendor driver |

## Setup Guide

### Step 1: Configure VFs on the PF

```bash
# Check SR-IOV capability
cat /sys/class/net/ens1f0/device/sriov_totalvfs
# Output: 64

# Create VFs
echo 8 > /sys/class/net/ens1f0/device/sriov_numvfs

# Verify
lspci | grep "Virtual Function"
```

### Step 2: Install SR-IOV Network Operator

The SR-IOV Network Operator automates VF creation and device plugin deployment:

```bash
# OpenShift
oc apply -f https://raw.githubusercontent.com/openshift/sriov-network-operator/master/deploy/...

# Or use the standalone device plugin
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/sriov-network-device-plugin/master/deployments/...
```

### Step 3: Create the NAD

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: sriov-rdma-net
  namespace: default
  annotations:
    k8s.v1.cni.cncf.io/resourceName: intel.com/sriov_netdevice
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "sriov-rdma-net",
      "type": "sriov",
      "deviceType": "vfio-pci",
      "vlan": 0
    }
```

The `deviceType: vfio-pci` is critical — it tells the device plugin to bind the VF
to vfio-pci instead of leaving it as a kernel netdevice.

### Step 4: Create the SwiftGuest

```bash
kubectl apply -f config/samples/sriov/swiftguest-sriov.yaml
kubectl get swiftguest sriov-test -w
```

### Step 5: Verify inside the guest

```bash
swiftctl ssh sriov-test
# Inside guest:
lspci | grep -i "virtual function"
ip link show   # Should show hardware NIC (e.g., enp0s4)
ibstat          # For InfiniBand VFs
```

## GPUDirect RDMA

For GPU training workloads, combine GPU passthrough with SR-IOV RDMA:

```yaml
spec:
  gpuProfileRef:
    name: h200-hgx-4gpu
  interfaces:
  - name: mgmt
  - name: rdma
    type: sriov
    resourceName: intel.com/sriov_netdevice
    networkRef:
      name: sriov-rdma-net
```

For optimal performance, the SR-IOV VF and GPUs should be on the **same NUMA node**.
The SR-IOV device plugin supports NUMA-aware scheduling — configure it to prefer
VFs on the same NUMA node as the GPU.

Inside the guest, install:
- NVIDIA GPU driver (nvidia-open, matching host Fabric Manager version)
- Mellanox OFED (mlx5_core + nv_peer_mem for GPUDirect RDMA)
- NCCL with RDMA transport

See [config/samples/sriov/swiftguest-gpu-rdma.yaml](../../config/samples/sriov/swiftguest-gpu-rdma.yaml)
for a complete example.

## Troubleshooting

**VF not allocated (pod pending)**
- Check device plugin logs: `kubectl logs -n kube-system ds/sriov-device-plugin`
- Verify VFs exist: `lspci | grep "Virtual Function"` on the node
- Verify resource is available: `kubectl describe node <name> | grep sriov`

**Guest doesn't see the NIC**
- Verify the VF is bound to vfio-pci: `ls -la /sys/bus/pci/devices/<vf>/driver`
- Check IOMMU: `dmesg | grep -i iommu`
- Verify the guest has the NIC driver installed

**IOMMU group contains multiple devices**
- Some VFs share IOMMU groups with other VFs or the PF
- Use ACS override if needed (kernel parameter `pcie_acs_override=downstream,multifunction`)
- This is a host BIOS/platform issue, not a KubeSwift issue

**GPUDirect RDMA not working**
- Verify GPU and NIC are on the same NUMA node: `cat /sys/bus/pci/devices/<bdf>/numa_node`
- Verify `nv_peer_mem` module is loaded in the guest
- Test with `ib_write_bw` before running NCCL
