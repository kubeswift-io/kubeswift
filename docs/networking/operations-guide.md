# Networking Operations Guide

This guide covers how to connect KubeSwift VMs to physical networks, VLANs, bonds,
and isolated virtual networks. It is self-contained -- you do not need to read
Multus, CNI plugin, or OVN-Kubernetes documentation to follow these instructions.

> **Coming from VMware or Proxmox?** See the
> [Virtualization Platform Comparison](virtualization-comparison.md) for a concept
> mapping between your current platform and KubeSwift.

---

## How KubeSwift Networking Works

Every KubeSwift VM gets a **primary NIC** automatically. This NIC provides:
- DHCP IP address (from KubeSwift's built-in dnsmasq)
- Default gateway and NAT for outbound internet access
- SSH access via `swiftctl ssh`
- Cloud-init network bootstrap

No configuration is needed for the primary NIC. It works out of the box.

**Secondary NICs** connect VMs to additional networks -- physical NICs, VLANs,
storage networks, or isolated segments. Secondary NICs require:

1. **Multus CNI** installed in the cluster
2. A **NetworkAttachmentDefinition (NAD)** describing the network
3. An entry in `spec.interfaces[]` on the SwiftGuest referencing the NAD

A NAD is the Kubernetes equivalent of a VMware port group or Proxmox bridge
configuration. It defines which physical network, VLAN, or virtual segment the
interface connects to.

KubeSwift is **CNI-agnostic** -- it works with any CNI plugin that integrates with
Multus: macvlan, bridge, vlan, ipvlan, OVN-Kubernetes, Calico, Cilium, or SR-IOV.

### Data path

```
Guest VM
  |  virtio-net NIC
  |
 tap device (tap0, tap1, ...)
  |
 Linux bridge (br0, br1, ...)
  |
 Multus-created interface (net1, net2, ...)
  |
 CNI plugin (macvlan, bridge, OVN-K, ...)
  |
 Physical NIC / overlay tunnel / virtual switch
```

Traffic from the guest flows: virtio-net -> tap -> bridge -> Multus interface ->
CNI plugin -> network. The bridge is a simple L2 switch -- no NAT, no DHCP, no L3
processing on secondary NICs (those features are only on the primary NIC).

---

## Prerequisites

### Install Multus

Multus is only needed for secondary NICs. Skip this if your VMs need only the
primary management NIC.

```bash
# Install Multus (thick plugin)
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml
```

Verify Multus is running:

```bash
kubectl get pods -n kube-system -l app=multus
kubectl get crd network-attachment-definitions.k8s.cni.cncf.io
```

### CNI plugins

Most CNI plugins are included with the standard CNI plugins package
(`containernetworking/plugins`) -- macvlan, bridge, vlan, ipvlan, and host-device
are all available by default on most Kubernetes distributions. Verify on a worker
node:

```bash
ls /opt/cni/bin/macvlan /opt/cni/bin/bridge /opt/cni/bin/vlan
```

If missing, install the standard CNI plugins:

```bash
# Download from https://github.com/containernetworking/plugins/releases
CNI_VERSION=v1.5.1
curl -LO https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-linux-amd64-${CNI_VERSION}.tgz
sudo tar -C /opt/cni/bin -xzf cni-plugins-linux-amd64-${CNI_VERSION}.tgz
```

---

## Section 1: VMs on a Dedicated Physical Network

**When to use:** Worker nodes have multiple NICs. You want VMs to communicate on a
specific physical network (storage, compute, management) without routing through
the Kubernetes pod network.

**Analogy:** VMware vSwitch connected to a physical NIC with a port group. Or Proxmox
bridge bound to a physical interface.

### Host preparation

None required. macvlan creates virtual interfaces directly on the physical NIC --
no bridge, no VLAN sub-interface, no OVS needed.

Verify the NIC is up on each worker node:

```bash
ip link show eno2
# Should show: state UP
```

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
      "type": "macvlan",
      "master": "eno2",
      "mode": "bridge",
      "ipam": {
        "type": "host-local",
        "subnet": "192.168.100.0/24",
        "rangeStart": "192.168.100.100",
        "rangeEnd": "192.168.100.200",
        "gateway": "192.168.100.1"
      }
    }
