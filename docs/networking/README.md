# Networking

KubeSwift VM networking is built on a **tap+bridge** model inside the launcher pod.
The primary NIC uses KubeSwift's built-in dnsmasq DHCP server. Secondary NICs are
backed by Multus NetworkAttachmentDefinitions (NADs) — KubeSwift bridges each Multus
interface to a tap device that the hypervisor connects to a guest virtio-net NIC.

KubeSwift couples to the **Multus + NAD interface**, not to any specific CNI plugin.
The operator chooses their network stack; KubeSwift just references NADs.

## Guides

| Guide | Description |
|-------|-------------|
| [Multi-NIC Support](../multi-nic.md) | General multi-interface architecture, CRD spec, MAC generation, status reporting |
| [OVN-Kubernetes Integration](ovn-kubernetes.md) | Layer 2/3, localnet, UDN, CUDN — use cases and examples for OVN-Kubernetes |
| [SR-IOV NIC Passthrough](sriov.md) | VFIO passthrough for VFs — GPUDirect RDMA, DPDK, line-rate networking |

## Architecture

```
Guest VM
   |  virtio-net (eth0) --- primary NIC (bridge)
   |  virtio-net (eth1) --- secondary NIC (bridge, Multus)
   |  hardware NIC (eth2) --- SR-IOV VF (VFIO passthrough)
   |
  tap0 --- br0 (10.244.125.1/24) --- dnsmasq DHCP
  tap1 --- br1 --- net1 (Multus interface)
  /dev/vfio/<group> --- direct VFIO passthrough (no tap, no bridge)
   |
  pod network (eth0, NOT bridged)
```

- Primary NIC: dnsmasq DHCP, NAT via iptables, IP discovery via lease polling
- Secondary bridge NICs: bridged to Multus-created interfaces, no dnsmasq, no NAT
- SR-IOV NICs: VFIO passthrough — guest sees hardware NIC, no tap/bridge overhead
- Guest IP assignment for secondary NICs: CNI IPAM or static via cloud-init networkData
