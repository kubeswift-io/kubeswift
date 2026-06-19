# Per-tenant isolated VM networks via OVN-Kubernetes UserDefinedNetworks

Give each tenant its own **isolated layer-2 network** for SwiftGuest VMs — with
**IP-preserving live migration** — using an OVN-Kubernetes **UserDefinedNetwork
(UDN)**. This needs **no KubeSwift code or configuration beyond a `networkRef`**:
a *secondary* UDN auto-generates a NAD that KubeSwift's shipped OVN-Kubernetes
backend already drives (the same path as a hand-written `ovn-k8s-cni-overlay` NAD —
see [`ovn-l2-install.md`](ovn-l2-install.md) and [`multi-node-l2.md`](multi-node-l2.md)).

## When to use this

- You run **OVN-Kubernetes as the cluster CNI** (primary) — or kube-ovn as a
  secondary provider (use that project's NAD form instead; this guide is OVN-K).
- You want **per-tenant L2 isolation**: each tenant's VMs share a flat L2 segment
  (their own OVN logical switch), isolated from other tenants — and each VM keeps
  its IP across a `mode: live` migration.

This is **guest-level** tenancy: the VM's primary IP rides a *secondary* tenant
network; the launcher pod's `eth0` stays on the cluster-default network (control
path). For *namespace-native* tenancy — the VM on the namespace's **primary** UDN, so
every pod *and* VM in the namespace shares one tenant network — use **Model A**
([`udn-primary-tenancy.md`](udn-primary-tenancy.md), shipped in v0.5.0). Model A gives a
native UDN IP + cross-node reachability today; IP-preserving **live** migration is the
Model B (secondary-UDN) path's advantage for now (Model A live migration is a v2 item).

## The recipe

### 1. Create a per-tenant secondary UDN

```yaml
apiVersion: k8s.ovn.org/v1
kind: UserDefinedNetwork
metadata:
  name: tenant-a              # the NAD KubeSwift references has this same name
  namespace: tenant-a
spec:
  topology: Layer2
  layer2:
    role: Secondary
    subnets: ["10.30.0.0/16"]
    ipam:
      lifecycle: Persistent   # REQUIRED for IP-preserving LIVE migration (below)
```

OVN-K reconciles this to a NAD named `tenant-a` in namespace `tenant-a`. Its
generated `spec.config` is `type: ovn-k8s-cni-overlay`, `topology: layer2`, and —
because of `ipam.lifecycle: Persistent` — **`allowPersistentIPs: true`**. That
annotation is what lets KubeSwift pin the VM's IP via an `IPAMClaim` and preserve
it across a live migration; **without it you get the datapath + offline-migration
IP-keep, but a *live* migration would require `allowIPChange`**.

### 2. Reference the generated NAD from a SwiftGuest

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
      primary: true                # this NIC is the guest's primary (portable) IP
      networkRef: { name: tenant-a } # the UDN's metadata.name == the generated NAD
```

Reference the NAD by the **UDN's `metadata.name`** (`tenant-a`), *not* the
generated `config.name` (which is namespace-prefixed, e.g. `tenant-a_tenant-a`).
KubeSwift's `networkRef` resolves the NAD object by name and reads its config.

That's it. The shipped controller detects the NAD, creates+owns the per-guest
`IPAMClaim`, stamps the Multus annotation, and `network-init` bridges the VM onto
the tenant segment. The VM boots with an IP from `10.30.0.0/16`, reachable from any
pod/VM on the same tenant UDN, isolated from other tenants.

### 3. Live-migrate with the IP preserved

```bash
swiftctl migrate tenant-a-vm -n tenant-a --to <other-node>   # mode auto -> live
```

The webhook accepts `mode: live` **without** `allowIPChange` because the primary
rides a multi-node NAD; the VM keeps its tenant IP on the target node.

## Validation status

Cluster-validated end-to-end (OVN-K primary, 3-node cluster, 2026-06-18): a guest
on a UDN-generated persistent NAD booted with its tenant IP, was reachable
cross-node, and **live-migrated `mode: live` with no `allowIPChange`, IP preserved,
~3.3 s downtime** — identical to the hand-written-NAD path.

## Notes & gotchas

- **`ipam.lifecycle: Persistent` is load-bearing** for *live* migration. Omit it
  and live migration needs `allowIPChange` (the IP changes on the target).
- **Generated NAD name** is `<namespace>_<udn-name>` in `config.name`, but the NAD
  *object* is `<udn-name>` — use the latter in `networkRef`.
- **OVN-K install prerequisites** (when OVN-K is your CNI) are in
  [`ovn-l2-install.md`](ovn-l2-install.md). On OVN-K with `enableNetworkSegmentation`,
  ensure the `NetworkAttachmentDefinition`, `MultiNetworkPolicy`, and `IPAMClaim`
  CRDs are installed (OVN-K's control plane needs them at startup), and give the
  thick-Multus daemonset enough memory (its 50Mi default OOMs — use ≥512Mi).
- A plain (non-persistent) secondary UDN still gives you the **isolated datapath**
  and **offline**-migration IP-keep; only *live* migration needs the persistent
  IPAM.
- This is multi-tenancy at the **VM/guest** layer. Whole-namespace primary tenancy
  (the launcher pod's own primary on the tenant network) is a future enhancement,
  not yet shipped.