```

**Field reference:**

| Field | Description |
|-------|-------------|
| `type: macvlan` | Creates virtual interfaces on the physical NIC |
| `master: eno2` | The physical NIC to attach to |
| `mode: bridge` | macvlan bridge mode -- VMs can talk to each other and to external devices on eno2's network |
| `ipam.type: host-local` | Kubernetes-managed IP allocation from a static range |

**macvlan modes:**

| Mode | VM-to-VM (same node) | VM-to-external | Use case |
|------|---------------------|---------------|----------|
| `bridge` | Yes | Yes | Most common -- general purpose |
| `private` | No | Yes | Isolation between VMs on same node |
| `vepa` | Via switch | Yes | Requires VEPA-capable upstream switch |
| `passthru` | N/A | Yes | One VM per master interface (exclusive access) |

**IPAM options:**

| Type | Description | When to use |
|------|-------------|-------------|
| `host-local` | Static range managed by Kubernetes | Default choice -- simple and reliable |
| `dhcp` | Uses the network's existing DHCP server | When the physical network has its own DHCP |
| `static` | No IPAM -- set IP in cloud-init | When you control IPs via cloud-init |

> **DHCP IPAM note:** The `dhcp` IPAM type requires the DHCP CNI daemon running on
> each node. Install it via:
> ```bash
> # As a systemd service on each node
> sudo cp /opt/cni/bin/dhcp /usr/local/bin/cni-dhcp-daemon
> sudo systemctl enable --now cni-dhcp-daemon
> ```

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

### Configure guest networking

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
        ssh_authorized_keys:
          - ssh-ed25519 AAAA...
  networkData: |
    version: 2
    ethernets:
      enp0s3:
        dhcp4: true
      enp0s4:
        addresses:
          - 192.168.100.150/24
        routes:
          - to: 192.168.100.0/24
            via: 192.168.100.1
```

**Interface naming:**
- `enp0s3` -- primary NIC (KubeSwift management, DHCP from dnsmasq)
- `enp0s4` -- first secondary NIC (storage network)
- `enp0s5` -- second secondary NIC (if present)
- Pattern: first in `interfaces[]` = `enp0s3`, second = `enp0s4`, third = `enp0s5`

If using `host-local` IPAM, the CNI assigns an IP to the pod-side interface. Inside
the guest, set a static IP in the same subnet, or use `dhcp4: true` to pick up the
address via the bridge.

### Verification

```bash
# Check the NAD exists
kubectl get net-attach-def storage-net

# Check the pod has the Multus annotation
kubectl get pod storage-vm -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks}'

# Check network-init created the secondary bridge
kubectl logs storage-vm -c network-init

# Inside the guest
swiftctl ssh storage-vm
ip addr show enp0s4
ping 192.168.100.1
```

### Multiple physical NICs

If nodes have three or more NICs, create one NAD per NIC and reference them all:

```yaml
interfaces:
- name: mgmt
- name: storage
  networkRef:
    name: storage-net        # macvlan on eno2
- name: compute
  networkRef:
    name: compute-net        # macvlan on eno3
- name: backup
  networkRef:
    name: backup-net         # macvlan on eno4
```

> **macvlan host limitation:** The host cannot communicate with its own macvlan
> sub-interfaces. This is a Linux kernel limitation, not a KubeSwift issue. If the
> host needs to reach VMs on the macvlan network, use a separate NIC or a Linux
> bridge (Section 1, Pattern 4 below) instead.

---

## Section 2: VMs on Specific VLANs

**When to use:** Worker nodes have a NIC or bond in trunk mode carrying multiple VLANs.
You want to place different VMs (or different NICs of the same VM) on different VLANs.

**Analogy:** VMware distributed vSwitch with VLAN-tagged port groups. Or Proxmox bridge
with VLAN-aware mode where you assign VLAN tags per VM NIC.

Three approaches, from simplest to most powerful:

### Approach A: Pre-created VLAN sub-interfaces + macvlan

The simplest approach. No OVN-Kubernetes required.

#### First step: Host preparation

Create VLAN sub-interfaces on each worker node via netplan, nmcli, or a DaemonSet.

**Example 1: Netplan configuration (Ubuntu):**

```yaml
# /etc/netplan/60-vlans.yaml
network:
  version: 2
  vlans:
    bond0.100:
      id: 100
      link: bond0
    bond0.200:
      id: 200
      link: bond0
    bond0.300:
      id: 300
      link: bond0
```

```bash
sudo netplan apply
ip link show bond0.100  # verify
```

**Example 2: NetworkManager CLI (RHEL/Rocky):**

