# Multi-node L2 networking (IP-preserving guests)

> **Status (2026-06): datapath + offline migration VALIDATED; live-migration
> IP-preservation is partial (two bugs fixed, full completion OVN-K-gated).**
> The control-plane (`spec.interfaces[].primary`, the resolver wiring, the
> `PrimaryIPPreservedCrossNode()` migration gate) and the **runtime datapath**
> (attaching the *primary* NIC to a NAD and giving the guest the NAD's IP) are
> cluster-validated on a hand-rolled VXLAN-mesh L2. See the validation matrix
> below for exactly what is proven vs. what still needs an OVN-Kubernetes lab.

By default a SwiftGuest's primary IP is **node-local** — it comes from a
per-node dnsmasq on `br0` and is NAT-masqueraded out the pod's `eth0`. That IP
does **not** survive a move to another node, which is why cross-node migration
of a default-networking guest requires `spec.allowIPChange=true`.

To give a guest a **portable** primary IP (one that follows it across nodes —
for IP-preserving live/offline migration, telco/NFV, or stateful services with
external clients), put its **primary interface on a multi-node L2 network**: a
single L2 broadcast domain that spans every candidate node. KubeSwift attaches
to it through a Multus NetworkAttachmentDefinition (NAD), so the technology is
the operator's choice — the recommended reference is **OVN-Kubernetes layer-2**
(portable overlay, no physical-NIC dependency).

## Validation status

| Capability | Status | Notes |
|---|---|---|
| Datapath — guest gets the NAD's portable IP | ✅ validated | `setup_primary_nad_nic`: flush NAD IP → bridge → fixed-lease dnsmasq → status |
| Cross-node L2 reachability | ✅ validated | both directions over a VXLAN-mesh overlay |
| **Offline** migration IP-preservation | ✅ validated | guest reacquired its NAD IP on the target node, no `allowIPChange` |
| **Live** migration IP-preservation | ⚠️ partial | two real bugs fixed ([#235](https://github.com/projectbeskar/kubeswift/pull/235) Multus-annotation on the dst pod, [#236](https://github.com/projectbeskar/kubeswift/pull/236) dst-receiver send-retry); end-to-end completion is **OVN-K-gated** — see below |

**Why live migration is OVN-K-gated.** On a *hand-rolled* VXLAN-mesh substrate,
both the migration source and destination launcher pods end up **multi-homed
across two overlays** (the cluster CNI's pod network *and* the mesh). That
specific combination intermittently breaks the cross-node migration channel
(isolation: plain↔plain, plain↔NAD, NAD↔plain all work; only NAD↔NAD fails).
A real **OVN-Kubernetes layer-2** secondary network manages the attachment
*inside the CNI* and never multi-homes a pod across two overlays, so it does not
hit this — which is why end-to-end live-migration IP-preservation must be
validated on an OVN-K (or equivalent) Tier-C network, not the lab mesh. See
[`../design/network-architecture-requirements.md`](../design/network-architecture-requirements.md) §6.

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

### Migration-gate behavior

The IP-preservation gate keys on the **primary** interface, not on "any
`networkRef`":

| Guest networking | Cross-node migration |
|---|---|
| Default (node-local bridge) | requires `spec.allowIPChange=true` (IP changes) |
| Node-local primary **+** secondary NAD | requires `allowIPChange` — the *primary* IP still changes |
| **Primary on a NAD** (`primary: true` + `networkRef`) | accepted, IP preserved, no `allowIPChange` |
| Single NAD interface (de-facto primary) | accepted, IP preserved |

## Recipe A (reference): OVN-Kubernetes layer-2 NAD

> The portable, production-recommended path, and the one that validates
> end-to-end live-migration IP-preservation. Requires OVN-Kubernetes as the
> cluster CNI **or** as a secondary-network provider.

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

OVN-K provides DHCP/IPAM on the segment, so the guest receives a `10.20.x.y`
address that the segment delivers to whichever node the guest runs on — the
property that makes the IP portable across a migration. Because OVN-K does *not*
multi-home the pod across a second overlay, it avoids the NAD↔NAD migration-channel
issue of the hand-rolled mesh.

> **Note on the per-pod dnsmasq.** KubeSwift's datapath synthesizes DHCP for the
> guest via a per-pod dnsmasq (needed for a plain bridge NAD whose IPAM gives the
> *pod* an IP but no guest-reachable DHCP server). On a NAD that already serves
> DHCP on the segment (OVN-K layer-2), the per-pod dnsmasq is redundant; a
> "segment-DHCP" mode that skips it is a follow-up.

## Recipe B (lab / on-prem): hand-rolled VXLAN-mesh + bridge NAD

> The lightweight path that validated the datapath + offline migration here,
> and works on **any** cluster (no CNI change) including different-subnet nodes.
> It is **not** suitable for end-to-end live-migration IP-preservation (the
> multi-homed-pod issue above). Samples: [`../../config/samples/multi-node-l2/`](../../config/samples/multi-node-l2/).

A privileged DaemonSet builds a VXLAN uplink + host bridge on every node → one
flat L2; a Multus `bridge` NAD attaches launcher pods to it.

**Three load-bearing rules (each cost us a debugging cycle):**

1. **The VXLAN mesh MUST use a UDP port the cluster CNI does NOT use.** Calico's
   VXLAN is **4789**; sharing it silently breaks Calico's cross-node pod
   networking (and with it the migration mTLS channel). Use e.g. **8472**. On a
   flannel cluster (which uses 8472) pick something else.
2. **Every node that hosts a primary-on-NAD guest — or is a migration target —
   MUST run Multus.** A node excluded from `kube-multus-ds` silently produces a
   guest stuck at `network-init: primary NAD interface net1 not found` because
   the NAD never plumbs there.
3. **VXLAN MAC-move convergence.** After a migration the moved guest's MAC must
   GARP for the learning-mesh FDB to re-converge (~tens of seconds); a real L2
   fabric / OVN-K converges immediately.

The in-pod dnsmasq is hardened for the shared L2 (a single flat segment means
every pod's dnsmasq sees the others' guests' DHCP): it answers only its own
guest's MAC (`--dhcp-ignore=tag:!known`), hands an infinite lease (the helper-IP
server-id is ambiguous on the shared L2, so the guest must never renew), and
carries the overlay MTU via `option:mtu`.

## Limitations / follow-ups

- **End-to-end live-migration IP-preservation is OVN-K-gated** (the lab mesh's
  multi-homed-pod issue). The control-plane gate, the datapath, offline
  IP-preservation, and the two live-migration bug fixes are all in place; the
  remaining validation needs an OVN-K (or real Tier-C) network.
- **SR-IOV** is a hardware-passthrough NIC, not a Tier-C overlay; it is rejected
  for cross-node migration regardless.
