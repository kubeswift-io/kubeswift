# Virtualization Platform Comparison

Quick-reference for operators migrating from VMware ESXi, Proxmox VE, or other
traditional virtualization platforms to KubeSwift.

For step-by-step setup instructions, see the
[Networking Operations Guide](operations-guide.md).

---

## VMware ESXi / vSphere

| VMware Concept | KubeSwift Equivalent | Notes |
|---|---|---|
| vSwitch | Linux bridge on node + bridge CNI NAD | Node-local, like a standard vSwitch |
| Distributed vSwitch (dvSwitch) | OVN-Kubernetes | Cluster-wide, centrally managed |
| Port Group | NetworkAttachmentDefinition (NAD) | Defines network + VLAN + IPAM |
| Port Group VLAN tag | `vlanId` in NAD or VLAN sub-interface as macvlan master | See [Section 2](operations-guide.md#section-2-vms-on-specific-vlans) |
| Trunk port | bond0 with 802.1q VLANs | Same Linux bonding/VLAN stack |
| VM Network Adapter | `spec.interfaces[]` entry on SwiftGuest | |
| VMkernel adapter | Not applicable -- Kubernetes pod network handles node traffic | |
| Physical NIC (vmnic) | Host NIC (eno1, eno2, bond0) | |
| NIC teaming | Linux bond (802.3ad LACP or active-backup) | See [Section 8](operations-guide.md#section-8-host-preparation-reference) |
| Private VLAN | OVN-K UDN with namespace isolation | See [OVN-K guide](ovn-kubernetes.md) Section 4 |
| Host-only network | bridge CNI NAD with no physical NIC | See [Section 3](operations-guide.md#section-3-isolated-virtual-networks-no-physical-nic) |
| vMotion | SwiftMigration (live and offline migration) | KubeSwift supports live migration (sub-second downtime, optional mTLS, `kubectl drain` integration) and offline migration. See [Migration overview](../migration/overview.md). Cross-node IP preservation needs multi-node L2 (see [multi-node L2](multi-node-l2.md)) |
| Storage vSwitch | macvlan NAD on dedicated storage NIC | See [Section 1](operations-guide.md#section-1-vms-on-a-dedicated-physical-network) |

### Common VMware migration scenarios

| In VMware I... | In KubeSwift I... |
|---|---|
| Create a port group on VLAN 100 | Create a NAD with macvlan `master=bond0.100` |
| Add a NIC to a VM on that port group | Add an entry to `spec.interfaces[]` with `networkRef` to that NAD |
| Set a static IP on a VM NIC | Set the IP in SwiftSeedProfile `networkData` |
| Create an internal-only network | Create a bridge CNI NAD with no physical NIC |
| Trunk multiple VLANs to a VM | Add multiple `interfaces[]`, each referencing a different VLAN NAD |
| Use LACP bonding | Configure Linux bond on nodes (netplan or nmcli) |
| Set up a dedicated storage network | Create a macvlan NAD on the storage NIC |
| Isolate tenants | Use OVN-K UserDefinedNetwork per namespace |
| Use SR-IOV passthrough | Add `type: sriov` interface with `resourceName` |

### Key differences from VMware

1. **No centralized management plane for networking by default.** VMware vCenter
   manages dvSwitches globally. In KubeSwift, use OVN-Kubernetes for cluster-wide
   network management, or manage NADs per-namespace.

2. **NADs are namespace-scoped.** Unlike port groups which are datacenter-wide, NADs
   exist in a Kubernetes namespace. Use ClusterUserDefinedNetwork (CUDN) in OVN-K
   for cross-namespace networks.

3. **No GUI for network management.** All configuration is declarative YAML. Use
   `kubectl get net-attach-def` to list networks.

4. **Primary NIC is automatic.** In VMware you explicitly assign every NIC. In
   KubeSwift, the primary management NIC is created automatically -- you only
   configure secondary NICs.

5. **No live NIC hotplug.** In VMware you can add/remove NICs while a VM is running.
   In KubeSwift, interfaces are fixed at VM creation time.

---

## Proxmox VE

| Proxmox Concept | KubeSwift Equivalent | Notes |
|---|---|---|
| Linux Bridge (vmbr0) | Linux bridge on node + bridge CNI NAD | Direct equivalent |
| OVS Bridge | OVS bridge + OVN-K localnet NAD | For OVN-K deployments |
| VLAN tag on bridge | macvlan on VLAN sub-interface, or OVN-K localnet `vlanID` | |
| VLAN trunk to VM | Multiple `interfaces[]` with VLAN NADs | |
| Bond (bond0) | Linux bond (802.3ad, active-backup) | Same Linux bonding |
| Firewall | OVN-K network policies on secondary networks | Or Kubernetes NetworkPolicy on primary |
| SDN (Proxmox 8+) | OVN-Kubernetes | Both can use OVN under the hood |
| VM NIC model (virtio) | Always virtio-net (KubeSwift default) | SR-IOV for hardware passthrough |
| VLAN-aware bridge | macvlan on VLAN sub-interfaces or OVN-K localnet | |
| Internal network | bridge CNI NAD (same-node) or OVN-K Layer 2 (cross-node) | |
| Cloud-Init drive | SwiftSeedProfile (NoCloud datasource) | |

### Common Proxmox migration scenarios

| In Proxmox I... | In KubeSwift I... |
|---|---|
| Create vmbr1 and attach eno2 | Create a bridge on the node + bridge CNI NAD, or use macvlan directly |
| Set VLAN tag 100 on a VM NIC | Reference a NAD with macvlan `master=bond0.100` |
| Use VLAN-aware bridge with tags | Create separate NADs per VLAN (macvlan or OVN-K localnet) |
| Create an internal network (no physical port) | Create a bridge CNI NAD with `isGateway: false` |
| Bond two NICs (LACP) | Configure Linux bond via netplan/nmcli |
| Pass through a NIC (PCI passthrough) | Use `type: sriov` interface with SR-IOV VFs |
| Use cloud-init for network config | Set `networkData` in SwiftSeedProfile |
| Use Proxmox SDN with VNets | Use OVN-Kubernetes with NADs |

### Key differences from Proxmox

1. **Bridges are not the primary abstraction.** In Proxmox, you create a bridge
   (vmbr0, vmbr1) and attach NICs to it. In KubeSwift, macvlan is the simplest
   path -- it creates virtual interfaces directly on the physical NIC without a
   bridge. Use bridge CNI when you need the bridge model.

2. **No per-node web UI.** Proxmox has a web UI per node for network management.
   KubeSwift uses `kubectl` and YAML manifests.

3. **VLAN tagging is per-NAD, not per-NIC.** In Proxmox, you set a VLAN tag on
   each VM NIC. In KubeSwift, the VLAN is baked into the NAD definition, and the
   SwiftGuest references the NAD.

4. **Multi-node networking requires Multus + CNI.** In Proxmox, each node has its
   own bridges. In KubeSwift, Multus delegates to CNI plugins for secondary networks.
   For cross-node virtual networks, use OVN-Kubernetes.

5. **cloud-init is the primary guest configuration method.** Proxmox supports both
   cloud-init and manual configuration. KubeSwift uses SwiftSeedProfile (NoCloud)
   exclusively for guest provisioning.

---

## Concept Glossary

| Traditional term | Kubernetes/KubeSwift term | Description |
|---|---|---|
| Virtual switch | Linux bridge or OVN logical switch | L2 switching fabric |
| Port group | NetworkAttachmentDefinition (NAD) | Named network with VLAN/IPAM config |
| VLAN tag | VLAN sub-interface or OVN localnet `vlanID` | 802.1q VLAN identification |
| NIC team / bond | Linux bond | Link aggregation (LACP, active-backup) |
| VM NIC | `spec.interfaces[]` entry | Virtual network interface |
| Physical NIC | Host NIC (eno1, eno2, bond0) | Physical network adapter |
| Hypervisor | Cloud Hypervisor (primary) or QEMU (HGX SXM GPU only) | VM runtime |
| Guest agent | cloud-init + dnsmasq DHCP | Guest provisioning and IP discovery |
| Datastore | PersistentVolumeClaim (PVC) | VM disk storage |
| VM template | SwiftImage + SwiftGuestClass | Disk image + resource profile |
| Resource pool | SwiftGuestPool | Fleet of identical VMs |
| Host cluster | Kubernetes cluster | Compute infrastructure |
| SR-IOV passthrough | `type: sriov` interface | VFIO NIC passthrough to guest |
| GPU passthrough | `gpuProfileRef` on SwiftGuest | VFIO GPU passthrough to guest |