```bash
nmcli connection add type vlan ifname bond0.100 dev bond0 id 100
nmcli connection add type vlan ifname bond0.200 dev bond0 id 200
nmcli connection up vlan-bond0.100
nmcli connection up vlan-bond0.200
```

**Example 3: DaemonSet (hostNetwork, all distros; example only, no teardown):**

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: vlan-setup
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: vlan-setup
  template:
    metadata:
      labels:
        app: vlan-setup
    spec:
      hostNetwork: true
      containers:
      - name: vlan-setup
        image: ubuntu:22.04
        command: ["/bin/sh", "-c"]
        args:
        - |
          ip link add link bond0 name bond0.100 type vlan id 100 2>/dev/null || true
          ip link set bond0.100 up
          ip link add link bond0 name bond0.200 type vlan id 200 2>/dev/null || true
          ip link set bond0.200 up
          ip link add link bond0 name bond0.300 type vlan id 300 2>/dev/null || true
          ip link set bond0.300 up
          sleep infinity
        securityContext:
          capabilities:
            add: ["NET_ADMIN"]
```

#### Second step: Create NetworkAttachmentDefinitions (NADs) per VLAN

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan100-mgmt
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "vlan100-mgmt",
      "type": "macvlan",
      "master": "bond0.100",
      "mode": "bridge",
      "ipam": {
        "type": "host-local",
        "subnet": "10.100.0.0/24",
        "rangeStart": "10.100.0.100",
        "rangeEnd": "10.100.0.200"
      }
    }
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan200-storage
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "vlan200-storage",
      "type": "macvlan",
      "master": "bond0.200",
      "mode": "bridge",
      "ipam": {
        "type": "host-local",
        "subnet": "10.200.0.0/24",
        "rangeStart": "10.200.0.100",
        "rangeEnd": "10.200.0.200"
      }
    }
```

#### Last step: Set the interface in the SwiftGuest

```yaml
interfaces:
- name: mgmt
- name: management-vlan
  networkRef:
    name: vlan100-mgmt
- name: storage-vlan
  networkRef:
    name: vlan200-storage
```

### Approach B: vlan CNI plugin (auto-creates sub-interfaces)

No host preparation needed -- the vlan CNI plugin creates VLAN sub-interfaces
inside the pod's network namespace on demand.

#### First step: Verify the vlan CNI plugin

Verify the vlan CNI plugin binary exists on worker nodes:

```bash
ls /opt/cni/bin/vlan
```

#### Second step: Create NetworkAttachmentDefinitions (NADs) per VLAN

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan100
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "vlan100",
      "type": "vlan",
      "master": "bond0",
      "vlanId": 100,
      "ipam": {
        "type": "host-local",
        "subnet": "10.100.0.0/24"
      }
    }
```

The `vlan` CNI plugin creates a VLAN sub-interface inside the pod's network
namespace, not on the host. The sub-interface is per-pod and cleaned up automatically
when the pod exits.

#### Last step: Set the interface in the SwiftGuest

This is the same as previous example.

### Approach C: OVN-Kubernetes localnet

The most powerful approach. Requires OVN-Kubernetes as the CNI. See the
[OVN-Kubernetes Integration Guide](ovn-kubernetes.md) Section 3 for full details.

OVN handles VLAN tagging declaratively. Multiple NADs can specify different `vlanID`
values on the same localnet. OVN provides IPAM, network policies, and centralized
management.

### Choosing an approach

| Approach | Host prep | OVN-K required | IPAM | Network policies | VLAN config scope |
|----------|-----------|----------------|------|-----------------|-------------------|
| A: macvlan + sub-interface | Yes (netplan/nmcli) | No | host-local or DHCP | No | Per-node |
| B: vlan CNI | No | No | host-local | No | Per-pod (auto) |
| C: OVN-K localnet | Yes (OVS bridge) | Yes | OVN IPAM | Yes | Cluster-wide |

**Recommendation:** Start with Approach A or B for simplicity. Move to C when you
need centralized IPAM, network policies on secondary networks, or cluster-wide
management.

### Complete VLAN example -- VM with management + storage + application VLANs

Full end-to-end walkthrough using Approach A.

**1. Host preparation (on each worker node):**

```yaml
# /etc/netplan/60-vlans.yaml
network:
  version: 2
  vlans:
    bond0.100:
      id: 100
      link: bond0
    bond0.200:
      id: 200
      link: bond0
    bond0.300:
      id: 300
      link: bond0
