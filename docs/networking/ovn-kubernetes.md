# OVN-Kubernetes Integration Guide

> **Don't need OVN-Kubernetes?** For simpler approaches that don't require
> OVN-Kubernetes (macvlan, bridge, vlan CNI), see the
> [Networking Operations Guide](operations-guide.md).

This guide covers how to use OVN-Kubernetes secondary networks with KubeSwift VMs.
It assumes you have read [Multi-NIC Support](../multi-nic.md) for the general
multi-interface architecture.

KubeSwift couples to the **Multus + NetworkAttachmentDefinition (NAD)** interface.
OVN-Kubernetes provides the network backend — KubeSwift does not import or depend
on any OVN-Kubernetes types. This means the same SwiftGuest manifests work regardless
of whether the secondary network is backed by OVN-Kubernetes, macvlan, or bridge CNI.

---

## Prerequisites

- **OVN-Kubernetes** as primary CNI or as secondary network provider
- **Multus CNI** installed and configured to delegate to OVN-Kubernetes
- **OVN-Kubernetes multi-network feature** enabled (default in OpenShift 4.14+,
  configurable in upstream OVN-Kubernetes via `enable-multi-network-policies`)
- For **localnet** topology: OVS bridge mappings configured on worker nodes
- For **UserDefinedNetwork**: OVN-Kubernetes v0.6.0+ or OpenShift 4.17+

---

## OVN-Kubernetes Network Topologies

OVN-Kubernetes provides three secondary network topologies via the `ovn-k8s-cni-overlay`
CNI plugin. Each maps to a different use case with KubeSwift VMs.

| Topology | L2/L3 | Cross-node | IPAM | Physical access | Use case |
|----------|-------|------------|------|-----------------|----------|
| `layer2` | L2 | Yes (Geneve) | OVN subnet | No | Storage, GPU data plane |
| `layer3` | L3 | Yes (routed) | OVN per-node subnet | No | Tenant data plane |
| `localnet` | L2 | Via physical | OVN or static | Yes (OVS bridge) | VLANs, legacy infra |

All three work with KubeSwift's bridge-to-tap model: OVN-Kubernetes creates an interface
in the pod namespace, `network-init.sh` bridges it to a tap device, and the hypervisor
connects the tap to a guest virtio-net NIC.

---

## 1. Storage Network Isolation

**Use case**: VMs need a dedicated network for Ceph, iSCSI, or NFS traffic — separate
from management and SSH traffic on the primary NIC.

**Why Layer 2**: Storage protocols like iSCSI and NFS often require L2 adjacency.
Layer 2 avoids routing overhead and keeps the storage path simple.

### Create the NAD

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: storage-net
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "storage-net",
      "type": "ovn-k8s-cni-overlay",
      "topology": "layer2",
      "subnets": "192.168.100.0/24",
      "netAttachDefName": "default/storage-net"
    }
```

`subnets` enables OVN-Kubernetes IPAM. Pods (and VMs) attached to this NAD get an IP
from 192.168.100.0/24 automatically. Omit `subnets` if you prefer static IPs via
cloud-init.

### Create the SwiftGuest

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: storage-vm
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: default
  seedProfileRef:
    name: storage-seed
  interfaces:
  - name: mgmt
  - name: storage
    networkRef:
      name: storage-net
  runPolicy: Running
```

### Configure the guest network

Use cloud-init `networkData` in the SwiftSeedProfile to configure the secondary
interface inside the guest:

```yaml
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: storage-seed
  namespace: default
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: storage-vm
    users:
      - name: ubuntu
        sudo: ALL=(ALL) NOPASSWD:ALL
        lock_passwd: false
  metaData: |
    instance-id: storage-vm-001
    local-hostname: storage-vm
  networkData: |
    version: 2
    ethernets:
      enp0s3:
        dhcp4: true
      enp0s4:
        dhcp4: true
```

If `subnets` is set on the NAD, OVN-Kubernetes assigns an IP to the pod-side
interface. Inside the guest, `dhcp4: true` on the secondary NIC picks up this IP
via the bridge. If you omit `subnets` from the NAD, use static addressing instead:

