# KubeSwift Service Exposure Architecture

> Status: **DESIGN — approved, S0 spike validated (2026-06-13).** How network
> services are exposed **to** and **from** SwiftGuest VMs. Companion to
> [`network-architecture-requirements.md`](network-architecture-requirements.md)
> (the node-local-vs-multi-node-L2 framework this builds on) and
> [`../networking/multi-node-l2.md`](../networking/multi-node-l2.md).
>
> The five design decisions in §12 are resolved by the project lead; the
> phasing in §9 is approved; the load-bearing primitive (§1.2) is
> cluster-validated end-to-end in §11.

## 1. Goal & the core problem

A SwiftGuest is **one pod**, and the VM lives *behind* the launcher pod's
network namespace: a `br0` bridge (gateway + in-pod dnsmasq) with `tap0`, and
the VM gets a private DHCP IP (e.g. `192.168.99.x`). `eth0` (the pod's CNI
interface / pod-IP) is deliberately **not** enslaved to `br0` — enslaving it
broke pod networking ("Bug 1"). So the VM is hidden from the pod network and
from outside.

```
            cluster / outside world
                     │
            ┌──────── pod eth0 (CNI, pod-IP, routable) ────────┐
            │   launcher pod netns                              │
            │      br0 (192.168.99.1, gw + dnsmasq)             │
            │        └─ tap0 ── VM (192.168.99.x)  ← hidden     │
            └───────────────────────────────────────────────────┘
```

**The goal:** expose any TCP/UDP/SCTP service running in the VM to the cluster
and beyond, and let the VM reach cluster services — across any CNI, composing
with the cloud-native ecosystem rather than reinventing it.

### 1.1 Grounding — what exists today (verified in code)

| Fact | Source |
|---|---|
| The VM lives behind the launcher pod netns: `br0` (`192.168.99.1/24` + dnsmasq) ↔ `tap0`; `eth0` is **not** enslaved to `br0`. | `rust/swiftletd/scripts/network-init.sh` |
| **Egress SNAT already exists**: `POSTROUTING -s 192.168.99.0/24 ! -d 192.168.99.0/24 -j MASQUERADE` + `ip_forward=1`. So **VM→internet works today**. | `rust/swiftletd/scripts/network-init.sh` |
| The launcher hands the VM the pod's `resolv.conf` nameserver (cluster DNS) as DHCP DNS — but no rule guarantees the VM can *reach* a ClusterIP. | `rust/swiftletd/scripts/launcher-entrypoint.sh` |
| Launcher + init containers run **privileged** (intended policy) — we can program iptables/nft in the pod netns. | `internal/controller/swiftguest/security.go` |
| **No Service / EndpointSlice / `discovery.k8s.io` code anywhere** — greenfield. | repo-wide grep |
| swiftletd reports status via pod annotations; the controller maps them to `status.network`. | `internal/controller/swiftguest/constants.go` |
| The pool's current readiness signal is `GuestRunning=True` — VM **booted**, NOT in-guest service up. | `internal/controller/swiftguestpool/controller.go` |
| The live-migration mTLS path already injects a per-guest sidecar (env/file-parameterized, role flipped post-DeepCopy) — the pattern a remote-access proxy reuses. | `internal/controller/swiftguest/migration_sidecar.go` |
| Live migration renames the pod `<guest>-mig-<uid>` and may change the IP. Any Service must survive this. | context doc, W26 |

Prior art: KubeVirt's **masquerade** binding is iptables NAT (pod-`eth0` ↔ `tap0`),
its `ports` attribute is evaluated only by masquerade/passt, and `virtctl
expose` mints a **plain selector Service** targeting the virt-launcher pod's
labels. KubeSwift adopts the same shape.

## 2. Architecture & binding model

### 2.1 The canonical model — "pod-network binding"

The foundational primitive is a **binding mode** on the guest's primary
interface that sets its relationship to the pod's network identity. KubeSwift
ships **two** modes; the default changes nothing about today's behavior.

| Binding | What it is | VM IP | Service-exposable | Migration-friendly | Default |
|---|---|---|---|---|---|
| **`nat`** (rename of today) | `br0`+`tap0`, in-pod dnsmasq, MASQUERADE out `eth0`; VM hidden behind the pod IP | private `192.168.99.x` | **Yes** — port lands on the pod IP via in-pod DNAT | **Yes** — IP is pod-managed | **default** |
| **`bridge`** (primary-on-NAD) | primary NIC rides a multi-node-L2 NAD; guest gets the NAD's portable IP | NAD IPAM (portable) | via the NAD's own L2 reachability (not in-pod DNAT) | IP-preserving (Tier C) | opt-in |