```

```bash
sudo netplan apply
```

**2. Three NADs:**

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan100-mgmt
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "vlan100-mgmt",
      "type": "macvlan",
      "master": "bond0.100",
      "mode": "bridge",
      "ipam": {
        "type": "host-local",
        "subnet": "10.100.0.0/24",
        "rangeStart": "10.100.0.100",
        "rangeEnd": "10.100.0.200"
      }
    }
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan200-storage
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "vlan200-storage",
      "type": "macvlan",
      "master": "bond0.200",
      "mode": "bridge",
      "ipam": {
        "type": "host-local",
        "subnet": "10.200.0.0/24",
        "rangeStart": "10.200.0.100",
        "rangeEnd": "10.200.0.200"
      }
    }
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: vlan300-app
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "vlan300-app",
      "type": "macvlan",
      "master": "bond0.300",
      "mode": "bridge",
      "ipam": {
        "type": "host-local",
        "subnet": "10.30.0.0/24",
        "rangeStart": "10.30.0.100",
        "rangeEnd": "10.30.0.200"
      }
    }
```

**3. SwiftSeedProfile with networkData for all interfaces:**

```yaml
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: multi-vlan-seed
  namespace: default
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: multi-vlan-vm
    users:
      - name: ubuntu
        sudo: ALL=(ALL) NOPASSWD:ALL
        lock_passwd: false
        ssh_authorized_keys:
          - ssh-ed25519 AAAA...
  networkData: |
    version: 2
    ethernets:
      enp0s3:
        dhcp4: true
      enp0s4:
        addresses:
          - 10.100.0.150/24
      enp0s5:
        addresses:
          - 10.200.0.150/24
      enp0s6:
        addresses:
          - 10.30.0.150/24
```

**4. SwiftGuest with four interfaces:**

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: multi-vlan-vm
  namespace: default
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: default
  seedProfileRef:
    name: multi-vlan-seed
  interfaces:
  - name: mgmt
  - name: vlan100
    networkRef:
      name: vlan100-mgmt
  - name: vlan200
    networkRef:
      name: vlan200-storage
  - name: vlan300
    networkRef:
      name: vlan300-app
  runPolicy: Running
```

**5. Verification:**

```bash
kubectl get swiftguest multi-vlan-vm -w
# Wait for Running

swiftctl ssh multi-vlan-vm
ip addr show                    # Should show 4 interfaces
ping 10.100.0.1                 # VLAN 100 gateway
ping 10.200.0.1                 # VLAN 200 gateway
```

---

## Section 3: Isolated Virtual Networks (No Physical NIC)

**When to use:** VMs need to communicate with each other on a private network
that doesn't exist on any physical interface. Test environments, dev clusters,
application-tier segmentation.

**Analogy:** VMware host-only network. Or Proxmox internal bridge with no physical
port.

### Approach A: bridge CNI (same-node only)

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: internal-net
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "internal-net",
      "type": "bridge",
      "bridge": "br-internal",
      "isGateway": false,
      "ipam": {
        "type": "host-local",
        "subnet": "172.16.0.0/24"
      }
    }
```

| Field | Description |
|-------|-------------|
| `bridge: br-internal` | The bridge CNI creates this Linux bridge on the node if it doesn't exist |
| `isGateway: false` | The bridge has no IP -- VMs can only talk to each other |

**Limitation:** VMs on different nodes cannot communicate. The bridge is node-local.

### Approach B: OVN-Kubernetes Layer 2 (cross-node)

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: internal-overlay
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "internal-overlay",
      "type": "ovn-k8s-cni-overlay",
      "topology": "layer2",
      "subnets": "172.16.0.0/24",
      "netAttachDefName": "default/internal-overlay"
    }
