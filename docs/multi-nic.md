# Multi-NIC Support

KubeSwift supports multiple network interfaces per VM via Multus CNI integration.
This enables use cases like GPU data plane separation (RDMA + management), storage
isolation (dedicated Ceph/iSCSI NIC), and VLAN segmentation.

## Concepts

### Primary vs Secondary Interfaces

Every SwiftGuest has a **primary NIC** that uses KubeSwift's built-in tap+bridge+dnsmasq
networking. This is the management interface — DHCP, IP discovery, cloud-init, SSH.

**Secondary NICs** are backed by Multus NetworkAttachmentDefinitions (NADs). The CNI
plugin behind the NAD handles IP assignment, not KubeSwift.

### How It Works

1. The SwiftGuest controller reads `spec.interfaces`
2. For each secondary NIC, it adds a Multus annotation to the launcher pod
3. Multus creates interfaces (net1, net2, ...) in the pod's network namespace
4. `network-init.sh` bridges each Multus interface to a tap device
5. swiftletd passes multiple `--net` flags (CH) or `-netdev`/`-device` pairs (QEMU) to the hypervisor
6. Each interface appears as a virtio-net device inside the guest (eth0, eth1, ...)

### Backward Compatibility

If `spec.interfaces` is omitted or empty, a single default NIC is created —
identical to the behavior before multi-NIC support. Existing SwiftGuest manifests
work without changes.

## Prerequisites

- **Multus CNI** installed in the cluster (only needed for secondary NICs)
- **NetworkAttachmentDefinitions** created for each secondary network
- KubeSwift does NOT validate NAD existence — if a referenced NAD doesn't exist,
  Multus will fail the pod at creation time

## SwiftGuest Interface Spec

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: my-vm
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: default
  seedProfileRef:
    name: my-seed
  interfaces:
  - name: mgmt
    # No networkRef → primary NIC (KubeSwift tap+bridge+dnsmasq)
  - name: data
    networkRef:
      name: gpu-rdma-net        # NetworkAttachmentDefinition name
      namespace: default        # Optional, defaults to guest's namespace
  - name: storage
    networkRef:
      name: ceph-storage
  runPolicy: Running
```

### Rules

- If `interfaces` is nil or empty: single default NIC (backward compatible)
- The first interface WITHOUT `networkRef` is the primary NIC
- Interfaces WITH `networkRef` are secondary NICs via Multus
- Each interface must have a unique `name`
- Exactly one primary NIC is required when `interfaces` is non-empty

### Field Reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Unique interface identifier |
| `networkRef` | object | No | Reference to a NetworkAttachmentDefinition |
| `networkRef.name` | string | Yes (if networkRef) | NAD name |
| `networkRef.namespace` | string | No | NAD namespace (defaults to guest's namespace) |

## MAC Address Generation

KubeSwift generates deterministic MAC addresses for each interface using a hash
of `namespace/name/interface-name`. This ensures:

- Same MAC across pod recreations (DHCP lease stability)
- Unique MACs per interface
- `52:54:00` OUI prefix (QEMU/KVM standard locally-administered range)

## Status Reporting

- `status.network.primaryIP` — IP from the primary NIC's DHCP lease (unchanged)
- `status.network.interfaces[]` — all interfaces with name, MAC, and IP where discoverable
- Secondary NIC IPs are NOT auto-discovered via dnsmasq. They come from the CNI
  plugin's IPAM or static cloud-init `networkData`. Secondary IPs configured
  statically are visible inside the guest but may not appear in SwiftGuest status.

## Cloud-Init Network Configuration

For secondary interfaces, configure static IPs via the SwiftSeedProfile's `networkData`:

```yaml
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: multi-nic-seed
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: my-vm
  networkData: |
    version: 2
    ethernets:
      enp0s3:
        dhcp4: true          # Primary NIC — gets IP from KubeSwift dnsmasq
      enp0s4:
        addresses:
          - 192.168.100.10/24  # Secondary NIC — static IP
```

Guest interface naming depends on the OS. Ubuntu uses `enp0sN` (predictable names).
The order matches the `interfaces` array: first entry = enp0s3, second = enp0s4, etc.

## Examples

### GPU Data Plane Separation

```yaml
interfaces:
- name: mgmt        # Management: SSH, cloud-init, Kubernetes API
- name: rdma
  networkRef:
    name: gpu-rdma-net  # High-speed RDMA network for GPU communication
```

### Storage Isolation

```yaml
interfaces:
- name: mgmt
- name: storage
  networkRef:
    name: ceph-storage  # Dedicated Ceph OSD network
```

### VLAN Segmentation

```yaml
interfaces:
- name: mgmt
- name: vlan100
  networkRef:
    name: macvlan-vlan100  # macvlan NAD on VLAN 100
```

## CNI Compatibility

KubeSwift couples to the Multus + NAD interface, NOT to any specific CNI plugin.
Any CNI that works with Multus is supported:

| CNI Plugin | Use Case | Notes |
|------------|----------|-------|
| bridge | Simple L2 connectivity | Good for testing |
| macvlan | Direct host NIC access | Low overhead |
| OVN-Kubernetes | SDN with IPAM | Full network policy support |
| Calico | Network policy enforcement | Via Multus secondary |
| Cilium | eBPF networking | Via Multus secondary |
| SR-IOV | Hardware NIC passthrough | Phase C (not yet implemented) |

## Limitations

- Secondary NIC IPs are not auto-discovered in `status.network.interfaces`
- SR-IOV NIC passthrough (direct VF assignment to VM) is planned for a future phase
- No live NIC hotplug — interfaces are fixed at pod creation time
- Maximum number of NICs is limited by virtio-net PCI slot availability (practical limit ~28)