```yaml
  networkData: |
    version: 2
    ethernets:
      enp0s3:
        dhcp4: true
      enp0s4:
        addresses:
          - 192.168.100.10/24
```

See [config/samples/multi-nic/swiftguest-storage-isolation.yaml](../../config/samples/multi-nic/swiftguest-storage-isolation.yaml)
for a complete example.

---

## 2. GPU Data Plane Separation

**Use case**: Management traffic (SSH, monitoring, cloud-init) on the primary NIC.
GPU inter-node communication (NCCL, GPUDirect RDMA) on a dedicated high-bandwidth
secondary network.

**Why separate networks**: NCCL collective operations generate sustained high-bandwidth
traffic. Mixing this with management traffic on the same NIC causes head-of-line
blocking and degrades both SSH responsiveness and training throughput.

### Create the NAD

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: gpu-data-net
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "gpu-data-net",
      "type": "ovn-k8s-cni-overlay",
      "topology": "layer2",
      "subnets": "10.200.0.0/16",
      "netAttachDefName": "default/gpu-data-net"
    }
```

A /16 subnet provides room for many GPU VMs. Layer 2 is used because NCCL performs
better without routing hops.

### Create the SwiftGuest with GPU + data NIC

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: gpu-training-vm
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble
  gpuProfileRef:
    name: h200-hgx-4gpu
  guestClassRef:
    name: default
  seedProfileRef:
    name: gpu-seed
  interfaces:
  - name: mgmt
  - name: gpu-data
    networkRef:
      name: gpu-data-net
  runPolicy: Running
```

This combines GPU passthrough (via `gpuProfileRef`) with a dedicated data plane NIC.
The management NIC handles SSH and cloud-init; the gpu-data NIC handles NCCL traffic.

> **Note**: For true RDMA performance, SR-IOV NIC passthrough (Phase C) is recommended
> over OVN overlay networks. The OVN-Kubernetes overlay adds encapsulation overhead
> (Geneve) that reduces effective bandwidth. For training workloads where every Gbps
> matters, SR-IOV gives direct hardware access with no overhead.

See [config/samples/multi-nic/swiftguest-gpu-data-separation.yaml](../../config/samples/multi-nic/swiftguest-gpu-data-separation.yaml)
for a complete example.

---

## 3. VLAN Segmentation

**Use case**: VMs need to be placed on specific VLANs to reach legacy infrastructure,
bare-metal servers, or external networks that are not part of the Kubernetes overlay.

**Why localnet**: The `localnet` topology bridges OVN logical switches to physical
networks via OVS. This is the only OVN-Kubernetes topology that provides direct
access to physical infrastructure.

### Step 1: Configure OVS bridge mappings

Each worker node needs an OVS bridge connected to the physical NIC that carries
the target VLAN. Use NodeNetworkConfigurationPolicy (from nmstate) or configure
manually.

**Using NodeNetworkConfigurationPolicy (recommended)**:

```yaml
apiVersion: nmstate.io/v1
kind: NodeNetworkConfigurationPolicy
metadata:
  name: br-data-mapping
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: ''
  desiredState:
    ovn:
      bridge-mappings:
      - localnet: data-physnet
        bridge: br-data
        state: present
    interfaces:
    - name: br-data
      type: ovs-bridge
      state: up
      bridge:
        port:
        - name: eno2
```

This creates an OVS bridge `br-data` with port `eno2`, and maps it to the OVN
localnet name `data-physnet`.

**Manual OVS configuration** (if nmstate is not available):

```bash
ovs-vsctl add-br br-data
ovs-vsctl add-port br-data eno2
ovs-vsctl set open . external-ids:ovn-bridge-mappings="data-physnet:br-data"
```

### Step 2: Create the localnet NAD

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan100-net
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "data-physnet",
      "type": "ovn-k8s-cni-overlay",
      "topology": "localnet",
      "vlanID": 100,
      "netAttachDefName": "default/vlan100-net"
    }
```

The `name` field in the config must match the localnet name in the OVS bridge mapping
(`data-physnet`). The `vlanID` causes OVN to tag traffic with VLAN 100.

### Step 3: Create the SwiftGuest

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: vlan-vm
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: default
  seedProfileRef:
    name: vlan-seed
  interfaces:
  - name: mgmt
  - name: legacy
    networkRef:
      name: vlan100-net
  runPolicy: Running
```

