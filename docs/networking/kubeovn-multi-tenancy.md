# Per-tenant isolated VM networks via kube-ovn Subnets

Give each tenant its own **isolated layer-2 network** for SwiftGuest VMs — with
**IP-preserving live migration** — using a per-tenant **kube-ovn Subnet**. This is
the kube-ovn sibling of the OVN-Kubernetes UDN recipe
([`udn-multi-tenancy.md`](udn-multi-tenancy.md)); pick the guide that matches your
cluster's OVN substrate.

## When to use this

- You run a **different primary CNI** (Calico, Cilium, Flannel…) that you want to
  **keep**, and add **kube-ovn in non-primary mode** alongside it (the
  [kube-ovn install guide](ovn-l2-install.md)). If OVN-Kubernetes is your *primary*
  CNI instead, use [`udn-multi-tenancy.md`](udn-multi-tenancy.md).
- You want **per-tenant L2 isolation**: each tenant's VMs share a flat L2 segment
  (their own kube-ovn **Subnet** = its own OVN logical switch), and each VM keeps its
  IP across a `mode: live` migration.

This is **guest-level** tenancy: the VM's primary IP rides the tenant Subnet; the
launcher pod's `eth0` stays on your primary CNI (control path). KubeSwift attaches
through the standard Multus + NAD interface — no KubeSwift code or config beyond a
`networkRef`.

## The recipe

### 1. Create a per-tenant kube-ovn Subnet + NAD

The **Subnet** is cluster-scoped; the **NAD** is namespaced; `provider` links them as
`<nad-name>.<nad-namespace>.ovn`. Give each tenant its own pair:

```yaml
apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: tenant-a                    # cluster-scoped
spec:
  protocol: IPv4
  cidrBlock: 10.30.0.0/16           # clear of pod/service CIDRs and kube-ovn's default VPC (10.16/16)
  excludeIps: ["10.30.0.1"]
  gateway: "10.30.0.1"
  gatewayType: distributed
  natOutgoing: false                # a flat L2 segment, not routed/NAT'd
  provider: tenant-a.tenant-a.ovn   # <nad-name>.<nad-namespace>.ovn
---
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tenant-a
  namespace: tenant-a
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "type": "kube-ovn",
      "server_socket": "/run/openvswitch/kube-ovn-daemon.sock",
      "provider": "tenant-a.tenant-a.ovn"
    }
```

> Unlike the OVN-Kubernetes UDN path, kube-ovn needs **no persistent-IP flag** (no
> `allowPersistentIPs` / `ipam.lifecycle: Persistent`). KubeSwift's kube-ovn backend
> pins the guest's IP via the `<provider>.kubernetes.io/ip_address` annotation and
> lets the migration destination acquire it through a `kubevirt.io/migrationJobName`
> marker — so live-migration IP-keep works out of the box.

### 2. Reference the NAD from a SwiftGuest

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: tenant-a-vm
  namespace: tenant-a
spec:
  imageRef: { name: ubuntu-noble }
  guestClassRef: { name: default }
  seedProfileRef: { name: default }
  # RWX+Block is the live-migration storage requirement (Longhorn migratable shown).
  storage:
    accessMode: ReadWriteMany
    volumeMode: Block
    storageClassName: longhorn-migratable
  interfaces:
    - name: app
      primary: true                # the guest's primary (portable) IP
      networkRef: { name: tenant-a } # the NAD in this namespace
```

The shipped controller detects the kube-ovn NAD, stamps the guest's OVN port identity
(MAC + IP), and `network-init` bridges the VM onto the tenant Subnet. The VM boots
with an IP from `10.30.0.0/16`, reachable from any pod/VM on the same Subnet.

### 3. Live-migrate with the IP preserved

```bash
swiftctl migrate tenant-a-vm -n tenant-a --to <other-node>   # mode auto -> live
```

`mode: live` is accepted **without** `allowIPChange` (the primary rides a multi-node
NAD); the VM keeps its tenant IP on the target node.

## Stronger isolation (overlapping CIDRs / full L3 separation)

A bare per-tenant **Subnet** gives each tenant its own L2 segment (logical switch).
For **strict cross-tenant isolation** — e.g. overlapping CIDRs, or guaranteeing no
L3 path between tenants — use kube-ovn's first-class tenancy primitive, a **per-tenant
VPC** (a separate virtual router), or apply **kube-ovn NetworkPolicy** per Subnet. The
per-Subnet path above is the KubeSwift-validated one; the VPC + secondary-network
combination is kube-ovn-native but validate it for your topology. See the
[kube-ovn VPC docs](https://kubeovn.github.io/docs/stable/en/vpc/vpc/).

## Validation status

The guest-on-kube-ovn-Subnet datapath and **IP-preserving live migration** are
cluster-validated (the kube-ovn primary-on-NAD arc, PRs #235–#240: cross-node
`mode: live`, no `allowIPChange`, ~3.2 s downtime, IP preserved + reachable from a
third node). Per-tenant isolation is achieved by giving each tenant its own Subnet;
cross-tenant traffic policy is the operator's choice (separate Subnets / VPC /
NetworkPolicy, above).

## Notes & gotchas

- **`provider` must match** `<nad-name>.<nad-namespace>.ovn` on both the Subnet and
  the NAD config, or the attachment fails.
- **`cidrBlock` must not overlap** your pod/service CIDRs or kube-ovn's default VPC
  (`10.16.0.0/16`).
- **No DHCP conflict.** kube-ovn secondary Subnets don't serve DHCP; the pod gets a
  static IPAM address on `net1` and KubeSwift's per-pod dnsmasq hands that exact
  address to the guest.
- Installing/operating kube-ovn non-primary (and the full guest + migration
  walkthrough) is in [`ovn-l2-install.md`](ovn-l2-install.md).
- This is multi-tenancy at the **VM/guest** layer; the launcher pod's `eth0` stays
  on your primary CNI.