We deliberately do **not** add KubeVirt's other bindings as named modes:
SR-IOV is already `interfaces[].type: sriov` (the throughput substrate), and
there is no `passt`/`slirp` userspace stack in scope (CH uses tap; minimalism).
The taxonomy collapses to three orthogonal substrates that already have homes:
**`nat` = Service-exposure**, **`bridge`/NAD = portable-IP**, **`sriov` =
throughput**.

> **Why `nat` is the exposure substrate, not `bridge` — the load-bearing
> decision.** With `nat`, the VM's service port is reachable **at the pod IP**
> after an in-pod DNAT, so the VM becomes a **normal Kubernetes Endpoint** and
> the *entire* Service ecosystem (ClusterIP, NodePort, LoadBalancer via
> MetalLB/Cilium, Gateway/Ingress, NetworkPolicy, mesh) composes for free. With
> `bridge`/NAD the port is at the NAD IP — not the pod IP, not selectable by a
> normal Service — so exposure there means "use the NAD's L2", a CNI-specific
> story.

### 2.2 Where the bridge happens, and the mechanism

**A single in-pod DNAT, programmed in the launcher pod's netns,
CNI-agnostically.** When the guest declares port `P` (guest port `Q`):

```
# INGRESS (north → VM): traffic to podIP:P → VM
iptables -t nat -A PREROUTING -p tcp --dport P -j DNAT --to-destination <vmIP>:Q
# the existing POSTROUTING MASQUERADE handles the reply path
```

This is plain netfilter in the pod's **own** namespace, so it works on **any**
CNI — the lowest common denominator the design mandates. It is the **only new
datapath primitive**; everything else is standard Kubernetes objects the
controllers create. (Validated end-to-end, §11.)

The port list reaches the pod netns via the **RuntimeIntent** (a new
`ports[]` on the NIC intent); **`network-init.sh` installs the DNAT** in the
same privileged init container that already owns iptables. swiftletd's Rust
launch path is untouched. The Go controllers own Service/EndpointSlice creation.

## 3. CRD surface

**Decision: extend `SwiftGuest.spec.network` and add a `SwiftGuestPool.spec.service`
surface; do NOT introduce a standalone `SwiftService` CRD.** Ports are an
intrinsic property of one VM; a separate CRD would re-implement Service
lifecycle and create an orphan-GC hazard. The pool *is* the fleet-scoped
surface, so a field on it is the natural home and lets the pool controller own
EndpointSlice churn atomically with scale. A standalone `SwiftService{selector,
ports}` becomes justified only when a Service must span *heterogeneous* guests
(cross-pool) — deferred (YAGNI); the same in-pod-DNAT substrate makes it cheap
to add later.

### 3.1 SwiftGuest — declarative ports + binding (additive; nil = today)

```go
// SwiftGuestSpec (new)
//   Network configures pod-network binding and declarative service ports.
//   nil preserves current behavior (nat binding, no Service).
// +optional
Network *GuestNetworkSpec `json:"network,omitempty"`

type GuestNetworkSpec struct {
    // Binding: nat (default; ports Service-exposable via in-pod DNAT) or
    // bridge (primary on a multi-node-L2 NAD; portable IP; ports NOT DNAT'd).
    // +kubebuilder:validation:Enum=nat;bridge
    // +kubebuilder:default=nat
    // +optional
    Binding string `json:"binding,omitempty"`

    // Ports declares guest service ports. Each installs an in-pod DNAT
    // podIP:port -> vmIP:targetPort. Set Expose to mint a Service.
    // +optional
    Ports []GuestPort `json:"ports,omitempty"`
}

type GuestPort struct {
    // Name — DNS-label; REQUIRED when len(ports) > 1.
    // +optional
    Name string `json:"name,omitempty"`
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=65535
    Port int32 `json:"port"`
    // TargetPort — in-guest listening port. Defaults to Port.
    // +optional
    TargetPort int32 `json:"targetPort,omitempty"`
    // +kubebuilder:validation:Enum=TCP;UDP;SCTP
    // +kubebuilder:default=TCP
    // +optional
    Protocol corev1.Protocol `json:"protocol,omitempty"`
    // Expose, when set, mints ONE per-guest Service of this type targeting the
    // launcher pod (all exposed ports share the one Service — DECISION §12.2).
    // Omitted = DNAT only (reachable pod→VM, no Service object).
    // +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
    // +optional
    Expose string `json:"expose,omitempty"`
}
```