The guest's secondary NIC (`legacy`) is on VLAN 100 of the physical network connected
to `eno2`. The VM can reach bare-metal servers, switches, or any infrastructure on
that VLAN.

See [config/samples/multi-nic/swiftguest-vlan.yaml](../../config/samples/multi-nic/swiftguest-vlan.yaml)
and [config/samples/multi-nic/nad-ovn-localnet.yaml](../../config/samples/multi-nic/nad-ovn-localnet.yaml)
for complete examples.

---

## 4. Tenant Isolation with UserDefinedNetwork

**Use case**: Multi-tenant GPU cloud where each tenant's VMs must be isolated from
other tenants, even if they run on the same physical node.

**UserDefinedNetwork (UDN)** is an OVN-Kubernetes native CRD that creates network
infrastructure (including NADs) automatically. It provides stronger isolation than
manually-created NADs because OVN enforces the isolation at the logical switch level.

### Create a tenant namespace

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: tenant-alpha
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
```

The label `k8s.ovn.org/primary-user-defined-network` enables UDN as the primary
network for pods in this namespace.

### Create the UDN

```yaml
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: tenant-alpha-net
  namespace: tenant-alpha
spec:
  topology: Layer3
  layer3:
    role: Primary
    subnets:
    - cidr: 10.100.0.0/16
      hostSubnet: 24
```

This creates a Layer 3 network with per-node /24 subnets carved from 10.100.0.0/16.
OVN-Kubernetes handles IPAM and inter-node routing.

When `role: Primary`, this UDN replaces the default cluster network for the namespace.
KubeSwift's primary NIC (the interface without `networkRef`) uses this network instead
of the default pod network. This means:

- The management NIC gets an IP from the UDN's subnet (10.100.x.x)
- dnsmasq still runs on the bridge — DHCP works as normal
- SSH access is via the UDN network (routing must be configured to reach it)

### Isolation guarantee

VMs in `tenant-alpha` cannot communicate with VMs in `tenant-beta` — even if both
run on the same node. OVN enforces this at the logical switch level. There is no
shared L2 broadcast domain.

> **Note**: UDN is an OVN-Kubernetes specific feature. It is not available with
> Calico or Cilium.

See [config/samples/multi-nic/udn-tenant-isolation.yaml](../../config/samples/multi-nic/udn-tenant-isolation.yaml)
for a complete example.

---

## 5. Shared Network with ClusterUserDefinedNetwork

**Use case**: GPU training cluster where multiple team namespaces need a shared
data plane network for cross-namespace GPU communication, while still maintaining
isolation from other tenants.

**ClusterUserDefinedNetwork (CUDN)** is the cluster-scoped variant of UDN. It is
created by cluster administrators and spans multiple namespaces via a selector.

### Create the CUDN

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: shared-gpu-net
spec:
  namespaceSelector:
    matchExpressions:
    - key: kubernetes.io/metadata.name
      operator: In
      values: ["team-ml", "team-infra"]
  network:
    topology: Layer2
    layer2:
      role: Secondary
      subnets: ["10.200.0.0/16"]
```

This creates a Layer 2 secondary network shared between the `team-ml` and `team-infra`
namespaces. OVN-Kubernetes creates a NAD automatically in each selected namespace.

### Use from SwiftGuest

The auto-created NAD has the same name as the CUDN (`shared-gpu-net`). Reference it
from the SwiftGuest in either namespace:

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: training-vm
  namespace: team-ml
spec:
  imageRef:
    name: ubuntu-noble
  gpuProfileRef:
    name: h200-hgx-4gpu
  guestClassRef:
    name: default
  seedProfileRef:
    name: gpu-seed
  interfaces:
  - name: mgmt
  - name: gpu-data
    networkRef:
      name: shared-gpu-net
  runPolicy: Running
