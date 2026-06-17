# Running an IP-preserving guest on OVN-Kubernetes-primary

> Goal: on a cluster where **OVN-Kubernetes is the primary CNI**, put a KubeSwift
> guest's **primary** NIC on an OVN-K `layer2` network so it gets a **portable
> primary IP** — the substrate for **IP-preserving live migration** (cross-node
> `mode: live`, no `allowIPChange`), telco/NFV, and stateful services with external
> clients. This is the OVN-Kubernetes sibling of the kube-ovn-non-primary path in
> the [OVN L2 install guide](ovn-l2-install.md).
>
> **Status: cluster-validated.** A real RWX+Block disk-boot Ubuntu-Noble guest
> booted reachable cross-node and live-migrated with its IP preserved in **2.806 s**
> on a dedicated OVN-K-primary cluster (see [Verify](#step-5--verify-boot-cross-node-reach-ip-preserving-migration)).
>
> See [Multi-node L2 networking](multi-node-l2.md) for the feature/CRD reference,
> the [OVN-Kubernetes Integration Guide](ovn-kubernetes.md) for secondary-network
> use cases (storage / GPU data plane / VLAN / UDN), and the
> [RFC](../design/ovn-cni-backends.md) for the pluggable `ovnBackend` design.

---

## OVN-Kubernetes-primary vs. kube-ovn-non-primary — pick the right guide

Both deliver the same KubeSwift capability (portable primary IP + IP-preserving
live migration). The choice is purely about **which CNI runs your cluster's primary
pod network**:

| Your cluster | Use | Guide |
|---|---|---|
| **OVN-Kubernetes is already the primary CNI** (OpenShift, upstream OVN-K, telco) | OVN-K's **native** `layer2` NAD (this guide) | **this guide** |
| A **different** primary CNI (Calico, Cilium, Flannel…) you want to **keep** | **kube-ovn in non-primary mode** alongside it | [OVN L2 install guide](ovn-l2-install.md) |
| Greenfield / no primary CNI yet | OVN-Kubernetes or kube-ovn as the primary CNI | either; this guide if you choose OVN-K |

Upstream OVN-Kubernetes secondary networks assume OVN-K *is* the primary CNI — so
this guide is for clusters already standardized on it. On a Calico/Cilium cluster,
do **not** install OVN-Kubernetes as a parallel overlay; use kube-ovn-non-primary
instead (the [other guide](ovn-l2-install.md)).

> KubeSwift does **not** import or depend on any OVN-Kubernetes type. It attaches
> through the standard **Multus + NetworkAttachmentDefinition** interface, so the
> same SwiftGuest manifests work whether the NAD is backed by OVN-Kubernetes,
> kube-ovn, macvlan, or bridge CNI. For an OVN-K-class primary NAD the controller
> additionally programs the guest's OVN port identity and pins its IP automatically
> (see [Step 4](#step-4--run-a-guest-on-the-segment) and
> [Step 5](#step-5--verify-boot-cross-node-reach-ip-preserving-migration)).

---

## How it works (what KubeSwift does for you)

When a SwiftGuest's **primary** interface rides an OVN-K `layer2` NAD, KubeSwift
makes the guest reachable on the segment and preserves its IP across a migration —
**zero-touch, no manual `ovn-nbctl` / `IPAMClaim` authoring**:

- **Identity.** OVN-Kubernetes binds each logical-switch port to the *pod NIC's*
  identity, but KubeSwift's datapath bridges the guest's *own* hypervisor MAC
  behind the pod NIC. So the controller injects the **guest MAC** as the `mac`
  field of the guest's entry in the `k8s.v1.cni.cncf.io/networks` (Multus)
  annotation — making the OVN port identity the guest. (OVN-K's identity rides
  *inside* that annotation, unlike kube-ovn's flat `<provider>.kubernetes.io/*`
  keys — the backend handles the difference.)
- **IP pinning.** The controller creates and owns (per guest, GC'd by owner-ref) an
  **`IPAMClaim`** (`k8s.cni.cncf.io/v1alpha1`) and references it via
  `ipam-claim-reference` in the same Multus selection element. OVN-Kubernetes does
  **not** auto-create this claim — KubeSwift does. This is what pins the primary IP
  so it survives a move. (Requires `allowPersistentIPs: true` on the NAD — see
  [Step 3](#step-3--create-the-layer2-segment-the-nad).)
- **Datapath re-MAC.** Because the pod NIC now carries the guest's MAC, enslaving
  it to `br0` would make the kernel add a permanent fdb entry `<guest-mac> -> net1`
  that shadows the guest's tap. `network-init` re-MACs the pod NIC to a dummy
  **before** enslaving it (the KubeVirt bridge-binding pattern); the OVN port keeps
  the guest MAC, so OVN still delivers `net1 -> br0 -> tap`.
- **Live migration.** The destination pod inherits the same Multus annotation (so
  the same guest MAC + the same claim reference). OVN-Kubernetes allows the
  cross-node claim **overlap by default** — the dst acquires the IP while the source
  still holds it through cutover — so **no `migrationJobName`-style marker is
  needed** (simpler than kube-ovn).

All of this is automatic and a **no-op for every other networking mode** (node-local
bridge, non-OVN-K NAD, SR-IOV). Both the **controller-manager** and the launcher
(**`swiftletd`**) image must carry the integration.

---

## Prerequisites

- **OVN-Kubernetes as the cluster's primary CNI**, with the multi-network feature
  enabled (default in OpenShift 4.14+; upstream OVN-Kubernetes per its
  multi-network config).
- **Multus CNI** installed (KubeSwift requires Multus already). Every node that
  hosts a primary-on-NAD guest — or is a migration target — must run Multus.
- The **`IPAMClaim` CRD** (`ipamclaims.k8s.cni.cncf.io`, the NPWG
  network-attachment-definition-client persistent-IP CRD) installed. KubeSwift
  creates an `IPAMClaim` per guest to pin its primary IP; without the CRD the
  backend cannot persist the IP. Most OVN-K installs that ship `allowPersistentIPs`
  also ship this CRD; verify with `kubectl get crd ipamclaims.k8s.cni.cncf.io`.
- **(Optional) CSI VolumeSnapshot CRDs** (`snapshot.storage.k8s.io`) — only if you
  want **CSI VM snapshots**. They are **not** needed for the VM runtime or for
  live migration. A bare OVN-K cluster (e.g. Longhorn without the external
  snapshotter CRDs) historically crash-looped the controller; if you intend to use
  snapshots, install the snapshotter CRDs + controller first. (This hard
  dependency is being removed; until then, install them if you want snapshots and
  it is harmless to install them regardless.)
- **RWX + Block storage** (a migratable CSI StorageClass) if you want to **live**
  migrate the guest — the live-migration storage requirement (KubeVirt model). On
  Longhorn this is a StorageClass with `parameters.migratable: "true"`.
- KubeSwift **≥ the release carrying the OVN-Kubernetes backend** (OVN-K arc P2,
  `#244`) — both the controller stamping/IPAMClaim and the launcher `network-init`
  datapath re-MAC. Earlier builds boot an OVN-K-primary guest but it is
  **unreachable** (no OVN port identity, or the pod NIC shadows the guest's tap).

---

## Step 1 — confirm OVN-Kubernetes is the primary CNI

```bash
# OVN-Kubernetes components Running:
kubectl get pods -n ovn-kubernetes 2>/dev/null || kubectl get pods -A | grep -i ovnkube

# A plain pod gets an address from OVN-K's pod network (sanity):
kubectl run cni-check --image=busybox --restart=Never --command -- sleep 60
kubectl get pod cni-check -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod cni-check
```

This guide assumes OVN-K is already serving the cluster's primary network. If it is
not — and you do not want to re-CNI — use the
[kube-ovn-non-primary guide](ovn-l2-install.md) instead.

---

## Step 2 — confirm the persistent-IP CRD

```bash
kubectl get crd ipamclaims.k8s.cni.cncf.io
# NAME                              CREATED AT
# ipamclaims.k8s.cni.cncf.io        ...
```

If it is missing, install the NPWG network-attachment-definition-client CRDs (the
same project that defines `network-attachment-definitions`). KubeSwift needs it to
pin the guest's IP.

---

## Step 3 — create the layer2 segment (the NAD)

A `layer2` NAD with **`allowPersistentIPs: true`** defines an overlay L2 segment
spanning all nodes and enables the persistent-IP (IPAMClaim) mechanism KubeSwift
relies on:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ovn-l2
  namespace: default                 # same ns as your SwiftGuests
spec:
  config: |
    {
      "cniVersion": "0.4.0",
      "type": "ovn-k8s-cni-overlay",
      "name": "ovn-l2",
      "topology": "layer2",
      "subnets": "10.20.0.0/16",
      "allowPersistentIPs": true,
      "netAttachDefName": "default/ovn-l2"
    }
```

The runnable sample is
[`config/samples/multi-node-l2/ovn-l2-nad.yaml`](../../config/samples/multi-node-l2/ovn-l2-nad.yaml).

> **`allowPersistentIPs: true` is REQUIRED.** It is what lets OVN-Kubernetes persist
> an allocated IP in an `IPAMClaim` (and lets KubeSwift's backend pin the guest's
> primary IP). Without it, the IPAMClaim mechanism does not engage and the IP is
> **not** preserved across a migration. `subnets` must be set (the claim mechanism
> needs OVN-K IPAM on the segment).

Verify the NAD exists:

```bash
kubectl get net-attach-def ovn-l2 -n default
```

---

## Step 4 — run a guest on the segment

Put the guest's **primary** interface on the NAD (and use RWX+Block storage to make
it live-migratable):

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: ovnk-vm
  namespace: default
spec:
  imageRef: { name: ubuntu-noble }
  guestClassRef: { name: default }
  seedProfileRef: { name: default }
  runPolicy: Running
  # RWX+Block storage is required to LIVE-migrate (a migratable CSI):
  storage: { accessMode: ReadWriteMany, volumeMode: Block, storageClassName: <migratable-sc> }
  interfaces:
    - name: app
      primary: true                  # this NIC is the guest's primary (portable) IP
      networkRef: { name: ovn-l2 }    # ...riding the OVN-K layer2 segment
  migration:
    enabled: true
    preferredMode: live
    drainPolicy: Migrate
```

This is the same shape as
[`config/samples/migratable-guests/03-ip-preserving-live-migratable.yaml`](../../config/samples/migratable-guests/03-ip-preserving-live-migratable.yaml),
which already references the `ovn-l2` NAD.

The guest comes up `Running` with `status.network.primaryIP` from the `10.20.x`
segment. **You do nothing manual** — the controller injects the guest MAC +
`ipam-claim-reference` into the Multus annotation, creates+owns the per-guest
`IPAMClaim`, and `network-init` re-MACs the pod NIC. The guest is reachable on the
segment with **no `ovn-nbctl`**.

---

## Step 5 — verify (boot, cross-node reach, IP-preserving migration)

This is the cluster-validated flow — a real RWX+Block disk-boot Ubuntu-Noble guest
on a dedicated OVN-K-primary cluster.

**Boot + cross-node reachability.**

```bash
# Guest Running with its NAD IP:
kubectl get swiftguest -n default ovnk-vm \
  -o jsonpath='{.status.phase} {.status.network.primaryIP}{"\n"}'
# -> Running 10.20.0.1

# The controller created+owns the per-guest IPAMClaim:
kubectl get ipamclaim -n default ovnk-vm-net1 \
  -o jsonpath='{.status.ips}{"\n"}'                # -> ["10.20.0.1/16"]

# Reachable cross-node: ping the guest IP from a pod attached to ovn-l2 on ANOTHER node.
# (annotate the prober pod with k8s.v1.cni.cncf.io/networks: default/ovn-l2 and pin
#  it to a different node than the guest's) -> 0% packet loss
```

**IP-preserving live migration (the marquee).** Because the primary rides a
multi-node NAD, `mode: live` is accepted **without `allowIPChange`**:

```bash
swiftctl migrate ovnk-vm -n default --to <other-node>   # mode auto -> live
# or apply a SwiftMigration with spec.mode: live
kubectl get swiftmigration -n default -w
```

Expect **`Completed`**, `observedDowntime` a few seconds, the guest on the target
node with the **same** IP, and reachability from a **third** node afterward. In the
P3 validation a `mode: live` migration ntx2→ntx1 with **no `allowIPChange`**
Completed in **2.806 s** downtime with the IP **preserved (`10.20.0.1`) and
reachable from a third node (ntx3) at 0% loss**.

```bash
# After migration: same IP, now on the target node:
kubectl get swiftguest -n default ovnk-vm \
  -o jsonpath='{.status.nodeName} {.status.network.primaryIP}{"\n"}'
# -> <other-node> 10.20.0.1
```

---

## UserDefinedNetwork (UDN) notes

- **UDN *secondary* is handled transparently.** A `UserDefinedNetwork` (role
  Secondary, topology Layer2) generates a `type: ovn-k8s-cni-overlay` `layer2` NAD,
  which KubeSwift's OVN-Kubernetes backend detects exactly like the `ovn-l2` NAD
  above. A guest just points `interfaces[].networkRef` at the generated NAD — **no
  extra integration**. (Proven at the raw-pod level; rides the validated code
  path.) See [UDN tenant isolation](ovn-kubernetes.md#4-tenant-isolation-with-userdefinednetwork).
- **UDN *primary* (per-tenant primary networks / multi-tenancy) is a separate later
  phase — NOT shipped here.** Guest-on-the-pod-PRIMARY-network is a different
  datapath, requires the OVN-K `--enable-network-segmentation` gate, and
  destabilized the node datapath on a small validation cluster. Do not treat
  UDN-primary as available via this backend.

---

## Troubleshooting

- **Guest gets an IP but is unreachable cross-node.** Confirm the launcher pod's
  `k8s.v1.cni.cncf.io/networks` annotation carries the guest's `mac` on the NAD
  entry (the OVN port identity) and that the per-guest `IPAMClaim` exists
  (`kubectl get ipamclaim -n <ns> <guest>-net1`). If the annotation has no `mac`,
  the controller is **older than the OVN-K backend (`#244`)**. If the claim is
  missing, check that the NAD has **`allowPersistentIPs: true`** and that the
  `ipamclaims.k8s.cni.cncf.io` CRD is installed. Also confirm the launcher image
  carries the `network-init` re-MAC (`bridge fdb show br0` should **not** show a
  permanent `<guest-mac> -> net1` entry).
- **`mode: live` migration is rejected for `allowIPChange`.** The guest's
  **primary** interface must set both `primary: true` **and** `networkRef`
  (a secondary NAD does not make the primary IP portable). Confirm with
  `kubectl get swiftguest <name> -o jsonpath='{.spec.interfaces}'`.
- **dst can't keep the IP during migration.** Unlike kube-ovn, OVN-Kubernetes needs
  no `migrationJobName` marker — the src and dst share the same `IPAMClaim` and the
  overlap is allowed by default. If the dst fails to come up, confirm it inherited
  the source's `k8s.v1.cni.cncf.io/networks` annotation (the `#235` carry-through)
  and that `network-init` ran on the dst.
- **`IPAMClaim.status.ownerPod` looks stale after a migration.** This is advisory
  only — there is no kubevirt-ipam-claims controller updating it on an OVN-K-primary
  cluster, and IP delivery follows the live pod regardless. The KubeSwift backend
  owns the claim lifecycle (and GCs it with the guest via owner-ref).
- **Controller crash-loops on a bare OVN-K cluster.** Historically caused by missing
  CSI VolumeSnapshot CRDs (`snapshot.storage.k8s.io`). Install the snapshotter CRDs
  if you want CSI snapshots; they are not needed for the VM runtime. (This hard
  dependency is being removed in a separate change.)
- **Cross-node L2 fails.** OVN-Kubernetes layer2 uses Geneve (UDP 6081) between
  nodes; verify it is open in any firewall between nodes.

---

## See also

- [OVN L2 install guide (kube-ovn non-primary)](ovn-l2-install.md) — the sibling
  path for clusters on a different primary CNI.
- [Multi-node L2 networking](multi-node-l2.md) — the feature/CRD reference and the
  validation matrix.
- [OVN-Kubernetes Integration Guide](ovn-kubernetes.md) — OVN-K secondary-network
  use cases (storage, GPU data plane, VLAN/localnet, UDN).
- [RFC: Pluggable OVN CNI backends](../design/ovn-cni-backends.md) — the
  `ovnBackend` design and the P0/P3 validation evidence.