```

VMs on different nodes can communicate via Geneve tunnels. No physical NIC or bridge
configuration needed. See [OVN-Kubernetes Integration Guide](ovn-kubernetes.md)
Section 1 for full details.

### Choosing an approach

| Approach | Cross-node | Requires OVN-K | Host prep | Performance |
|----------|-----------|----------------|-----------|-------------|
| bridge CNI | No | No | None | Native (no encapsulation) |
| OVN-K Layer 2 | Yes | Yes | None | Geneve overhead (~50 bytes/packet, ~5-10%) |

---

## Section 4: Routed Networks Between VMs

For routed (L3) connectivity between VMs across nodes, use OVN-Kubernetes Layer 3
topology. This provides per-node subnets with routing between nodes.

See [OVN-Kubernetes Integration Guide](ovn-kubernetes.md) for full details.

---

## Section 5: Tenant Isolation

For multi-tenant isolation where VMs in one namespace cannot communicate with VMs
in another, use OVN-Kubernetes UserDefinedNetwork (UDN) or ClusterUserDefinedNetwork
(CUDN).

See [OVN-Kubernetes Integration Guide](ovn-kubernetes.md) Sections 4 and 5.

---

## Section 6: Hardware NIC Passthrough (SR-IOV)

For maximum performance (GPUDirect RDMA, DPDK, line-rate NFV), use SR-IOV NIC
passthrough. The guest sees a real hardware NIC via VFIO -- no virtio-net, no tap,
no bridge overhead.

See [SR-IOV NIC Passthrough](sriov.md) for setup and configuration.

---

## Section 7: ipvlan -- Not Supported

> **ipvlan is incompatible with KubeSwift.** ipvlan interfaces cannot be added to
> a Linux bridge -- the kernel rejects `ip link set <ipvlan-dev> master <bridge>`.
> Since KubeSwift's networking model bridges every Multus interface to a tap device,
> ipvlan cannot work.

**Use macvlan instead.** macvlan covers the same use cases (direct physical NIC
access, multiple virtual interfaces per master) and is fully compatible with the
bridge-to-tap model.

If your upstream switch limits MAC addresses per port (port security), consider
using the bridge CNI with a pre-created host bridge (Section 1, Pattern 4) instead
of macvlan. This uses a single veth MAC per port rather than a per-VM MAC.

---

## Section 8: Host Preparation Reference

### Bond configuration

**Ubuntu (netplan):**

```yaml
# /etc/netplan/50-bond.yaml
network:
  version: 2
  ethernets:
    eno1:
      dhcp4: false
    eno2:
      dhcp4: false
  bonds:
    bond0:
      interfaces: [eno1, eno2]
      parameters:
        mode: 802.3ad
        lacp-rate: fast
        mii-monitor-interval: 100
      addresses:
        - 10.0.0.10/24
      routes:
        - to: default
          via: 10.0.0.1
```

```bash
sudo netplan apply
cat /proc/net/bonding/bond0  # verify
```

**RHEL/Rocky (nmcli):**

```bash
nmcli connection add type bond ifname bond0 bond.options "mode=802.3ad,miimon=100"
nmcli connection add type ethernet ifname eno1 master bond0
nmcli connection add type ethernet ifname eno2 master bond0
nmcli connection modify bond0 ipv4.addresses 10.0.0.10/24 ipv4.gateway 10.0.0.1 ipv4.method manual
nmcli connection up bond0
```

**Bond modes:**

| Mode | Name | Use case |
|------|------|----------|
| 0 | balance-rr | Round-robin -- simple, no switch config |
| 1 | active-backup | Failover only -- most compatible |
| 2 | balance-xor | XOR hash -- requires static EtherChannel |
| 4 | 802.3ad | LACP -- highest throughput, requires LACP-capable switch |
| 5 | balance-tlb | Adaptive transmit load balancing |
| 6 | balance-alb | Adaptive load balancing (TX + RX) |

For production, use **802.3ad (LACP)** for maximum throughput or **active-backup**
for maximum compatibility.

### VLAN sub-interfaces

**Ubuntu (netplan):**

```yaml
# /etc/netplan/60-vlans.yaml
network:
  version: 2
  vlans:
    bond0.100:
      id: 100
      link: bond0
    bond0.200:
      id: 200
      link: bond0
```

```bash
sudo netplan apply
```

**RHEL/Rocky (nmcli):**

```bash
nmcli connection add type vlan ifname bond0.100 dev bond0 id 100
nmcli connection add type vlan ifname bond0.200 dev bond0 id 200
```

### Linux bridge (for bridge CNI)

**Manual:**

```bash
ip link add br-storage type bridge
ip link set eno2 master br-storage
ip link set br-storage up
```

**Ubuntu (netplan):**

```yaml
network:
  version: 2
  bridges:
    br-storage:
      interfaces: [eno2]
      parameters:
        stp: false
