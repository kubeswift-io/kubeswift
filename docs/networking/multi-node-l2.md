# Multi-node L2 networking (IP-preserving guests)

> **Status (2026-06):** the **control-plane** half is shipped — the
> `spec.interfaces[].primary` field, the resolver wiring, and the corrected
> SwiftMigration IP-preservation gate. The **runtime datapath** (attaching the
> *primary* NIC to a NAD inside the launcher, and discovering its NAD-assigned
> IP) is landing across follow-up PRs; until then, a `primary: true` + NAD
> interface is admitted and classified correctly for migration but the in-guest
> datapath is not yet wired. See
> [`../design/network-architecture-requirements.md`](../design/network-architecture-requirements.md)
> for the framework and the chosen approach.

By default a SwiftGuest's primary IP is **node-local** — it comes from a
per-node dnsmasq on `br0` and is NAT-masqueraded out the pod's `eth0`. That IP
does **not** survive a move to another node, which is why cross-node migration
of a default-networking guest requires `spec.allowIPChange=true`.

To give a guest a **portable** primary IP (one that follows it across nodes —
e.g. for IP-preserving live migration, telco/NFV, or stateful services with
external clients), put its **primary interface on a multi-node L2 network**: a
single L2 broadcast domain that spans every candidate node. KubeSwift attaches
to it through a Multus NetworkAttachmentDefinition (NAD), so the technology is
the operator's choice — the recommended reference is **OVN-Kubernetes
layer-2** (portable overlay, no physical-NIC dependency).

## The `primary` field

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: portable-ip-guest
spec:
  imageRef: { name: ubuntu-noble }
  guestClassRef: { name: default }
  seedProfileRef: { name: default }
  interfaces:
    - name: app
      primary: true                 # this NIC is the guest's primary IP
      networkRef: { name: ovn-l2 }   # ...and it rides a multi-node NAD
```

- At most one interface may set `primary: true`, and only a bridge-type
  interface (the default) can be primary.
- When `primary: true` **and** `networkRef` are both set, the primary NIC rides
  the NAD instead of the node-local bridge — the guest's IP comes from the NAD's
  IPAM and is portable.
- This is also the operator's **attestation** that the NAD is a genuine
  multi-node L2: the SwiftMigration webhook then treats the guest as
  IP-preserving and does **not** require `allowIPChange` for cross-node moves.

### Migration-gate behavior (shipped)

The IP-preservation gate keys on the **primary** interface, not on "any
`networkRef`":

| Guest networking | Cross-node migration |
|---|---|
| Default (node-local bridge) | requires `spec.allowIPChange=true` (IP changes) |
| Node-local primary **+** secondary NAD | requires `allowIPChange` — the *primary* IP still changes |
| **Primary on a NAD** (`primary: true` + `networkRef`) | accepted, IP preserved, no `allowIPChange` |
| Single NAD interface (de-facto primary) | accepted, IP preserved |

(The middle row is the correctness fix from
[network-architecture-requirements.md §7.2](../design/network-architecture-requirements.md):
a secondary NAD no longer makes a node-local-primary guest look IP-preserving.)

## Reference recipe: OVN-Kubernetes layer-2 NAD

> Requires OVN-Kubernetes as the cluster CNI (or secondary-network provider).
> The dev cluster runs Calico, so this path is **not validated there** — it is
> documented as the reference for clusters on OVN-K. macvlan is the lightweight
> alternative on operator-controlled L2 (see the design doc §5.1; note the
> MAC-filtering caveat on clouds / Hetzner).

A layer-2 NAD defines an overlay L2 segment spanning all nodes:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ovn-l2
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.4.0",
      "type": "ovn-k8s-cni-overlay",
      "name": "ovn-l2",
      "topology": "layer2",
      "subnets": "10.20.0.0/16",
      "netAttachDefName": "default/ovn-l2"
    }
```

Then reference it from the guest's primary interface as shown above. OVN-K
provides DHCP/IPAM on the segment, so the guest receives a `10.20.x.y` address
that the segment delivers to whichever node the guest runs on — the property
that makes the IP portable across a migration.

## Limitations / follow-ups

- **Runtime datapath in progress** (see the status banner). The control-plane
  classification is shipped; the launcher-side attach + IP discovery land next.
- **A prerequisite multi-NIC fix** (the `network-init` container gaining the
  RuntimeIntent mount so Multus interfaces are actually bridged) ships ahead of
  the primary-on-NAD datapath.
- **SR-IOV** is a hardware-passthrough NIC, not a Tier-C overlay; it is rejected
  for cross-node migration regardless (Phase 4+).
