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
| [Service Exposure](service-exposure.md) | Expose services to/from VMs — ports, Services, egress observability, pool-balanced Services |
| [Ecosystem Integrations](ecosystem-integrations.md) | MetalLB, Gateway API, Tailscale, Istio, Linkerd recipes |
| [Operations Guide](operations-guide.md) | Physical networks, VLANs, bonds -- how to give VMs access to real networks |
| [Virtualization Comparison](virtualization-comparison.md) | VMware ESXi and Proxmox VE concept mapping for migration |
| [Multi-NIC Support](../multi-nic.md) | CRD spec reference, MAC generation, status reporting, architecture |
| [OVN-Kubernetes Integration](ovn-kubernetes.md) | Layer 2/3, localnet, UDN, CUDN -- OVN-Kubernetes specific topologies |
| [SR-IOV NIC Passthrough](sriov.md) | VFIO passthrough for VFs -- GPUDirect RDMA, DPDK, line-rate networking |

## Architecture

```
Guest VM
   |  virtio-net (enp0s3) --- primary NIC (bridge)
   |  virtio-net (enp0s4) --- secondary NIC (bridge, Multus: macvlan/bridge/OVN-K/vlan)
   |  virtio-net (enp0s5) --- secondary NIC (bridge, Multus: another network)
   |  hardware NIC (enp0s6) --- SR-IOV VF (VFIO passthrough)
   |
  tap0 --- br0 (192.168.99.1/24) --- dnsmasq DHCP
  tap1 --- br1 --- net1 (Multus: macvlan on eno2, or bridge CNI, or OVN-K overlay)
  tap2 --- br2 --- net2 (Multus: macvlan on bond0.100, or OVN-K localnet VLAN)
  /dev/vfio/<group> --- direct VFIO passthrough (no tap, no bridge)
   |
  pod network (eth0, NOT bridged)
```

- Primary NIC: dnsmasq DHCP, NAT via iptables, IP discovery via lease polling
- Secondary bridge NICs: bridged to Multus-created interfaces, no dnsmasq, no NAT
- SR-IOV NICs: VFIO passthrough — guest sees hardware NIC, no tap/bridge overhead
- Guest IP assignment for secondary NICs: CNI IPAM or static via cloud-init networkData
