# Network Architecture Requirements

> Status: **DESIGN** (Tracked Follow-up #1). Evaluates the multi-node
> L2 options for KubeSwift, recommends one as the reference path, and
> enumerates the code gaps to close. Promotes live-migration.md
> Constraint 6 into a first-class requirements framework that several
> features depend on.
> Last updated: 2026-06-09.

## 1. Why this document exists

Several KubeSwift capabilities are gated not on KubeSwift code but on the
**cluster's network architecture**. They were surfaced piecemeal — as
Constraint 6 in [`live-migration.md`](live-migration.md), as the
"Networks that preserve IPs cross-node" section in
[`../migration/networking-requirements.md`](../migration/networking-requirements.md),
and as the OVN-K integration guide in
[`../networking/ovn-kubernetes.md`](../networking/ovn-kubernetes.md). This
doc consolidates them into one framework so operators and future
features reason about the choice once, in one place.

The pivotal axis is **node-local vs multi-node networking**, and within
multi-node, **L3-routed vs L2-flat**.

## 2. The networking spectrum

KubeSwift attaches a guest to the network through the launcher pod. There
are three tiers, in increasing capability and cluster cost:

| Tier | Model | Guest IP | What it costs the operator |
|---|---|---|---|
| **A. Node-local bridge** (default today) | tap+br0 per pod, dnsmasq DHCP from a node-local range, NAT-masqueraded out the pod's eth0 | Private RFC1918, **local to the node**; not reachable off-node | Nothing — works out of the box |
| **B. Multi-node L3 (routed)** | The cluster pod network (Calico/Cilium VXLAN/BGP) routes the pod IP across nodes | Reachable cross-node, **but the IP is bound to the source node's IPAM/veth** — it changes when the pod moves | Nothing beyond the existing CNI |
| **C. Multi-node L2 (flat)** | The guest's NIC sits on **one L2 broadcast domain spanning all nodes** (macvlan on a shared NIC, or an OVN-K logical switch) | A stable L2 identity (MAC+IP) that **follows the guest to any node** | A multi-node-L2 network attachment (this document) |

KubeSwift's default is **Tier A**. The cluster's CNI provides **Tier B**
for pod-to-pod traffic but does **not** give a guest a portable IP. Only
**Tier C** delivers an IP that survives a move between nodes.

## 3. Capabilities that require multi-node L2 (Tier C)

| Capability | Why it needs Tier C |
|---|---|
| **Live migration with IP preservation** | CH keeps the guest's MAC/IP byte-for-byte across the move (Constraint 6). The destination node must deliver that same IP. On Tier A/B the IP is node-bound, so the guest comes up with a different IP — today gated behind `spec.allowIPChange=true`. |
| **Offline migration with IP preservation** | Same as above for the bounded-downtime offline path — the recreated guest must reacquire its old IP on the new node. |
| **Multi-tenancy with cross-node isolation** | Per-tenant L2 segments that isolate guests at layer 2 regardless of which node they land on (Tier B's flat pod network does not isolate tenants). |
| **Telco / NFV** | VNFs expect L2 adjacency, multicast/broadcast, non-IP ethertypes, and stable MACs — none of which survive a Tier A/B move. |
| **Stateful services with external clients** | A guest whose clients hold its IP (databases, license servers, appliances) must keep that IP across maintenance moves; an IP change is a client-visible outage. |

Everything else KubeSwift does — fresh boots, pools, snapshots, GPU
workloads, *offline migration where an IP change is acceptable* — is fine
on Tier A. **Tier C is opt-in, per guest, via a network attachment.** It
is never the default; most guests do not need it and should not pay the
operator cost.

## 4. KubeSwift's integration surface is already correct

A key finding: KubeSwift does **not** need to pick one Tier-C technology
at the code level. The integration point is already the right
abstraction — a guest interface references a **Multus
NetworkAttachmentDefinition (NAD)**:

```yaml
spec:
  interfaces:
    - name: app
      networkRef: { name: tenant-l2 }   # any NAD; macvlan, OVN-K L2, or UDN
```

All three Tier-C options (§5) are **just different NAD types behind this
same `networkRef`**. macvlan, OVN-K layer-2, and OVN-K UDN each ship a
NAD; KubeSwift attaches the launcher pod to it via the existing Multus
path (`MultusAnnotationKey`, see `internal/controller/swiftguest/`).
swiftletd's tap/bridge logic is bypassed for that interface — the guest
NIC rides the NAD.

This means the design work is **thin at the code layer** and concentrated
in three gaps (§7): the *primary* interface can't yet ride a NAD, the
webhook trusts `networkRef != nil` without verifying multi-node
capability, and there's no validated operator recipe or preflight.

## 5. The three Tier-C options

### 5.1 Multus + macvlan / bridge on a shared physical NIC

The guest's NIC is a macvlan (or Linux-bridge) sub-interface of a
**physical NIC that is on the same L2 segment on every node** (typically
a dedicated NIC or a tagged VLAN the operator controls).

- **Pros:** lightest; reuses the Multus/NAD machinery KubeSwift already
  has; no new CNI; near-native performance (macvlan has almost no
  overhead); the operator already understands the L2.
- **Cons:** requires an operator-controlled L2 segment present on **every**
  candidate node — a shared physical NIC or VLAN trunk. macvlan uses a
  fresh per-endpoint MAC, which **cloud and many hosting networks reject**
  (MAC filtering / port security — e.g. Hetzner's public network blocks
  unknown MACs; AWS/GCP block macvlan on the primary ENI). IPAM is the
  operator's problem (static, or a DHCP server on the L2, or Whereabouts).
- **Best fit:** on-prem / bare-metal with a controllable L2 or a private
  VLAN; lab and telco edge.

### 5.2 OVN-Kubernetes layer-2 secondary network

OVN-K creates a **logical switch that spans all nodes** (an overlay L2);
the guest's NIC attaches to it via a `k8s.cni.cncf.io/v1` NAD with
`topology: layer2`. The IP/MAC live in OVN's logical space and are
delivered to whichever node the pod runs on, over the existing tunnel
fabric (Geneve).

- **Pros:** **no physical-NIC dependency** — it's an overlay, so it works
  on clouds and MAC-filtered networks where macvlan can't; built-in IPAM;
  spans arbitrary nodes; this is the **KubeVirt-proven path** for
  live-migration-with-IP-preservation. Portable across environments.
- **Cons:** requires **OVN-Kubernetes** as the cluster CNI (or as a
  secondary-network provider). On a Calico/Cilium cluster (like the dev
  cluster) that's a CNI migration/addition — a significant operator
  change. Geneve overlay adds a small per-packet cost vs macvlan.
- **Best fit:** the general-purpose, portable recommendation; any cluster
  already on (or willing to adopt) OVN-K.

### 5.3 OVN-Kubernetes user-defined networks (UDN)

The newest OVN-K surface: a **per-namespace (or cluster-scoped) network**
declared with a `UserDefinedNetwork` CR, giving native multi-tenancy with
cross-node L2 isolation — a tenant's guests share an isolated segment no
matter where they run.

- **Pros:** first-class multi-tenancy and isolation; the cleanest model
  for a multi-tenant VM platform; can make the UDN the guest's **primary**
  network (not just a secondary NIC).
- **Cons:** requires OVN-K at a **recent version** with UDN enabled;
  newest/least-battle-tested surface; largest scope.
- **Best fit:** multi-tenant deployments and the long-term direction; not
  v1.

## 6. Recommendation

**Keep the `networkRef` → NAD abstraction as the single integration point
(do not bake a technology into KubeSwift), and adopt a tiered operator
recommendation:**

1. **Reference / preferred multi-node L2: OVN-Kubernetes layer-2
   secondary network (§5.2).** It is portable (overlay, no physical-NIC
   or MAC-filtering constraints), has built-in IPAM, and is the
   KubeVirt-proven path for IP-preserving live migration. Ship a
   validated recipe and make it the documented default for
   IP-preserving migration.
2. **Lightweight alternative: Multus + macvlan/bridge (§5.1)** for on-prem
   / bare-metal operators who already control an L2 segment and want
   near-native performance with no new CNI. Document the MAC-filtering
   caveat prominently.
3. **Forward-looking: OVN-K UDN (§5.3)** for multi-tenancy — track as the
   direction for a future multi-tenancy phase, not v1.

Because all three sit behind the same `networkRef`, an operator picks the
technology and KubeSwift's code is identical. KubeSwift's job is to (a)
let any interface — including the primary — ride a NAD, (b) verify the
attachment is genuinely multi-node-capable before promising IP
preservation, and (c) ship the recipes and preflight.

**Validation reality (honest, like SR-IOV):** the dev cluster is k0s +
Calico on Hetzner bare-metal with no second NIC and public-network MAC
filtering. So **macvlan can't be validated here** (MAC filtering) and
**OVN-K isn't deployed** (Calico). Tier-C cluster validation is
**infra-gated** — the same posture as SR-IOV (code + recipe ship;
hardware/infra validation deferred). The design and the webhook/preflight
logic are fully unit-testable; an end-to-end IP-preserving live migration
needs a cluster with a real Tier-C attachment (an OVN-K lab, or a private
VLAN for macvlan).

**Validation outcome (2026-06, hand-rolled VXLAN-mesh substrate).** A lab
VXLAN mesh (Recipe B in
[`../networking/multi-node-l2.md`](../networking/multi-node-l2.md)) let us
validate most of this on the Calico cluster after all: the **primary-on-NAD
datapath**, **cross-node L2 reachability**, and **offline migration
IP-preservation** are all cluster-proven. It surfaced two real KubeSwift
live-migration bugs (both would break NAD live migration on *any* Tier-C
network) — fixed in #235 (the dst pod dropped its Multus annotation →
`DstNeverReady`) and #236 (the dst-receiver-not-ready send-retry) — plus a
dnsmasq shared-L2 hardening (#237). **End-to-end live-migration
IP-preservation remains OVN-K-gated**: on the hand-rolled mesh both migration
pods are multi-homed across two overlays (the mesh + Calico), which
intermittently breaks the migration channel (isolation: only the NAD↔NAD case
fails). A real OVN-K layer-2 manages the attachment in-CNI and would not hit
this — so §3's marquee live-migration capability is the one piece that still
needs the OVN-K lab. Full diagnosis:
`docs/design/multi-node-l2-validation-spike.md` (local).

## 7. Implementation gaps (what code work this implies)

The abstraction is right, so the work is small and well-scoped:

1. **Primary interface on a multi-node network.** Today the *primary*
   (DHCP/management) interface is always the node-local bridge; only
   *secondary* (`networkRef != nil`) interfaces can ride a NAD. For
   IP-preserving migration of the guest's *main* IP, the primary must be
   able to ride a NAD too. Design: allow `interfaces[0].networkRef` (or a
   guest-level `primaryNetworkRef`) to put the primary NIC on a Tier-C
   network, bypassing tap+br0+dnsmasq for it. swiftletd already attaches
   NAD interfaces; the change is in the resolver/pod-builder primary-NIC
   path and the IP-discovery (DHCP comes from the NAD's IPAM, not the
   node-local dnsmasq).

2. **Verify multi-node capability instead of trusting `networkRef != nil`.**
   The SwiftMigration webhook currently treats *any* `networkRef` as
   multi-node-capable (documented gap in
   [`../migration/networking-requirements.md`](../migration/networking-requirements.md)).
   A node-local NAD would pass admission but silently lose the IP. Close
   it with an explicit signal — preferred: a **capability label/annotation
   on the NAD** (`kubeswift.io/multi-node-l2: "true"`) that the operator
   sets when they attest the attachment spans nodes, which the webhook
   reads. (NAD-content introspection — parsing `topology: layer2` etc. —
   is brittle across macvlan/OVN-K/UDN; an explicit attestation is the
   PR #26 "default-to-explicit" discipline.)

3. **`swiftctl migrate --check` / preflight + operator recipes.** A
   pre-flight that confirms the guest's interface is on a multi-node-L2
   attachment (and warns otherwise), mirroring the Phase-1 target-node
   Ready check. Plus a validated recipe per option (OVN-K L2 first,
   macvlan second) under `docs/networking/`.

4. **Promote Constraint 6** in [`live-migration.md`](live-migration.md) to
   a one-line pointer at this document (done in spirit by this doc; the
   constraint text stays as the migration-local statement).

These are individually small (the heavy lifting is OVN-K being present in
the cluster, which is the operator's, not KubeSwift's). Sequencing and PR
breakdown belong in a follow-up implementation plan once an option is
green-lit for a validation environment.

## 8. What composes with what

- **Live/offline migration:** Tier C is the prerequisite for IP
  preservation. Without it, migration still works but the guest gets a new
  IP (gated by `spec.allowIPChange`). Cross-reference:
  [`live-migration.md`](live-migration.md) Constraint 6,
  [`../migration/networking-requirements.md`](../migration/networking-requirements.md).
- **SR-IOV:** an SR-IOV NIC is node-bound hardware; it is **not** a Tier-C
  path and is rejected for cross-node migration (Phase 4+). Tier C is the
  software-overlay answer; SR-IOV is the hardware-passthrough answer for
  throughput, and the two are orthogonal.
- **vhost-user-net** ([`../virtiofs.md`](../virtiofs.md)): an
  operator-backend NIC; its cross-node story is the backend's, not
  KubeSwift's — orthogonal to Tier C.
- **Multi-NIC** ([`../multi-nic.md`](../multi-nic.md)): the existing NAD
  secondary-interface machinery is exactly the Tier-C attachment surface;
  this doc generalizes it to the primary NIC and to migration semantics.
- **Multi-tenancy (future):** UDN (§5.3) is the direction.

## 9. Cross-references to add (per existing per-feature docs)

When this design is acted on, add a pointer to this doc from:
[`../networking/README.md`](../networking/README.md),
[`../networking/ovn-kubernetes.md`](../networking/ovn-kubernetes.md),
[`../networking/sriov.md`](../networking/sriov.md),
[`../multi-nic.md`](../multi-nic.md),
[`../migration/networking-requirements.md`](../migration/networking-requirements.md),
and [`live-migration.md`](live-migration.md) Constraint 6.