Status (no silent failures):

```go
type GuestNetworkStatus struct {        // EXTENDS the existing struct
    // ...existing PrimaryIP / Interfaces...
    // Egress reports verified VM→cluster-service reachability (§4):
    //   "" not yet probed | "ClusterServices" reachable | "DirectOnly"
    //   internet/SNAT works but ClusterIP DNAT not in this netns (eBPF CNI).
    // +optional
    Egress string `json:"egress,omitempty"`
    // ExposedPorts echoes the installed DNAT rules; ServiceRef names the Service.
    // +optional
    ExposedPorts []ExposedPortStatus `json:"exposedPorts,omitempty"`
    // +optional
    ServiceRef *corev1.LocalObjectReference `json:"serviceRef,omitempty"`
}
```

Conditions: `PortsProgrammed` (DNAT installed — reported by swiftletd via a new
`kubeswift.io/ports-programmed` annotation), `ServiceReady` (Service +
EndpointSlice reference a Ready endpoint), `EgressReady` (the §4 probe passed).
Printer columns (`priority=1`): `EXPOSE` (`.status.network.serviceRef.name`),
`EGRESS` (`.status.network.egress`).

Webhook (per-operation discipline, Principle #10): `ValidateCreate` rejects
`expose != "" && binding == "bridge"` (a NAD-bound VM is not pod-IP-selectable);
rejects `ports` on an `sriov` primary (no tap to DNAT to); rejects duplicate
`port`s; requires `name` when `len(ports) > 1`. **`ports` WITHOUT `expose` is
allowed on `bridge` guests** (DNAT-skipped; useful for NetworkPolicy port
targeting — DECISION §12.4). `ValidateUpdate` is shape-only.

### 3.2 SwiftGuestPool — the balanced-service surface (first-class)

```go
// SwiftGuestPoolSpec (new)
//   Service exposes ONE load-balanced Service across all pool replicas.
//   Endpoints are the replicas' pod IPs (post-DNAT), added/removed as the pool
//   scales and during rolling updates. The HPA/metrics seam (§6.3).
// +optional
Service *PoolServiceSpec `json:"service,omitempty"`

type PoolServiceSpec struct {
    Ports []GuestPort `json:"ports"`        // per-port Expose ignored; type is set once below
    // +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
    // +kubebuilder:default=ClusterIP
    // +optional
    Type string `json:"type,omitempty"`
    // Headless, when true, sets clusterIP: None for per-replica DNS
    // (StatefulSet-style — useful for sharded AI/ML inference). DECISION §12.3.
    // +optional
    Headless bool `json:"headless,omitempty"`
    // ReadinessGate: GuestRunning (default; VM booted + has IP) or ServiceReady
    // (an in-guest TCP/HTTP probe passes — don't route until the app is up).
    // +kubebuilder:validation:Enum=GuestRunning;ServiceReady
    // +kubebuilder:default=GuestRunning
    // +optional
    ReadinessGate string `json:"readinessGate,omitempty"`
    // +optional
    ReadinessProbe *PoolReadinessProbe `json:"readinessProbe,omitempty"`
}
```

The pool propagates `Service.Ports` into each replica's `spec.network.ports`
(each replica installs its DNAT) and the **pool controller** owns the single
Service + its EndpointSlices (§6).

## 4. Egress to cluster services

**VM→internet works today** (the MASQUERADE rule). **VM→ClusterIP + cluster DNS
is the gap, and it is CNI-dependent:**

- **kube-proxy (iptables/IPVS)** clusters: the ClusterIP DNAT lives in the host
  netfilter `nat` table that VM traffic transits after the pod's
  forward+SNAT — so **it works today** (the dev cluster is k0s + Calico +
  kube-proxy; §11 confirms the pod-netns path reaches ClusterIPs).
- **eBPF kube-proxy-replacement (Cilium / Calico-eBPF)**: ClusterIP resolution
  is an eBPF program on the pod's `eth0` hook; traffic originating on `tap0`/
  `br0` and forwarded out `eth0` does **not** traverse that hook, so the VM
  cannot resolve ClusterIPs. Documented upstream:
  [KubeVirt #10388](https://github.com/kubevirt/kubevirt/issues/10388),
  [Cilium #37669](https://github.com/cilium/cilium/issues/37669).

### 4.1 The CNI-agnostic mechanism (two tracks, auto-selected, status-surfaced)

- **DNS (always):** dnsmasq already forwards to cluster DNS, so the guest
  resolves `svc.ns.svc.cluster.local` today. Add `domain-search` +
  `option:15` so short names resolve.
- **Track 2a — kube-proxy clusters: nothing extra.** The in-netns/host ClusterIP
  DNAT already serves the VM. The controller **verifies and reports** it (§4.2).
  **This is the validated baseline.**
- **Track 2b — eBPF clusters: an explicit opt-in redirect** behind
  `spec.network.egressMode: clusterServices`, resolved at a spike on a real
  eBPF cluster (in-pod redirect vs a userspace forwarder). **We have no Cilium
  cluster today → this is a ROADMAP item** (same infra-gated posture as SR-IOV
  / Tier-C). The Service CIDR is **auto-discovered** (read a sentinel Service's
  ClusterIP) with an operator override `egress.serviceCIDR` (DECISION §12.1).

### 4.2 No silent failures — the egress probe (the marquee §4 deliverable)

swiftletd attempts a connection from the pod context to cluster DNS
(`10.96.0.10:53`) at startup and writes `kubeswift.io/egress-cluster-reachable:
true|false`; the controller maps it to `status.network.egress` and an
`EgressReady` condition. On an eBPF cluster without Track 2b this surfaces
`EgressReady=False, reason=ClusterIPUnreachableInPodNetns` pointing at
`egressMode: clusterServices` — turning a notorious silent failure into a
`kubectl`-visible, documented state.

## 5. L4 ingress Service (declare ports → Service)

For a `nat`-bound guest with `ports[].expose`:

1. **In-pod DNAT** `podIP:port → vmIP:targetPort` (§2.2) makes `podIP:port` a
   live endpoint.
2. The controller mints **one Service per guest** carrying all exposed ports.
3. **Endpoints — single guest = plain selector Service.** A Service with
   `selector: {swift.kubeswift.io/guest: <name>}` and numeric `targetPort` is
   populated by the endpoint controller with the launcher pod IP; kube-proxy
   load-balances `serviceIP → podIP:port → (in-pod DNAT) → vmIP`. **No
   EndpointSlice management needed** (KubeVirt's `virtctl expose` model).
   For honest readiness, set a **launcher-container readiness probe** that
   TCP-connects `podIP:port` (succeeds only once the in-guest app is up,
   because the DNAT forwards to the VM) — so pod-Readiness == VM-service-
   Readiness, and the selector Service's endpoint readiness is correct with
   zero endpoint code.
4. **Pool case = managed EndpointSlices** (§6) — needed for one Service / N
   pods / custom readiness / scale churn.

TCP/UDP/SCTP all flow (the `Protocol` enum). `expose: ClusterIP|NodePort|
LoadBalancer` is the Service type. Because the guest is now a normal endpoint,
it composes upward (§7): a `LoadBalancer` Service gets its IP from MetalLB /
Cilium LB-IPAM; a ClusterIP Service is a normal `backendRef` for a Gateway API
`HTTPRoute`/`TCPRoute`.

## 6. Pool-balanced service

One Service, endpoints = the pool's VMs, updated as the pool scales and during
rolling updates. The **pool controller** owns it.

### 6.1 EndpointSlice lifecycle
- The pool controller creates **one Service** (`<pool>-svc`) with **no selector**
  and **manages `EndpointSlice` objects directly** (`discovery.k8s.io/v1`,
  labeled `kubernetes.io/service-name` + a `managed-by` label).
- Each reconcile computes the desired endpoint set from replicas that pass the
  **readiness gate**, recomputed on scale-up/down and rolling-update (a replica
  being replaced is removed before its pod is deleted, added when its successor
  is ready) — analogous to the existing `readyCount`/`unavailable` accounting.

### 6.2 Readiness gating — booted vs app-up
`readinessGate: GuestRunning` (default) uses today's signal. `ServiceReady`
runs an **in-guest readiness probe** (swiftletd reports via a new
`kubeswift.io/service-ready` annotation); only `service-ready` replicas enter
the EndpointSlice — the production-correct mode.

### 6.3 The HPA / metrics seam (anticipated, not built)
The pool already exposes a **scale subresource**
(`+kubebuilder:subresource:scale`). That is the HPA contract. Because Service
membership is derived from replicas + readiness, **a future HPA scaling
`spec.replicas` needs zero service-layer change** — endpoints follow. The only
remaining work for metrics-driven autoscaling is a custom-metrics adapter
(out of scope; the observability program's CR-state gauges are the foundation).

## 7. Build vs leverage

| Layer | Decision | Specifics |
|---|---|---|
| In-pod VM↔pod DNAT | **BUILD** (only new datapath) | ~15 lines in `network-init.sh`; the irreducible primitive |
| Per-guest / per-pool Service + EndpointSlice | **BUILD** (thin) | stock `corev1.Service` / `discovery.k8s.io` objects |
| Egress SNAT (internet) | **HAVE** | already shipped |
| Egress to ClusterIP (eBPF) | **BUILD-behind-opt-in + ROADMAP spike** | Track 2b; needs a Cilium cluster |
| ClusterIP / NodePort routing | **LEVERAGE** | kube-proxy / CNI |
| Bare-metal LoadBalancer | **LEVERAGE** | MetalLB or Cilium LB-IPAM; KubeSwift writes only `type: LoadBalancer` |
| Ingress / L7 / Gateway | **LEVERAGE** | Gateway API (GA v1.5) — Envoy Gateway, Istio, Cilium, NGINX Gateway Fabric; the guest Service is a `backendRef` |
| Remote access (off-cluster operator; the RDP case) | **LEVERAGE + thin glue** | Tailscale K8s operator (tailnet ingress proxy), Cloudflare Tunnel, frp; optional Service annotation |
| Service mesh / VM-in-mesh (Telco, zero-trust E-W) | **LEVERAGE** | Istio ambient enrolls the launcher pod (no in-VM sidecar) via namespace label; `WorkloadEntry`/`WorkloadGroup` for explicit registration |
| Multi-node L2 / portable IP | **LEVERAGE (Multus NAD)** | already the `networkRef` abstraction; OVN-K layer2 / macvlan |

**Posture:** KubeSwift builds *exactly one* datapath primitive and the
Service/EndpointSlice controllers, and rides the entire CNCF north-south + mesh
ecosystem above L4 — maximal CNI-agnostic + feature-rich with minimal surface.

## 8. Use-case → capability mapping

| Use case | Demands | KubeSwift must |
|---|---|---|
| In-cluster service-to-service | ClusterIP ingress + egress-to-ClusterIP | in-pod DNAT + verified ClusterIP egress (the foundational pair) |
| North-south (web/APIs) | LoadBalancer/Ingress/Gateway | mint a Service; leverage MetalLB/Cilium + Gateway API |
| Remote/external (RDP demo, operator off-net) | reach a guest port from outside | mint a Service + leverage Tailscale/Cloudflare (productize the socat+port-forward hack) |
| **Edge computing** | works on **any** CNI (often kube-proxy/k3s/Flannel); minimal deps | the CNI-agnostic in-pod DNAT is exactly right; `nat` + NodePort/LB; no eBPF assumption |
| **AI/ML inference** | load-balanced, autoscaling endpoint across replica VMs; GPU guests | **pool-balanced Service** + `readinessGate: ServiceReady` (don't route until the model loads) + the HPA seam; headless option for sharded serving; composes with GPU pools |
| **AI/ML training** | E-W collective comms (NCCL) between GPU VMs — often **not** a Service but direct VM↔VM | a *connectivity* need: **multi-node L2 (Tier C)** + optionally SR-IOV/RDMA; exposure layer is secondary |
| **Telco / NFV** | stable MACs, L2 adjacency, multicast/non-IP, **not** NAT; SCTP; portable IP | **`bridge`/NAD** + `sriov`; SCTP in `GuestPort`; explicitly NOT the `nat` path |
| Stateful svc w/ external clients | IP survives maintenance moves | **`bridge`/NAD portable IP (Tier C)**; Service stable across pod rename (§10) |

**Sequencing insight:** the `nat`-exposure path serves *most* cases (in-cluster,
north-south, remote, edge, inference). Telco/NFV and training lean on
connectivity substrates (Tier C, SR-IOV) that already exist or are designed —
they are not blocked on this work. The design's center of gravity is correctly
the `nat`+Service+pool path.

## 9. Layer placement

| Mechanism | Go controller | RuntimeIntent | swiftletd (Rust) | init container (shell) |
|---|---|---|---|---|
| Port declaration → DNAT | resolve `spec.network.ports` → intent | **+`Ports []PortIntent`** | unchanged | **`network-init.sh` installs DNAT** |
| containerPort on launcher | **pod.go adds it** | — | — | — |
| Per-guest selector Service | **swiftguest controller creates+GCs** | — | — | — |
| Pool Service + EndpointSlices | **pool controller manages** | — | — | — |
| Egress ClusterIP verify | maps annotation → `status.network.egress` | — | **probe + annotation** | (Track-2b redirect, eBPF) |
| In-guest readiness (`ServiceReady`) | gates EndpointSlice membership | — | **probe + annotation** | — |

Netfilter stays in the shell init container; Service/endpoint objects stay in
Go; status probes stay in swiftletd reporting via annotations (the established
pattern). swiftletd's launch path is untouched.

## 10. Phasing & dependency analysis

### 10.1 Egress-vs-L4 sequencing — the answer
**Ingress L4 is the spine; build it first. Egress-to-ClusterIP runs in parallel
but its hard part is infra-gated.** Reasoning: egress-to-internet already works;
the only egress gap is ClusterIP-on-eBPF, which is CNI-conditional (a no-op on
the kube-proxy dev cluster) and can't be validated without a Cilium cluster.
L4 ingress is buildable and validatable today, and it introduces the in-pod
DNAT primitive that the pool service and remote-access paths also build on —
highest validated value-per-risk. The two are not strongly coupled (independent
rules in the same file). Both ship at equal priority; the *sequence* is
ingress-first for dependency reasons.

### 10.2 Phases

- **S0 — Spike (DONE, §11).** In-pod DNAT + ClusterIP Service + cross-pod
  reachability + Bug-1 + migration-survival, validated on the dev cluster.
- **S1 — Binding rename + in-pod DNAT + per-guest L4 Service (the spine).**
  `spec.network{binding, ports}`; `network-init.sh` DNAT; launcher containerPort
  + readiness probe; selector Service mint+GC; status/conditions/printcolumns;
  webhook. Unblocks remote access, pool service, Gateway/LB. *Parallel with S2.*
- **S2 — Egress observability + (opt-in) ClusterIP egress.** The §4.2 probe +
  `status.network.egress` + `EgressReady` (works on every CNI; converts the
  silent failure to visible state) + DNS search-domain fix. Track-2b eBPF
  redirect → **ROADMAP** (needs a Cilium cluster). *Parallel with S1.*
- **S3 — Pool-balanced Service.** `SwiftGuestPool.spec.service`; managed
  EndpointSlices; readiness gating; headless option. **Depends on S1.** Carries
  the HPA seam (no HPA code).
- **S4 — Ecosystem glue + docs (mostly leverage).** Validated recipes:
  Service→MetalLB; Service→Gateway API; Service→Tailscale (productized RDP);
  Istio-ambient enrollment. Minimal code; mostly `docs/networking/` + samples.
  **Depends on S1/S3.**

## 11. S0 spike results — CLUSTER-VALIDATED (2026-06-13)

Validated the one load-bearing primitive on the dev cluster (k0s 1.34 +
Calico + kube-proxy), against the live `lm-ubuntu` guest — whose launcher pod
was `lm-ubuntu-mig-c927d5` (it had been **live-migrated**: renamed `-mig-`,
`migration-role: destination`, still labeled `swift.kubeswift.io/guest=lm-ubuntu`).

| Check | Method | Result |
|---|---|---|
| In-pod DNAT | `iptables -t nat -A PREROUTING --dport 2222 -j DNAT --to 192.168.99.15:22` in the privileged launcher | installed |
| Selector Service tracks the (migrated) pod | `Service{selector: guest=lm-ubuntu, port 2222→2222}` | EndpointSlice populated `10.244.213.39:2222` **ready=true** — auto-matched the `-mig-` pod via the guest label (**migration-survival validated live**) |
| **Ingress end-to-end, cross-node** | a busybox pod on **miles** → ClusterIP `10.100.106.111:2222` | returned the VM's real banner **`SSH-2.0-OpenSSH_9.6p1 Ubuntu-3ubuntu13.16`** (pod→ClusterIP→kube-proxy→migrated-pod→in-pod DNAT→VM:22) |
| **Bug-1 cleared** | launcher → apiserver ClusterIP `10.96.0.1:443` after DNAT | **REACHABLE**; guest stayed `Running` / `GuestRunning=True` |
| Egress mechanism (Track 2a) | cluster is kube-proxy mode + pod-netns reaches ClusterIP + VM internet-egress proven (MASQUERADE) | **confirmed** by inference; true VM-origin measurement folds into S2's egress probe |

Artifacts removed after the spike (DNAT deleted, Service deleted; `lm-ubuntu`
untouched). **Conclusion:** the in-pod-DNAT + selector-Service keystone works
end-to-end, cross-node, survives live migration, and does not regress pod
networking. The design rests on validated mechanics.

## 12. Decisions (resolved by the project lead, 2026-06-13)

1. **Egress Service-CIDR discovery:** auto-discover, with an operator override.
2. **One Service per guest carrying multiple ports** (matches `virtctl expose`).
3. **Headless pool Service option** (`clusterIP: None`) — added (sharded AI/ML).
4. **`ports` without `expose` on a `bridge`/NAD guest** — allowed (DNAT-skipped;
   for NetworkPolicy port targeting).
5. **Track-2b eBPF egress mechanism** — resolved at a spike; **a Cilium-based
   cluster is a roadmap item** (infra-gated).

## 13. Risks & principle alignment

- **"Bug 1" (pod-networking breakage):** the design never enslaves `eth0`; it
  adds scoped `nat`-table DNAT only. **Cleared by S0** (apiserver reachable
  post-DNAT). LOW.
- **Live migration:** the per-guest selector (`swift.kubeswift.io/guest`) is
  carried onto the dst pod by `newDstPod`'s DeepCopy, so the Service auto-
  re-targets the renamed pod; the ClusterIP is stable, the endpoint IP blips
  during cutover (matching downtime). IP-preserving migration uses
  `binding: bridge`/NAD. **Validated live by S0** (the spike's pod *was* a
  migrated pod). The brief src/dst dual-match transient is handled by endpoint
  readiness gating (mirror the PR #57 stable-sort discipline if a managed-
  EndpointSlice variant needs deterministic membership).
- **Security / trust boundary:** exposure is operator-declared and opt-in
  (default = no exposure). NetworkPolicy composes for free (the guest is a
  normal pod endpoint selectable by `swift.kubeswift.io/guest`); the in-pod
  DNAT forwards only declared ports (least-exposure vs a whole-pod-IP bridge).
  Remote-access trust moves to the tunnel operator's authn (tailnet ACLs / CF
  Access), the same posture as the migration mTLS trust-anchor model.
- **Principle alignment:** minimalism (one datapath primitive; no new CRD; no
  new container) · CH-first (orthogonal to the hypervisor; launch path
  untouched) · k8s-native (the VM becomes a first-class Endpoint) · no silent
  failures (the §4.2 egress probe + `EgressReady`/`PortsProgrammed`/
  `ServiceReady` conditions) · distributed-by-design (pool Service spans nodes;
  Tier-C portable IP) · CNI-agnostic (plain netfilter baseline; CNI/ecosystem
  features compose where present, never required) · explicit discipline
  (per-operation webhook; opt-in exposure; explicit enums).

## Sources

- [KubeVirt network binding plugins](https://kubevirt.io/user-guide/network/network_binding_plugins/) · [`virtctl expose`](https://github.com/kubevirt/kubevirt/blob/main/pkg/virtctl/expose/expose.go)
- [KubeVirt #10388 — VM cannot reach Services without kube-proxy](https://github.com/kubevirt/kubevirt/issues/10388) · [Cilium #37669 — KubeVirt VM cannot reach service IP (eBPF)](https://github.com/cilium/cilium/issues/37669)
- [Gateway API v1.5 (Stable, 2026)](https://kubernetes.io/blog/2026/04/21/gateway-api-v1-5/)
- [Cilium LB-IPAM vs MetalLB](https://docs.rafay.co/blog/2025/06/17/using-cilium-as-a-kubernetes-load-balancer-a-powerful-alternative-to-metallb/)
- [Tailscale Kubernetes operator](https://tailscale.com/docs/kubernetes-operator/concepts/architecture) · [Istio ambient — add VM/external workloads](https://istio.io/latest/docs/ambient/usage/add-workloads/)