```

VMs in `team-ml` and `team-infra` can communicate over the 10.200.0.0/16 network.
VMs in namespaces not selected by the CUDN cannot reach this network.

CUDN is the only way to use `localnet` topology with the UDN CRD model. For localnet
CUDNs, specify `topology: Localnet` with bridge mapping references.

See [config/samples/multi-nic/cudn-shared-network.yaml](../../config/samples/multi-nic/cudn-shared-network.yaml)
for a complete example.

---

## 6. Telco / NFV Considerations

KubeSwift + OVN-Kubernetes can address some NFV requirements, but the tap+bridge
networking model has inherent overhead compared to direct hardware access.

### What works well

- **Management plane**: Primary NIC on default or UDN network
- **Data plane via localnet**: VMs on VLANs connected to physical infrastructure
- **Service chaining**: Multiple secondary NICs for network function chains (firewall,
  load balancer, NAT) where each NIC connects to a different network segment

### Limitations for high-performance NFV

- **DPDK**: OVN-Kubernetes supports OVS-DPDK for userspace datapath, but KubeSwift's
  tap+bridge model adds a kernel-space hop. For line-rate packet processing, SR-IOV
  passthrough (Phase C) is the recommended path.

- **VPP**: Similar to DPDK — VPP workloads inside VMs benefit from direct hardware
  access. The tap+bridge model works for functional testing but not for production
  line-rate performance.

- **Recommendation**: For NFV workloads requiring deterministic latency or line-rate
  throughput, plan for SR-IOV NIC passthrough when it becomes available in KubeSwift.
  Use OVN-Kubernetes secondary networks for control plane, management, and moderate-
  bandwidth data paths.

---

## CNI Compatibility Matrix

This table covers **secondary network** capabilities when used with Multus. The
primary CNI always provides the default pod network.

| Feature | OVN-Kubernetes | Calico | Cilium | macvlan | bridge |
|---------|---------------|--------|--------|---------|--------|
| Layer 2 secondary | Yes | -- | -- | Yes | Yes |
| Layer 3 secondary | Yes | -- | Yes | -- | -- |
| Localnet (physical) | Yes | -- | -- | Yes | -- |
| VLAN tagging | Yes | -- | -- | Yes | Yes |
| UDN tenant isolation | Yes | -- | -- | -- | -- |
| Network policies on secondary | Yes | -- | Yes | -- | -- |
| IPAM | Yes | Yes | Yes | DHCP/static | DHCP/static |
| SR-IOV | via NAD | via NAD | via NAD | -- | -- |

"--" means not supported as a Multus secondary network via that CNI plugin.

---

## Troubleshooting

### VM secondary NIC has no IP

- Check if the NAD has `subnets` defined. Without it, OVN-Kubernetes does not
  assign IPs — use static config via cloud-init `networkData`.
- Verify the NAD exists: `kubectl get net-attach-def <name> -n <namespace>`.
- Check pod events: `kubectl describe pod <guest-name>`. Multus errors appear
  as pod events.

### Localnet interface has no connectivity

- Verify OVS bridge mapping: `ovs-vsctl get open . external-ids:ovn-bridge-mappings`.
- Verify the physical NIC is a port on the OVS bridge: `ovs-vsctl list-ports br-data`.
- Verify the `name` in the NAD config matches the localnet name in the bridge mapping.
- Check VLAN ID is correct and the upstream switch port is configured for that VLAN.

### UDN namespace has no network

- Verify the namespace has the label `k8s.ovn.org/primary-user-defined-network: ""`.
- Verify the UDN is created in the correct namespace: `kubectl get udn -n <namespace>`.
- Check OVN-Kubernetes operator logs for errors.

### VM boots but secondary NIC is missing inside guest

- Check the Multus annotation on the pod: `kubectl get pod <name> -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks}'`.
- Check network-init container logs: `kubectl logs <pod> -c network-init`.
- Verify the interface order matches: first interface in `spec.interfaces` = enp0s3,
  second = enp0s4, etc.

### Cross-node VM connectivity fails on Layer 2

- OVN-Kubernetes Layer 2 uses Geneve encapsulation between nodes. Verify Geneve
  port (UDP 6081) is open in any firewalls between nodes.
- Check OVN southbound database for logical switch port bindings:
  `ovn-sbctl show | grep <pod-name>`.
