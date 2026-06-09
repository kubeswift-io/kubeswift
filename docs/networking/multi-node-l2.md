# Multi-node L2 networking (IP-preserving guests)

> **Status (2026-06): control-plane shipped + runtime EXPERIMENTAL.** The
> control-plane (the `spec.interfaces[].primary` field, the resolver wiring, and
> the corrected SwiftMigration IP-preservation gate) is shipped and tested. The
> launcher **runtime datapath** (attaching the *primary* NIC to a NAD and giving
> the guest the NAD's IP) is now implemented but the **datapath is UNVALIDATED**
> — the dev cluster has no working multi-node L2 (Multus secondary attach does
> not produce an interface there). It will be validated and tuned on an
> OVN-Kubernetes cluster. Treat primary-on-NAD as experimental until then. See
> [`../design/network-architecture-requirements.md`](../design/network-architecture-requirements.md)
> for the framework.
>
> **Runtime model (KubeVirt-style bridge binding):** network-init reads the IP
> the NAD's CNI assigned to the pod's Multus interface, flushes it off that
> interface, bridges the interface to the guest's tap, and the launcher's
> in-pod dnsmasq hands that exact IP to the guest (matched by MAC) — so the
> guest's primary IP is the NAD's portable IP, and the existing lease-file IP
> discovery works unchanged. The in-pod dnsmasq binds via a best-effort helper
> IP (`<subnet>.254`) in the NAD subnet — a heuristic with a documented
> collision risk, expected to be refined during real-cluster validation.

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

- **Runtime datapath is EXPERIMENTAL and unvalidated** (see the status banner) —
  it needs an OVN-K / working-NAD cluster to validate and tune (the helper-IP /
  dnsmasq binding in particular).
- **Prerequisite multi-NIC fixes — shipped.** The `network-init` container now
  mounts the RuntimeIntent + run dir, the launcher image carries `python3`, and
  network-init skips vhost-user NICs, so multi-NIC bridging actually runs. (These
  were latent bugs that meant multi-NIC never worked end-to-end before.)
- **Helper-IP heuristic.** The in-pod dnsmasq binds to `<NAD-subnet>.254`; if
  that host is taken on the segment it will collide. Refine during validation.
- **SR-IOV** is a hardware-passthrough NIC, not a Tier-C overlay; it is rejected
  for cross-node migration regardless (Phase 4+).
