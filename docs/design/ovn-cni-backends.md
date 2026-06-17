# RFC: Pluggable OVN CNI backends (OVN-Kubernetes as a second substrate)

> Status (2026-06-17): **SHIPPED (P1+P2 merged #243/#244; P3 cluster-validated
> 2026-06-17).** OVN-Kubernetes is now a fully-supported second OVN substrate: a
> SwiftGuest whose **primary** NIC rides an OVN-K `layer2` NAD is reachable
> cross-node on its own IP and **preserves that IP across a `mode: live` migration
> with no `allowIPChange`** — the same capability v0.4.5 shipped on kube-ovn, now on
> a second substrate. P1 (#243) lifted the kube-ovn logic behind the pluggable
> `ovnBackend` seam; P2 (#244) added `ovnKubernetesBackend`; **P3 validated the full
> stack on a real OVN-K-primary cluster** (a real RWX+Block disk-boot Ubuntu-Noble
> guest booted reachable cross-node and live-migrated IP-preserving in **2.806 s** —
> see the **Validation** callout below and the validated §8 P3 row). The pivotal
> unknown (OVN-K non-KubeVirt *live* IP-keep) was retired by the P0 spike: OVN-K
> gets it via the NPWG `IPAMClaim` path, *simpler* than kube-ovn (no
> `migrationJobName`-equivalent needed — §3 / §8). The two decisions that needed
> the user's call — **(c) dual-substrate maintenance** and **(d) supported-vs-
> advanced tiering** — were signed off 2026-06-17 → "full first-class support".
> Operator install guide: [`ovn-kubernetes-install.md`](../networking/ovn-kubernetes-install.md).
> UDN-primary multi-tenancy remains a separate later phase (P5 / R6). Author:
> staff-architect.

> ### Validation (P3, 2026-06-17 — SHIPPED evidence)
>
> Validated on a dedicated **OVN-Kubernetes-primary** cluster (3 libvirt VMs
> `ntx1/2/3`, k0s v1.34.3, nested virt, Longhorn with a `migratable` StorageClass,
> KubeSwift image `sha-12dad1e`). A **real RWX+Block disk-boot Ubuntu-Noble**
> SwiftGuest `ovnk-vm`, primary on an `ovn-l2` NAD (`ovn-k8s-cni-overlay` `layer2`
> `allowPersistentIPs: true`, subnet `10.20.0.0/16`):
>
> - **Boot:** the controller stamped the guest `mac` + `ipam-claim-reference` into
>   the Multus networks annotation; `IPAMClaim ovnk-vm-net1` was created+owned
>   (`ips: ["10.20.0.1/16"]`); `network-init` re-MAC'd `net1` off the guest MAC (the
>   #240 datapath); the guest came up **Running, IP `10.20.0.1`, reachable
>   cross-node** (ping from a pod attached to `ovn-l2` on another node, 0% loss).
> - **Live migration:** `mode: live` ntx2→ntx1, **no `allowIPChange`** →
>   **Completed, downtime 2.806 s, IP preserved `10.20.0.1`**, reachable from a
>   **third** node (ntx3) at 0% loss.

## 0. TL;DR

v0.4.5 shipped IP-preserving live migration on **kube-ovn** (`kubeovn/kube-ovn`)
via two seams: a controller pod-annotation **stamp**
([`kubeovn.go`](../../internal/controller/swiftguest/kubeovn.go) → launcher pod;
[`dst_pod.go::mergeAnnotationsForDst`](../../internal/controller/swiftmigration/dst_pod.go)
→ migration dst pod) and a **CNI-agnostic datapath re-MAC** in
[`network-init.sh::setup_primary_nad_nic`](../../rust/swiftletd/scripts/network-init.sh).

This RFC adds **OVN-Kubernetes** as a *second* OVN substrate and refactors the
stamp seam into a minimal pluggable `ovnBackend` interface (two implementations:
`kubeOVNBackend` = behavior-preserving refactor of today's code; `ovnKubernetesBackend`
= new). The two substrates serve different operator postures:

- **kube-ovn, NON-primary** = "keep your Calico/Cilium" — a Multus secondary NAD
  on a kube-ovn subnet. The v0.4.5 shipped path. No CNI replacement.
- **OVN-Kubernetes, PRIMARY** = "OVN-native" — a Multus secondary NAD on an OVN-K
  `layer2` network (incl. UDN-secondary), with **IP-preserving migration offline +
  LIVE** (Mechanism B, spike-proven §8) and localnet/provider VLANs. Targets a
  cluster already running OVN-K as its CNI (no Calico-rebuild forced on the dev
  cluster — the spike used a dedicated OVN-K cluster, §10-e). **UDN-*primary*
  multi-tenancy is a separate later phase** (different datapath; R6).

**The datapath re-MAC carries over unchanged** (the OVN-port-binds-to-pod-NIC
problem is generic to any OVN CNI). **The migration IP-keep does NOT carry over
as-is** — it needs an OVN-K-specific mechanism (NPWG `IPAMClaim`, not kube-ovn's
`migrationJobName`). That asymmetry was the entire risk of this RFC (§3) and the
**P0 spike has now resolved it: PASS** (§8 / spike doc).

**Feasibility verdict (one paragraph).** Adding OVN-K as a second substrate is
**feasible and worth it** across the full feature set — the datapath, reachability,
UDN-secondary / localnet use cases, AND IP-preserving migration both **offline and
LIVE**. The **IP-preserving *LIVE* migration** marquee was the one genuinely-
uncertain piece in the original RFC: the research (§3) showed OVN-K's headline
live-migration IP-keep is **shaped around KubeVirt** (Mechanism A) in a way
kube-ovn's is not, and the clean non-KubeVirt path (NPWG `IPAMClaim` /
`ipam-claim-reference`, Mechanism B) was documented for *IP-reuse-across-pod-
recreation* but **not documented for the src/dst simultaneous-overlap a live
cutover needs**. **The P0-c spike answered that empirically — YES (§8):** two pods
on different nodes referencing the **same `IPAMClaim`** simultaneously is **allowed
by default** (the dst comes up with the IP while the src still holds it; cutover
clean; IP reachable cross-node from a 3rd node), with **no migration-marker
annotation** — *simpler* than kube-ovn. So: ship OVN-K-primary for everything-
including-live-IP-keep with confidence. The one residual unknown is now a smaller,
P2-resolvable implementation detail — the OVN-K **identity** annotation (the
`mac_address` equivalent that programs the OVN logical-switch-port to BE the guest;
§3.5 / §8). It does not block the RFC.

---

## 1. Problem & goal

### 1.1 Problem

KubeSwift's only validated multi-node-L2 / IP-preserving-migration substrate is
**kube-ovn as a Multus secondary** (v0.4.5). Two gaps:

1. **No OVN-K-primary support.** Operators standardizing on **OVN-Kubernetes as
   their cluster CNI** (OpenShift, upstream OVN-K clusters, telco) cannot get
   KubeSwift's portable-IP / migration story without *also* installing kube-ovn
   as a parallel overlay — an awkward two-OVN deployment.

2. **The integration is kube-ovn-shaped, hard-coded.** The detector keys on the
   literal NAD `config.type == "kube-ovn"`; the annotation keys are kube-ovn's
   `<provider>.kubernetes.io/{mac,ip}_address`; the migration trick is kube-ovn's
   read of `kubevirt.io/migrationJobName`. None of it generalizes to a second OVN
   CNI without a branch.

### 1.2 Goal

- Support **OVN-Kubernetes (primary CNI)** as a second OVN substrate for: primary
  -on-NAD portable IPs, the node-local datapath regression, and **IP-preserving
  migration both offline and LIVE** (Mechanism B / `IPAMClaim` — **spike-proven**,
  §3 / §8; no longer gated).
- Refactor the existing seam into a **pluggable `ovnBackend`** so a *third* OVN
  CNI (or a future OVN-K topology) is a new small implementation, not a new branch
  through the controller. The bar is **two implementations** — not a framework.
- **Unlock OVN-K use cases** (§5): **UDN-*secondary* networks** (proven, transparent
  to the backend) and localnet/provider networks. (UDN-*primary* multi-tenancy is a
  **separate later phase** — different datapath, node-stability caveat — §4.4 / R6.)
- **Do NOT replace kube-ovn.** Both substrates stay supported; they target
  different operator postures (§0).

### 1.3 Non-goals

- Importing or depending on any OVN-K Go types (`k8s.ovn.org/*`). KubeSwift stays
  coupled only to the **Multus NAD** surface (`k8s.cni.cncf.io`), reading NADs
  unstructured — exactly as today. (UDN/CUDN are detected via the NADs OVN-K
  *generates* from them, not by reading the UDN CRD — see §4.4.)
- A KubeVirt dependency. If the live-IP-keep spike forces KubeVirt-object
  synthesis, that is a §10 decision, not a default.
- Making OVN-K-primary the *default* or *recommended* operator path on day one
  (§10-d). It enters as a validated, documented option; kube-ovn-non-primary
  stays the low-friction default.

---

## 2. What carries over vs. what's new

Grounded in a read of the three shipped seam files.

| Shipped piece | File | Carries to OVN-K? | Why |
|---|---|---|---|
| **Datapath re-MAC** (re-MAC pod NIC off guest MAC before enslaving) | `network-init.sh::setup_primary_nad_nic` (lines 221-243) | **YES, unchanged** | Gated purely on `nic_mac == guest_mac`. The "OVN LSP binds to the pod-NIC identity; bridging the guest MAC behind it makes a shadowing fdb entry" problem is **generic to any OVN CNI**. OVN-K binds its LSP the same way kube-ovn does. The re-MAC is already CNI-agnostic by construction. |
| **`primary`-on-NAD field + resolver + `setup_primary_nad_nic` structure** | `swiftguest_types.go::PrimaryInterface`, `PrimaryIPPreservedCrossNode`; the read→flush→bridge→serve→discover datapath | **YES, unchanged** | None of it is kube-ovn-specific. It already validated on a hand-rolled VXLAN mesh *and* kube-ovn. OVN-K layer2 is just another NAD whose IPAM hands the pod NIC an IP. |
| **Multus networks request-annotation preservation on dst pod** | `dst_pod.go::mergeAnnotationsForDst` (the `MultusAnnotationKey` block, lines 298-300) | **YES, unchanged** | The dst pod needs the same `k8s.v1.cni.cncf.io/networks` request regardless of which CNI backs the NAD. |
| **The seam call sites** | `controller.go:481` (`stampKubeOVNIdentity`), `dst_pod.go:296` (`mergeAnnotationsForDst`) | **YES — becomes the interface boundary** | These two call sites are exactly where the `ovnBackend` interface plugs in (§3). |
| **NAD-type detection** | `kubeovn.go::kubeOVNPrimaryProvider` (keys on `config.type == "kube-ovn"`) | **NO — needs OVN-K detector** | OVN-K NADs are `type: ovn-k8s-cni-overlay` (+ `topology: layer2`/`localnet`). New detector. |
| **Identity annotations** (stamp the guest MAC/IP onto the launcher pod) | `kubeovn.go::stampKubeOVNIdentity` (`<provider>.kubernetes.io/{mac,ip}_address`) | **NO — needs OVN-K form** | OVN-K's portable-IP request is **not** kube-ovn's per-provider annotation. It is either the NPWG `ipam-claim-reference` (secondary layer2) or OKEP-5233 preconfigured-address annotations (primary L2 UDN). See §3/§4. |
| **Live-migration IP-keep trick** | `dst_pod.go` (`kubevirt.io/migrationJobName` → kube-ovn skips IPAM conflict check) | **NO — OVN-K needs a different (simpler) mechanism** | Was the pivotal unknown; **P0-c spike PASS (§3 / §8).** OVN-K: src+dst reference the **same `IPAMClaim`**; overlap is allowed **by default** (no marker annotation). The backend create/GCs the claim; `mergeAnnotationsForDst` carries `ipam-claim-reference` onto the dst (the #235 pattern). |

**Read of the asymmetry:** ~70% of the shipped work (the entire datapath, the
field/resolver/migration-gate control plane, the dst-pod annotation-preservation
machinery, both seam call sites) is reusable verbatim. The OVN-K-specific new
code is a **detector + an identity/migration backend** — which is precisely the
scope the `ovnBackend` interface is sized to.

---

## 3. The pivotal unknown — OVN-K live-migration IP-keep (RESOLVED — P0-c PASS)

> **Resolution (2026-06-17): the spike answered this empirically — PASS.** OVN-K
> *does* deliver non-KubeVirt live IP-keep, via **Mechanism B (`IPAMClaim`)** —
> and the src/dst simultaneous-overlap a live cutover needs is **allowed by
> default**, with **no migration-marker annotation** (cleaner than kube-ovn). The
> research below stands as the design's reasoning; the spike's confirmation is in
> §3.2 ("the catch") and §8, full detail in
> [`ovn-cni-backends-spike.md`](ovn-cni-backends-spike.md).

**Question:** does OVN-Kubernetes expose an **annotation-drivable** VM-live-
migration IP-keep for a **non-KubeVirt** controller — the way kube-ovn does (where
the single `kubevirt.io/migrationJobName` annotation, with **no VMI object read**,
flips `AllowLiveMigration` so the dst shares the src's static IP through cutover)?

**Answer (research + spike): YES — via Mechanism B, and *not* the same shape as
kube-ovn.** The research (authoritative OVN-K + KubeVirt/NPWG docs, 2025–2026)
found THREE relevant mechanisms, split along the primary/secondary boundary, none
a drop-in equal of kube-ovn's one annotation. The cleanest (Mechanism B, NPWG
`IPAMClaim` on a layer2 secondary) was documented for IP-reuse-across-pod-recreation
but **silent on the live src/dst overlap** — so the RFC originally gated it behind
the spike. **The spike then proved the overlap works (§3.2 / §8): same-claim-both-
pods, overlap allowed by default, dst acquires the IP while src holds it.**

### 3.1 Mechanism A — primary-network live-migration IP-keep (KubeVirt-shaped)

OVN-K's headline "Kubevirt VM Live Migration" feature **preserves the IP on the
primary (default) network** and keeps TCP alive via point-to-point routes + DHCP-
served IP + ARP-proxy GW-MAC stability. To recognize a migratable VM pod it
requires:

- annotation `kubevirt.io/allow-pod-bridge-network-live-migration` **AND**
- label **`vm.kubevirt.io/name=<vm>`**, and it correlates the src vs dst
  virt-launcher pods via the **`kubevirt.io/vm=<vm>`** label + `CreationTimestamp`,
  then watches **`kubevirt.io/migration-target-start-timestamp`** /
  `kubevirt.io/nodeName` on the target.

**Crucial nuance (the good news):** the recognition is **from pod
labels/annotations only — NO VirtualMachine / VirtualMachineInstance *object* is
read** by `ovnkube-controller`. So it is *technically* label-drivable by a non-
KubeVirt controller that stamps the **full KubeVirt label set**. **The bad news:**
it is materially more coupling than kube-ovn (a *set* of `kubevirt.io/*` labels +
the migration-timestamp protocol vs. one annotation), it is the *primary* network
(replacing the default pod network with the KubeVirt bridge-binding model — not
KubeSwift's Multus-secondary-NAD model), and stamping fake `vm.kubevirt.io/name`
labels on non-KubeVirt pods is brittle (it rides an implementation detail, not a
contract; an OVN-K release could start expecting a real VMI).

### 3.2 Mechanism B — secondary-network persistent IPs (`IPAMClaim`, **non-KubeVirt**)

For **layer2 *secondary* networks**, OVN-K persists an allocated IP in an
`ipamclaims.k8s.cni.cncf.io` object when the NAD sets `allowPersistentIPs: true`
(equivalently `lifecycle: Persistent`) *and* `subnets` is defined. A pod claims a
persisted IP by referencing the claim via the **`ipam-claim-reference`** attribute
in its `k8s.v1.cni.cncf.io/networks` selection element — the **NPWG multi-network
de-facto standard v1.3 §4.1.2.1.11**. This is **architecture-agnostic**: the
KubeVirt `ipam-extensions` controller is just *one* client that creates the
`IPAMClaim` and sets `ipam-claim-reference`; **any controller (KubeSwift) can
create the claim and reference it itself.** This is the clean non-KubeVirt path.

**The catch the spike had to resolve — RESOLVED, PASS.** Persistent-IP is
*documented* only as IP-reuse across pod recreation (old pod gone → new pod with
the same claim → same IP). The sources did **not** confirm two pods (src + dst,
overlapping during a live cutover) may reference the **same** `IPAMClaim` and hold
the IP **simultaneously** — exactly what a *live* (not offline) migration cutover
needs. kube-ovn solves the overlap explicitly (it skips the IPAM conflict-check on
the `migrationJobName`-marked dst); OVN-K's persistent-IP docs were silent on it.
**The P0-c spike proved it empirically (§8):** pod B on a *different node*,
referencing the **same `IPAMClaim`**, created **while pod A still ran**, came up
Ready with **the same IP** — no conflict, no rejection, **no marker annotation
required** (overlap is allowed by default; *simpler* than kube-ovn). After deleting
A, B kept the IP and answered a 3rd-node probe at 0% loss. **One non-blocking nit:**
`IPAMClaim.status.ownerPod` goes **stale** post-cutover (there is no kubevirt-ipam-
claims controller to update it) — IP *delivery* follows the live pod regardless;
the OVN-K backend owns the claim lifecycle and updates/GCs `ownerPod` itself.
**Implementation contract this fixes:** per guest, the backend creates an
`IPAMClaim`, stamps `ipam-claim-reference: <claim>` on the launcher pod, and
`mergeAnnotationsForDst` preserves it on the dst pod (the #235 pattern) so src+dst
share the claim through cutover.

### 3.3 Mechanism C — OKEP-5233 PreconfiguredUDNAddresses (primary L2 UDN, forward-looking)

OVN-K added (actively integrated mid-2025; OpenShift `PreconfiguredUDNAddresses`
feature gate) a way for a pod on a **primary Layer2 UDN/CUDN** to request a
**static IP + MAC + default gateway** via annotation — explicitly built for
"migrate legacy VMs/pods with predefined network config into OVN-K, non-NATed."
This is the most KubeSwift-aligned primitive (static IP/MAC by annotation, no
KubeVirt), but it is **newer, feature-gated, primary-UDN-only**, and (like B)
its src/dst-overlap behavior during a live cutover is unverified. Treat as a
**secondary spike target** / future path, not the v1 bet.

### 3.4 Verdict feeding the design

| Path | Non-KubeVirt? | IP across pod-recreate (⇒ **offline** migration) | Simultaneous src/dst overlap (⇒ **live** migration) | KubeSwift fit |
|---|---|---|---|---|
| **A** primary KubeVirt-shaped | label-drivable, brittle | n/a (it IS the live path) | **yes, but requires KubeVirt label set + primary-net model** | poor (not our Multus-NAD model; brittle coupling) |
| **B** secondary `IPAMClaim` | **YES, clean (NPWG std)** | **YES (documented)** | **YES — spike-proven (§8); allowed by default, no marker** | **best fit** (Multus-secondary, our model) |
| **C** primary L2 UDN preconfigured-addr | **YES** | likely yes | unverified (not spiked — Mechanism A/B make it unnecessary for v1) | good but newer/feature-gated |

**Design bet — CONFIRMED.** Target **Mechanism B (`IPAMClaim` on a layer2
secondary)** as the v1 OVN-K backend. It matches KubeSwift's existing
Multus-secondary-NAD model exactly, is a clean non-KubeVirt standard, and the
spike proved it delivers **both OFFLINE and LIVE** IP-preserving migration. The
§10-b fallback (offline-only on OVN-K / Mechanism-A KubeVirt-label synthesis /
Mechanism C) is **no longer needed** — Mechanism B does the full job, so it is
retired (§10-b).

### 3.5 The one residual unknown — the OVN-K identity (port-binding) mechanism (P2 detail, NOT a blocker)

The IP-keep question (above) is the marque risk and it is resolved. One smaller
question is **not yet spiked** and is deferred to the P2 backend build (or a short
follow-on spike): **the OVN-K *identity* mechanism.** kube-ovn binds its logical-
switch-port to the pod-NIC MAC and runs an ARP responder, so KubeSwift's bridged
**guest** MAC has to be programmed onto the OVN port (the kube-ovn `mac_address`
annotation) AND the #240 re-MAC applied for L2 to reach the guest. **Does OVN-K
bind its LSP the same way?** Almost certainly (it is generic OVN behaviour), in
which case the **#240 datapath re-MAC carries unchanged** (it is CNI-agnostic by
construction — §2) and the OVN-K backend additionally needs to put the guest MAC
into the OVN-K-side identity slot — likely the NAD/pod `mac` field of the Multus
selection element or a `k8s.ovn.org/...` annotation (the `mac_address` equivalent).

**Why this does NOT block the RFC:** it is an `Identity()`-implementation detail
fully contained in `ovnKubernetesBackend`, with a cheap disproof (a foreign MAC
behind `net1` → cross-node reachable?) that either runs as a short P2 spike or
simply falls out of the P2 backend bring-up. The interface already carries the
guest MAC through `PodAnnotations`; only the *key/shape* is open. Tracked as the
known residual unknown in §7 (V3) and §8 (P2).

---

## 4. The pluggable `ovnBackend` interface

### 4.1 Design principles applied

- **Minimalism / "two implementations is the bar."** The interface is the *least*
  abstraction that lets `kubeOVNBackend` and `ovnKubernetesBackend` coexist behind
  one seam. No registry, no plugin loader, no config — a slice of two backends,
  first-match-wins on `Detect`.
- **Right layer.** Detection + identity-annotation policy is **controller** logic
  (it reads NADs, stamps pods). The datapath stays in **shell** (`network-init.sh`,
  already CNI-agnostic). Nothing new in **Rust/RuntimeIntent** — the runtime
  contract is unchanged (a NAD primary NIC is a NAD primary NIC to swiftletd).
- **No silent failures.** `Detect` returning an error (NAD Get failure) fails
  closed and requeues (today's behavior). A detected-but-misconfigured OVN-K NAD
  (e.g. layer2 without `allowPersistentIPs` when IP-preservation is required)
  surfaces a **condition**, not a silent boot of an unreachable/non-portable guest.

### 4.2 The interface (Go sketch)

Package `internal/controller/swiftguest` (lives with the call site; no new
package — minimalism). `ovnBackend` is unexported; it is an internal seam, not a
public API.

```go
// ovnIdentity is what a backend computes for a guest whose primary rides one of
// its NADs: the per-pod annotations that program the OVN port to BE the guest
// (so OVN delivers L2 to the guest behind the re-MAC'd pod NIC), plus the
// migration-overlap annotations the dst pod needs.
type ovnIdentity struct {
    // PodAnnotations to add to the launcher pod (identity: MAC, optional IP).
    PodAnnotations map[string]string
    // MigrationDstAnnotations to add to the live-migration DST pod so it keeps
    // the src's IP through cutover. kube-ovn: the IP key + kubevirt.io/migrationJobName.
    // ovn-kubernetes: the same ipam-claim-reference the src carries (the shared
    // claim IS the overlap mechanism — spike-proven §8 — so this is just the #235
    // request-annotation carry-over, no extra marker). Empty only if a backend has
    // no live-overlap mechanism (⇒ live migration offline-only; see §6 gate).
    MigrationDstAnnotations map[string]string
    // ClaimsToEnsure are cluster objects the backend needs created before the
    // pod (e.g. an IPAMClaim for OVN-K mechanism B — created+GC'd per guest,
    // since OVN-K does NOT auto-create it; spike §8). Nil for kube-ovn.
    ClaimsToEnsure []ovnClaim // unstructured spec + GVK; reconciler ensures+owns
}

type ovnBackend interface {
    // Name is for logs/conditions ("kube-ovn", "ovn-kubernetes").
    Name() string
    // Detect inspects the guest's PRIMARY NAD config. ok=true means "this
    // backend owns this guest's primary network". A NAD Get failure returns
    // err (fail closed). ok=false,nil means "not mine" → next backend.
    Detect(ctx context.Context, c client.Client, guest *SwiftGuest) (ok bool, err error)
    // Identity computes the launcher-pod + migration-dst annotations (and any
    // claims to ensure) for a guest this backend Detected. guest.Status carries
    // the assigned IP once known (for IP pinning), same as today.
    Identity(ctx context.Context, c client.Client, guest *SwiftGuest) (ovnIdentity, error)
}
```

### 4.3 The two implementations

- **`kubeOVNBackend`** — a **behavior-preserving** lift of today's
  [`kubeovn.go`](../../internal/controller/swiftguest/kubeovn.go).
  `Detect` = `config.type == "kube-ovn"` (the existing `kubeOVNPrimaryProvider`
  type check). `Identity` returns `PodAnnotations` = the two `<provider>.kubernetes.io/{mac,ip}_address`
  keys, `MigrationDstAnnotations` = the IP key + `kubevirt.io/migrationJobName`,
  `ClaimsToEnsure` = nil. **The existing kube-ovn unit tests must pass unchanged**
  — that's the refactor's correctness gate.
- **`ovnKubernetesBackend`** — **new**. `Detect` = `config.type ==
  "ovn-k8s-cni-overlay"` (optionally narrowed to `topology: layer2` for v1, per
  §10-a). `Identity` (Mechanism B, **spike-proven §8**): ensure (create+own) an
  `IPAMClaim` for the guest; return `PodAnnotations`/`MigrationDstAnnotations` that
  put `ipam-claim-reference: <claim>` into the **per-NAD entry of the
  `k8s.v1.cni.cncf.io/networks` annotation** — note OVN-K's identity rides *inside*
  the Multus networks selection element, NOT a flat `<provider>.kubernetes.io/*` key
  like kube-ovn (a real shape difference the interface absorbs). For the **live
  overlap, no extra marker is needed** — src and dst sharing the same claim IS the
  overlap (allowed by default; spike §8), so `MigrationDstAnnotations` is just the
  same claim reference (the #235 carry-over). The guest MAC must also be programmed
  onto the OVN logical-switch-port so its identity is the guest — the **one residual
  P2 detail** is the exact OVN-K-side form of that (a NAD/pod `mac` field or a
  `k8s.ovn.org/...` annotation; the kube-ovn `mac_address` equivalent), to be pinned
  during the P2 build or a short follow-on spike (§3.5 / §8). The #240 datapath
  re-MAC carries unchanged.

### 4.4 Detection ordering & UDN/CUDN

- The reconciler holds `backends := []ovnBackend{kubeOVNBackend{}, ovnKubernetesBackend{}}`
  and calls `Detect` in order; first `ok` wins. (`kube-ovn` and `ovn-k8s-cni-overlay`
  are disjoint `type`s, so order is immaterial for correctness — it's just a stable
  iteration.)
- **UDN/CUDN are detected via their generated NADs, not the CRDs.** A
  `UserDefinedNetwork` / `ClusterUserDefinedNetwork` causes OVN-K to *generate* a
  NAD (`type: ovn-k8s-cni-overlay`) in each selected namespace; KubeSwift's
  `networkRef` points at that generated NAD and `ovnKubernetesBackend.Detect`
  matches it like any other OVN-K NAD. **No `k8s.ovn.org` type import** (non-goal
  §1.3). This keeps the §5 use cases inside the existing coupling.

- **Crucial UDN distinction the spike surfaced (secondary vs primary).** UDN has
  two roles, and only ONE rides the generated-NAD seam transparently:
  - **UDN *Secondary* — proven end-to-end (spike §8), transparent LOW-risk add.** A
    `UserDefinedNetwork` (topology Layer2, role Secondary) reconciles, generates a
    Multus NAD, and a pod attaches and gets an IP. **This is the KubeSwift-relevant
    mode for the `ovnBackend`:** KubeSwift rides the generated NAD via
    `interfaces[].networkRef` exactly like the layer2 NAD proven in P0-c, so
    `ovnKubernetesBackend` handles it on the **same code path — zero new
    integration**. It folds into the backend once the segmentation gate is on.
  - **UDN *Primary* — a SEPARATE, later capability (NOT in the core OVN-K
    backend).** Guest-on-the-pod-PRIMARY-network is a *different* datapath /
    integration than a secondary NAD (the guest would ride the pod's primary
    network, not a secondary `networkRef`), and the spike could only reconcile it at
    the control plane — the **node datapath was NOT proven** on the small 3-VM
    cluster (ovnkube-node crashlooped on UDN-network bring-up; see §9 R6). Phase it
    separately and validate on a **properly-resourced OVN-K cluster**; do not assume
    it here.
- **`--enable-network-segmentation` is a cluster-config prerequisite, not KubeSwift
  code, and not a free flip.** UDN of either role requires the OVN-K
  `--enable-network-segmentation` gate (a redeploy flag). The spike showed enabling
  it on a small/k0s OVN-K cluster **can destabilize the node datapath** (§9 R6).
  Operators enabling it need a robustly-resourced OVN-K install; the RFC flags this
  rather than treating UDN as a free flip.

### 4.5 Seam wiring (the two call sites become interface dispatch)

- `controller.go:481` `stampKubeOVNIdentity(...)` → `r.stampOVNIdentity(...)`:
  iterate backends, `Detect`, on match `Identity`, ensure `ClaimsToEnsure`, apply
  `PodAnnotations` to `desiredPod`. Same fail-closed-on-Get-error semantics.
- `dst_pod.go::mergeAnnotationsForDst` → the kube-ovn-suffix loop becomes "ask the
  guest's backend for `MigrationDstAnnotations`." Since `mergeAnnotationsForDst`
  currently keys off the **src pod's** stamped annotation (no backend object in
  scope there), the cleanest move is: the SwiftMigration controller resolves the
  backend from the **guest** (it already has the guest) and passes the
  `MigrationDstAnnotations` map into `newDstPod`, replacing the suffix-sniffing
  loop. (Mechanically small; preserves the "keyed off what the src carries"
  inertness for non-OVN guests by returning an empty map when no backend matches.)

This is the whole refactor: **one new interface, two impls, two call sites
rewired.** No CRD change, no RuntimeIntent change, no Rust change.

---

## 5. Use cases unlocked by OVN-K-primary

Which genuinely need **OVN-K-primary** vs. are already achievable on kube-ovn-non-
primary:

| Use case | Needs OVN-K-primary? | Spike status | Notes |
|---|---|---|---|
| **UDN *Secondary* network on a VM** — isolated Layer2 secondary, generated NAD | **YES (segmentation gate)** | **PROVEN (§8)** | `UserDefinedNetwork` role=Secondary → generated NAD; KubeSwift rides it via `networkRef` on the **same `ovnKubernetesBackend` code path — zero new integration** (§4.4). The transparent UDN win. |
| **UDN / CUDN *Primary* native multi-tenancy** — per-tenant isolated *primary* VM networks, isolation at the CNI even same-node | **YES** | **control-plane only — DATAPATH NOT proven (§8 / §9 R6)** | A **DIFFERENT datapath** than secondary-NAD (guest on the pod's PRIMARY network). Control plane reconciled on the spike cluster; ovnkube-node crashlooped bringing the UDN networks up on the small 3-VM cluster. **SEPARATE, later phase** needing a robust OVN-K cluster — not in the core backend. Samples staged: [`udn-tenant-isolation.yaml`](../../config/samples/multi-nic/udn-tenant-isolation.yaml), [`cudn-shared-network.yaml`](../../config/samples/multi-nic/cudn-shared-network.yaml). |
| **localnet / provider networks** — VLAN-tagged L2 onto physical infra for telco/NFV, legacy-network bridging | **YES (for OVN-K-managed localnet)** | not spiked (V6 stretch) | OVN-K `topology: localnet` + OVS bridge mappings ([`nad-ovn-localnet.yaml`](../../config/samples/multi-nic/nad-ovn-localnet.yaml)). kube-ovn has its own underlay/vlan, but the OVN-K-native localnet+UDN integration is the OVN-K-primary draw. |
| **KubeVirt-parity primary-network live migration** | **YES** | n/a — not pursued | Mechanism A (§3.1) is a primary-network OVN-K feature. **No longer relevant** — Mechanism B delivers live IP-keep without it (§3 / §10-b retired). |
| **OVN-K-native NetworkPolicy / multi-network-policy on the VM primary** | **YES** | not spiked | Operators on OVN-K want their existing policy model to cover VMs. |
| **Portable secondary IP / IP-preserving OFFLINE migration** | **NO** | PROVEN (Mechanism B, §8) | Already shipped on kube-ovn-non-primary (v0.4.5). OVN-K offers a *second* way (Mechanism B), not a new capability. |
| **IP-preserving LIVE migration** | **NO (capability exists on kube-ovn)** | **PROVEN on OVN-K (§8)** | Was the spike question — now a *second substrate* for an existing capability, proven via Mechanism B (and simpler than kube-ovn). |

**Framing for the user:** the value is now broad and largely de-risked. The
**transparent, proven** OVN-K wins are **UDN-secondary networks** and **IP-
preserving migration (offline AND live)** — both spike-proven, neither needing
anything beyond the v1 backend. **localnet/provider networks** and **OVN-K-native
NetworkPolicy** are additional OVN-K-primary draws (not separately spiked; V6
stretch). The one genuinely-separate, *not-yet-proven* item is **UDN-*primary*
multi-tenancy** (a different datapath, node-stability risk on small clusters) —
phased as its own later capability, NOT a v1 backend feature. **The RFC's core
value no longer hinges on any open unknown.**

---

## 6. The live-migration eligibility gate (no silent failure)

Today `PrimaryIPPreservedCrossNode()` (true ⇒ skip `allowIPChange`) keys only on
"primary has a `networkRef`." With two substrates of differing live-capability,
that's too coarse. Add a backend-aware refinement **without** changing the offline
story:

- **Offline** IP-preserving migration: works on **both** backends (kube-ovn today;
  OVN-K Mechanism B persists across pod-recreate with certainty). `PrimaryIPPreservedCrossNode()`
  stays correct for offline.
- **Live** IP-preserving migration: gated on the backend reporting a non-empty
  `MigrationDstAnnotations` (i.e. it HAS a live-overlap mechanism). **Both shipped
  backends report one** — kube-ovn (`migrationJobName` + IP) and OVN-K (the shared
  `ipam-claim-reference`, spike-proven §8) — so both **pass** the gate and support
  live IP-keep. The gate exists as the **no-silent-failure backstop** (Design
  Principle #6): a backend with *no* live mechanism (a future offline-only OVN CNI,
  or an OVN-K NAD misconfigured without `allowPersistentIPs`) returns an empty
  `MigrationDstAnnotations`, and the SwiftMigration webhook/controller **refuses
  `mode: live` with a clear message** (e.g. "primary network '<backend>' supports
  offline IP-preserving migration only; use mode: offline or set allowIPChange")
  rather than silently falling through to an IP change or a hung `DstNeverReady`.
  This mirrors the existing VFIO live-refusal pattern. A separate
  **misconfiguration condition** (§4.1: a detected OVN-K NAD lacking
  `allowPersistentIPs` when IP-preservation is requested) surfaces the operator
  error explicitly instead of booting a non-portable guest.

---

## 7. Validation matrix (the OVN-K-primary `ntx` cluster)

P3 validates on the **dedicated OVN-K-primary cluster `ntx`** (the dev cluster is
not rebuilt — §10-e), where OVN-K is the primary CNI. Everything KubeSwift does
node-locally is **pod-internal** (br0/tap0/dnsmasq/DNAT/egress-probe all live
inside the launcher netns), so it *should* be CNI-independent — but "should" is
why this is a matrix, not an assumption. **The P0 spike already proved the OVN-K
*mechanisms* at the raw-pod level (persistent-IP, src/dst overlap, UDN-secondary
NAD); P3 re-proves them with a real KubeSwift VM boot + live-migrate on the `ntx`
cluster (nested virt).** The matrix below is the P3 ship bar.

| # | Scenario | Expectation | Spike (raw-pod) | Risk if it fails |
|---|---|---|---|---|
| V1 | **Smoke-test regression** (`make smoke-test`, the 5 scenarios) on OVN-K-primary | All pass; disk/kernel/qemu/gpu/multi-nic behave as on Calico | not run (needs VM boot) | OVN-K's pod-netns setup (default-route, MTU, the pod's own primary iface) interferes with br0/NAT/dnsmasq. **Run this FIRST — it's the cheapest disproof.** |
| V2 | **Service-exposure** (S1 ClusterIP DNAT) + **egress probe** (S2) on OVN-K | DNAT reaches the VM; egress probe reports `ClusterServices` | not run | OVN-K's primary datapath differs from kube-proxy on the in-pod-DNAT path (the eBPF-egress caveat already tracked for Cilium may bite here) |
| V3 | **Primary-on-NAD reachability** — guest on an OVN-K layer2 NAD, cross-node | Guest reachable on its NAD IP from a pod on another node; `status.network.primaryIP` populated; re-MAC fires | partial (persistent-IP + cross-node reach proven raw-pod) | **The residual identity unknown (§3.5):** OVN-K LSP may not deliver L2 to the stamped guest MAC behind the re-MAC'd NIC until the OVN-K-side `mac` slot is set. Pin in P2; V3 confirms with a real bridged guest. |
| V4 | **OFFLINE** IP-preserving migration on OVN-K (Mechanism B) | Guest reacquires its NAD IP on the target node, no `allowIPChange` | **PROVEN** (claim persists across pod-recreate) | `IPAMClaim` not honored / claim-reference form wrong (de-risked by spike) |
| V5 | **LIVE** IP-preserving migration on OVN-K (the marquee) | dst keeps src IP through cutover, sub-few-second downtime, reachable from a 3rd node | **PROVEN** (same-claim overlap; 3rd-node reach post-cutover) | Mechanism is proven; P3 re-proves it on a real VM. The §6 gate makes any regression a correct refusal, not a silent break. |
| V6a | **UDN *Secondary* on a VM** — guest on a generated UDN NAD, cross-node reach | Guest reachable on the UDN-NAD IP; same `ovnKubernetesBackend` path as V3 | **PROVEN** (UDN-secondary NAD generated; pod attached + got IP) | Requires the `--enable-network-segmentation` gate on; transparent once on (§4.4) |
| V6b *(stretch / separate phase)* | **UDN *Primary* multi-tenancy** — two namespaces, isolated primary networks; **localnet** VLAN reachability | Per-tenant isolation holds; localnet guest reaches the physical VLAN | control-plane only — **node datapath NOT proven** (§8 / R6) | A **different datapath**; needs a robust OVN-K cluster (the 3-VM `ntx` ovnkube-node crashlooped). Validate as its own later phase, not the v1 ship bar. |

**V1 is the gate for "is OVN-K-primary even a viable KubeSwift substrate."** If the
node-local path regresses under OVN-K, that's a bigger finding than the migration
question and must be fixed before backend code matters. **V3-V5 + V6a are the P3
ship bar** (their mechanisms are spike-proven; P3 confirms on real VMs); **V6b is a
separate later phase**, not part of the v1 ship decision.

---

## 8. Phasing → PRs

**P0 is DONE.** The spike resolved §3.2's overlap question — **PASS** — so backend
code is unblocked. The one residual unknown (the OVN-K identity/`mac` slot, §3.5)
is an `ovnKubernetesBackend`-internal detail that P2 pins inline (or a short
follow-on spike), not a gate on starting the work.

| Phase | Deliverable | Gate / PR |
|---|---|---|
| **P0 — Spike — ✅ DONE (2026-06-17)** | On the OVN-K-primary `ntx` cluster (3 libvirt VMs, k0s 1.34.3, nested virt). **PASS:** (b) **Mechanism B offline** — a layer2 `allowPersistentIPs` NAD + a pre-created `IPAMClaim` + `ipam-claim-reference` persists the IP across pod-recreate; (c) **the pivotal live overlap** — two pods on different nodes referencing the **same `IPAMClaim`** simultaneously is **allowed by default** (dst gets the IP while src holds it; cutover clean; reachable from a 3rd node), **no marker annotation** — simpler than kube-ovn; **plus** UDN-secondary proven end-to-end (generated NAD + pod IP). **Surfaced:** OVN-K does NOT auto-create the `IPAMClaim` (the backend must), `status.ownerPod` goes stale post-cutover (advisory; backend owns it), UDN-*primary* node datapath crashlooped on this small cluster (separate phase, R6), and the OVN-K **identity** mechanism (§3.5) is the one not-yet-spiked residual. Findings doc: [`ovn-cni-backends-spike.md`](ovn-cni-backends-spike.md). | **Done. (c) PASS retired §10-b.** |
| **P1 — `ovnBackend` refactor (behavior-preserving)** | Introduce the interface; lift the shipped #239/#240 kube-ovn logic into `kubeOVNBackend`; rewire the two call sites. **kube-ovn unit tests pass unchanged; kube-ovn cluster behavior identical** — the existing kube-ovn cluster validation is the regression gate. Zero OVN-K code yet. | PR 1. A pure refactor of shipped behavior; regression-gated by the existing kube-ovn tests + a kube-ovn cluster re-validation. |
| **P2 — `ovnKubernetesBackend`** | New backend implementing the P0-proven Mechanism B (one mechanism for **both** offline and live — no separate live path needed): the `ovn-k8s-cni-overlay`/`layer2` detector; **`IPAMClaim` create+own+GC per guest** (OVN-K does not auto-create it); `ipam-claim-reference` (+ guest `mac`) in the Multus selection element; **`mergeAnnotationsForDst` carry-over** of the claim reference onto the dst pod (the #235 pattern — the live overlap); the §6 live-eligibility gate. **Pin the §3.5 identity slot here** (the `mac_address`-equivalent: NAD/pod `mac` or `k8s.ovn.org/...`) — short follow-on spike if it doesn't fall out of bring-up. RBAC for `ipamclaims.k8s.cni.cncf.io` (generated-NAD reads already covered by the existing `network-attachment-definitions` grant). UDN-*secondary* needs no extra code (rides the generated NAD, §4.4). | PR 2 (no PR 2b — Mechanism B is one path for offline+live). Unit-tested with fake NADs/claims. |
| **P3 — Cluster validation (`ntx`, real KubeSwift VMs) — ✅ DONE (2026-06-17)** | The §7 matrix on the OVN-K-primary `ntx` cluster (3 libvirt VMs, k0s 1.34.3, nested virt, image `sha-12dad1e`) — real VM boot + live-migrate, not raw-pod probes. **PASS:** a **real RWX+Block disk-boot Ubuntu-Noble** guest `ovnk-vm` on a `layer2` `allowPersistentIPs` NAD (subnet `10.20.0.0/16`) came up **Running, IP `10.20.0.1`, reachable cross-node** (V3 — the residual identity slot, the Multus selection-element `mac` field, confirmed: a foreign MAC behind `net1` is delivered to the guest); the controller created+owned `IPAMClaim ovnk-vm-net1` and `network-init` re-MAC'd `net1`. **LIVE** migration ntx2→ntx1 with **no `allowIPChange`** Completed in **2.806 s** with the IP **preserved (`10.20.0.1`) and reachable from a 3rd node** (V5). **Surfaced:** the stock `ovn-l2-nad.yaml` sample was missing `allowPersistentIPs: true` (the IPAMClaim prerequisite — fixed); a bare OVN-K cluster without the `snapshot.storage.k8s.io` CSI VolumeSnapshot CRDs historically crash-looped the controller (an optional snapshot prereq, not a runtime blocker — tracked separately for hard-dependency removal). UDN-secondary rides the same code path (V6a, transparent); **UDN-primary (V6b) remains a separate later phase** (R6). | Done. **SHIPPED.** |
| **P4 — Docs — ✅ DONE (2026-06-17)** | This RFC → "SHIPPED" with the P3 validation outcome (the **Validation** callout + the validated §8 P3 row). New operator install guide for OVN-K-primary ([`ovn-kubernetes-install.md`](../networking/ovn-kubernetes-install.md), sibling to the kube-ovn [`ovn-l2-install.md`](../networking/ovn-l2-install.md)); updated [`ovn-kubernetes.md`](../networking/ovn-kubernetes.md), [`multi-node-l2.md`](../networking/multi-node-l2.md); fixed the [`ovn-l2-nad.yaml`](../../config/samples/multi-node-l2/ovn-l2-nad.yaml) sample (`allowPersistentIPs`). The UDN-secondary path is documented as transparent; the UDN-primary/CUDN samples are marked as the separate-phase capability. | Done. **SHIPPED.** |
| **P5 — UDN-primary multi-tenancy *(separate later phase)*** | Guest-on-pod-PRIMARY-network UDN/CUDN multi-tenancy — a **different datapath** than the secondary-NAD backend (§4.4). Requires a robustly-resourced OVN-K cluster (the 3-VM `ntx` ovnkube-node crashlooped on UDN-network bring-up under the segmentation gate, R6). Its own design + spike + PRs when such a cluster exists. | Deferred; not gated by P1-P4. |

---

## 9. Risks

- **R1 — The live-overlap mechanism doesn't exist for non-KubeVirt on OVN-K.**
  ~~*Likelihood: medium-high.*~~ **RETIRED — the P0-c spike proved Mechanism B's
  simultaneous overlap works** (§3 / §8): same-claim-both-pods, allowed by default,
  no marker. The §6 gate remains as the general no-silent-failure backstop for any
  *future* offline-only backend, but the OVN-K live path is no longer at risk.
- **R2 — Node-local path regresses under OVN-K-primary (V1).** *Likelihood: low-
  medium.* OVN-K manages the pod's primary iface/routes/MTU differently than
  Calico. **Mitigation:** V1 is the first, cheapest test; failures here are fixable
  in `network-init.sh` (MTU, route ordering) and block backend work, not the whole
  effort.
- **R3 — Dual-substrate maintenance cost.** Two OVN backends, two identity forms,
  two migration mechanisms to keep working as both CNIs evolve. **Mitigation:** the
  interface caps the divergence to `Detect`+`Identity`; the datapath and control
  plane stay single-sourced. Two implementations is the explicit bar — reject a
  third *speculative* backend until a real third CNI shows up (Principle #1/#7).
- **R4 — OVN-K version/feature-gate drift.** `allowPersistentIPs` + `IPAMClaim`
  (the v1 bet) and UDN/CUDN have version/feature-gate floors (`IPAMClaim` /
  `allowPersistentIPs` are the NPWG-persistent-IP floor; UDN needs
  `--enable-network-segmentation` and a version floor). **Mitigation:** document
  floors; the §6 gate + a clear condition when a required feature is absent (no
  silent failure). (OKEP-5233 / Mechanism C is no longer on the path — §3/§10-b.)
- **R5 — Brittle coupling if §10-b had picked Mechanism A.** ~~*Real if A is
  chosen.*~~ **MOOT — §10-b is retired** (Mechanism B does the full job, no
  KubeVirt-label synthesis). Recorded so a future maintainer doesn't reintroduce
  Mechanism A without re-weighing this.
- **R6 — UDN-*primary* node-datapath instability on small/under-resourced OVN-K
  clusters.** *Likelihood: cluster-dependent.* The spike's `ntx` (3 libvirt VMs,
  k0s) **crashlooped ovnkube-node** bringing UDN-primary networks up under the
  `--enable-network-segmentation` gate (NB-DB COPP transaction timeout + a k0s
  kubelet-cgroup init quirk); recovery was deleting the UDN networks + rolling the
  gate OFF. **This does NOT touch the v1 backend** (UDN-secondary and the
  IPAMClaim path are unaffected — both proven on the same cluster). **Mitigation:**
  scope UDN-primary multi-tenancy as a **separate later phase** (P5) requiring a
  robustly-resourced OVN-K install; flag `--enable-network-segmentation` as a
  not-free cluster-config prerequisite (§4.4); do not assume UDN-primary in the
  core backend.

---

## 10. Decisions

> **Post-spike state:** ALL decisions are now **RESOLVED**. The affirmed decision
> and (a),(b),(e) were resolved by the P0 spike; **(c)** and **(d)** were signed off
> by the user on **2026-06-17 → "full first-class support"** (accept the
> dual-substrate maintenance cost; ship OVN-K-primary as a documented, validated,
> SUPPORTED option). The arc is GREEN to proceed to P1.

**(Affirmed) Implement the pluggable `ovnBackend` interface — YES.** This directly
answers the user's question ("will we implement the `ovnBackend` object so we can
support kube-ovn, OVN-Kubernetes and more?"): **yes, unambiguously.** The seam is a
single internal `ovnBackend` interface (§4) with **two implementations** today —
`kubeOVNBackend` (a behavior-preserving lift of the shipped #239/#240 kube-ovn
logic) and `ovnKubernetesBackend` (new). A **third** OVN CNI is then a new small
implementation (a `Detect` + an `Identity`), NOT a new branch through the
controller. The bar stays **two implementations, not a framework** (no registry /
loader / config — a slice of backends, first-match-wins on `Detect`; Principle #1).
A speculative third backend is rejected until a real third CNI shows up
(Principle #1/#7). The interface bounds the dual-substrate divergence to
`Detect`+`Identity`; the datapath and control plane stay single-sourced.

**a) Which OVN-K secondary type to target FIRST. — RESOLVED: `layer2` secondary.**
The spike confirmed `layer2` + `allowPersistentIPs` + `IPAMClaim` (Mechanism B) is
the right first target: closest analogue to the shipped kube-ovn path, cleanest
non-KubeVirt IP-keep, and it delivers **both offline AND live** IP-preservation
(spike §8). **UDN-*secondary*** rides the same backend transparently (generated
NAD, no extra code — §4.4) and folds in once the segmentation gate is on. `localnet`
and **UDN-*primary*** are NOT the first target — UDN-primary is a separate later
phase (P5 / R6).

**b) Fallback if OVN-K live-IP-keep were NOT non-KubeVirt-drivable. — RESOLVED:
RETIRED, no fallback needed.** The P0-c spike proved Mechanism B does the full job
(live overlap allowed by default, simpler than kube-ovn — §3 / §8). The three
fallback options (offline-only / Mechanism-A KubeVirt-label synthesis / Mechanism
C) are dropped. The §6 live-eligibility gate is **kept** as the general
no-silent-failure backstop (it would refuse `mode: live` on any *future* backend
that reports no live mechanism, or a misconfigured OVN-K NAD), but neither shipped
backend triggers it.

**c) Dual-substrate maintenance commitment. — RESOLVED (user 2026-06-17: ACCEPT).**
Shipping `ovnKubernetesBackend` means committing to maintain **both** kube-ovn AND
OVN-K backends going forward (two identity forms, two migration mechanisms, kept
working as both CNIs evolve).
→ **Decision: ACCEPT the dual-substrate cost** (user-signed-off 2026-06-17). The `ovnBackend` interface
caps the divergence to `Detect`+`Identity` (~a detector + an `IPAMClaim` lifecycle
+ an identity-annotation form per backend); the entire datapath (`network-init.sh`
re-MAC) and control plane (resolver, migration gate, dst-annotation machinery) stay
**single-sourced** — ~70% of the work is shared verbatim (§2). The marginal cost of
the second backend is genuinely bounded, and OVN-K-primary is a real operator
segment (OpenShift / upstream OVN-K / telco). *If the user prefers to NOT carry the
cost, the alternative is (d)-advanced-only: ship+document OVN-K-primary as a
validated path but not a first-class-maintained option — a weaker but cheaper
posture.*

**d) Is OVN-K-primary a *supported operator option* or a *validation/advanced
path*? — RESOLVED (user 2026-06-17: SUPPORTED option).**
→ **Decision: ships as a documented, validated, SUPPORTED option (install
guide + samples + runbook), with kube-ovn-non-primary remaining the low-friction
DEFAULT/recommended path** (user-signed-off 2026-06-17). OVN-K-primary is "for operators already standardized on
OVN-K, or who want UDN-secondary networks." This warrants the full P4 doc/runbook
investment (sibling to `ovn-l2-install.md`). *The alternative — advanced/validate-
first only (lighter docs, "use at your own risk") — pairs with declining (c); pick
(c)+(d) together.* **Tiering note (the supported-vs-advanced line within OVN-K):**
recommend **first-class-supported** = the spike-proven set [layer2-secondary
reachability, offline + live IP-preserving migration, UDN-secondary]; **documented-
advanced/validate-first** = [localnet/provider VLAN, OVN-K-native NetworkPolicy on
the VM] (not separately spiked); **separate-later-phase, explicitly not-yet-
supported** = [UDN-primary multi-tenancy] (R6 — needs a robust OVN-K cluster).

**e) Cluster scope / blast radius. — RESOLVED (a dedicated OVN-K cluster already
exists; the dev cluster is NOT rebuilt).** The spike ran on a **dedicated
OVN-K-primary cluster `ntx`** (3 libvirt VMs, k0s 1.34.3, `ovn-kube-ubuntu:
release-1.2`, Multus, nested virt), so the original "replace Calico with OVN-K on
the dev frida/miles/boba k0s" rebuild is **not required** — the dev cluster keeps
its current CNI, and P3 (real-VM validation) runs on `ntx`. Confirmed install
surface on `ntx`: `allowPersistentIPs` (core) + the NPWG `IPAMClaim` CRD are
present and proven; `--enable-network-segmentation` is available but carries node-
stability risk on this small cluster (R6) and is only needed for UDN. V1 (smoke
regression on `ntx`) remains the go/no-go-for-backend-work viability gate in P3.

---

## 11. Cross-references

- **P0 spike findings (gitignored):** [`ovn-cni-backends-spike.md`](ovn-cni-backends-spike.md)
  — P0-c PASS, UDN secondary/primary split, the residual identity unknown.
- Shipped kube-ovn integration: [`kubeovn.go`](../../internal/controller/swiftguest/kubeovn.go),
  [`dst_pod.go`](../../internal/controller/swiftmigration/dst_pod.go),
  [`network-init.sh`](../../rust/swiftletd/scripts/network-init.sh).
- Multi-node L2 arc + validation matrix: [`../networking/multi-node-l2.md`](../networking/multi-node-l2.md),
  [`../networking/ovn-l2-install.md`](../networking/ovn-l2-install.md).
- Network framework: [`network-architecture-requirements.md`](network-architecture-requirements.md) (§6).
- OVN-K integration guide (secondary networks today): [`../networking/ovn-kubernetes.md`](../networking/ovn-kubernetes.md).
- Staged OVN-K samples: [`../../config/samples/multi-nic/nad-ovn-layer2.yaml`](../../config/samples/multi-nic/nad-ovn-layer2.yaml),
  [`nad-ovn-localnet.yaml`](../../config/samples/multi-nic/nad-ovn-localnet.yaml),
  [`udn-tenant-isolation.yaml`](../../config/samples/multi-nic/udn-tenant-isolation.yaml),
  [`cudn-shared-network.yaml`](../../config/samples/multi-nic/cudn-shared-network.yaml).

### Research sources (OVN-K live-migration / persistent-IP mechanism, 2025–2026)

- OVN-K live migration feature (KubeVirt label/annotation recognition, primary-only):
  <https://github.com/ovn-kubernetes/ovn-kubernetes/blob/master/docs/features/live-migration.md>,
  <https://ovn-kubernetes.io/features/live-migration/>
- OVN-K multi-homing / persistent IPs / `IPAMClaim` / `ipam-claim-reference` (NPWG v1.3):
  <https://ovn-kubernetes.io/features/multiple-networks/multi-homing/>
- NPWG `IPAMClaim` CRD + KubeVirt `ipam-extensions` (the non-KubeVirt-generic claim path):
  <https://github.com/kubevirt/ipam-extensions>,
  <https://github.com/k8snetworkplumbingwg/network-attachment-definition-client>
- OKEP-5233 Preconfigured UDN Addresses (static IP/MAC/GW on primary L2 UDN):
  <https://ovn-kubernetes.io/okeps/okep-5233-preconfigured-udn-addresses/>
- kube-ovn live migration (the shipped substrate, for contrast):
  <https://kubeovn.github.io/docs/v1.13.x/en/kubevirt/live-migration/>