```

### OVS bridge (for OVN-K localnet)

```bash
ovs-vsctl add-br br-data
ovs-vsctl add-port br-data bond0
ovs-vsctl set open . external-ids:ovn-bridge-mappings="physnet1:br-data"
```

### Persisting configuration across reboots

| Method | Persistent | Notes |
|--------|-----------|-------|
| netplan | Yes | Configs in `/etc/netplan/` survive reboots |
| nmcli | Yes | Connections stored in `/etc/NetworkManager/system-connections/` |
| `ip` commands | No | Lost on reboot -- use for testing only |
| OVS | Yes | Stored in OVSDB, persistent by default |

---

## Section 9: Cloud-Init Network Configuration Reference

Guest network configuration is set via the SwiftSeedProfile `networkData` field,
which uses netplan v2 syntax.

### Single secondary NIC (DHCP)

```yaml
networkData: |
  version: 2
  ethernets:
    enp0s3:
      dhcp4: true
    enp0s4:
      dhcp4: true
```

### Single secondary NIC (static)

```yaml
networkData: |
  version: 2
  ethernets:
    enp0s3:
      dhcp4: true
    enp0s4:
      addresses:
        - 192.168.100.50/24
      routes:
        - to: 192.168.100.0/24
          via: 192.168.100.1
```

### Multiple secondary NICs (mixed)

```yaml
networkData: |
  version: 2
  ethernets:
    enp0s3:
      dhcp4: true
    enp0s4:
      addresses:
        - 10.100.0.50/24
    enp0s5:
      addresses:
        - 10.200.0.50/24
    enp0s6:
      dhcp4: true
```

### Guest interface naming convention

| Position in `spec.interfaces[]` | Ubuntu (predictable) | Rocky (net.ifnames=0) |
|--------------------------------|---------------------|----------------------|
| First (index 0) | `enp0s3` | `eth0` |
| Second (index 1) | `enp0s4` | `eth1` |
| Third (index 2) | `enp0s5` | `eth2` |
| Fourth (index 3) | `enp0s6` | `eth3` |

Naming is based on PCI bus position -- virtio-net devices appear on PCI slot 3, 4, 5, etc.

### Distro-agnostic configuration

Use `match:` for configuration that works on both Ubuntu and Rocky/RHEL:

```yaml
networkData: |
  version: 2
  ethernets:
    primary:
      match:
        name: "en*3"
      dhcp4: true
    secondary:
      match:
        name: "en*4"
      addresses:
        - 192.168.100.50/24
```

---

## Section 10: Troubleshooting

### VM secondary NIC has no IP

1. Check NAD exists: `kubectl get net-attach-def <name>`
2. Check pod Multus annotation:
   ```bash
   kubectl get pod <name> -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks}'
   ```
3. Check network-init logs: `kubectl logs <pod> -c network-init`
4. Check cloud-init inside guest:
   ```bash
   swiftctl ssh <guest>
   cloud-init status --long
   cat /etc/netplan/*.yaml
   ```
5. If using DHCP IPAM: verify the DHCP CNI daemon is running on the node

### VM can't reach devices on the physical network

1. Verify the physical NIC is UP: `ip link show <nic>` on the node
2. Verify the macvlan master is correct: check `master` in NAD config
3. **macvlan host limitation:** The host cannot communicate with its own macvlan
   sub-interfaces. This is a Linux kernel limitation. Use a separate NIC or bridge
   for host-to-VM traffic.
4. Check iptables/nftables rules on the node that might block traffic

### VLAN traffic not tagged correctly

1. Verify the VLAN sub-interface exists: `ip link show bond0.100` on the node
2. Verify the upstream switch port is configured as a trunk allowing the VLAN
3. For OVN-K localnet: verify OVS bridge mapping and `vlanID` in NAD config
4. Capture traffic: `tcpdump -i bond0 -e vlan` to verify 802.1q tags

### VM boots but secondary NIC is missing inside guest

1. Check Multus annotation on the pod:
   ```bash
   kubectl get pod <name> -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks}'
   ```
2. Check network-init container logs: `kubectl logs <pod> -c network-init`
3. Verify interface order: first in `spec.interfaces` = `enp0s3`, second = `enp0s4`
4. Inside guest: `lspci | grep -i virtio` to verify virtio-net devices

### Performance considerations

| Network type | Overhead | Latency | When to use |
|-------------|----------|---------|-------------|
| macvlan | Near-native | Sub-ms | Default choice for physical network access |
| bridge CNI | One extra bridge hop | Sub-ms | Virtual switches, host bridge access |
| OVN-K overlay | ~50 bytes/packet | +0.1-0.5ms | Cross-node virtual networks |
| SR-IOV | None (hardware) | Native | GPUDirect RDMA, DPDK, line-rate NFV |
