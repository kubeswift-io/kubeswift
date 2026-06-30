# Design: UI Backend Enablement — gateway, contract, and data planes

> Status: **P0 SHIPPED + cluster-validated (2026-06-22).** Decisions resolved (§4); the P0 cut (§6)
> built across PRs #259/#260/#261/#263/#264 and proven end-to-end on two real clusters (see "P0 —
> shipped + validated" below). Decision/build record for the KubeSwift **backend** that unlocks a
> separate `kubeswift-ui` (Angular static app); the Angular app itself is out of scope (separate
> repo). Remaining backend work is the **P1+ planes** (§5).

## 1. Context

A web UI for KubeSwift is planned — Angular + Material Design 3, RxJS, a **static-asset
build** (no SSR / no Node runtime), GCP-console aesthetic; heavy VM inventories (virtual
scrolling over hundreds of rows), live telemetry (CPU/RAM/Disk-IO/migration state), and
embedded consoles (xterm.js serial + noVNC graphical).

A browser **cannot** speak Connect/gRPC-Web to the Kubernetes API, nor reach a launcher
pod's serial socket or a VNC port directly. So the UI needs a backend **gateway** (a
Connect-Go server) plus a **proto contract**. Both are greenfield: the repo has no API
server, no proto, no `web/` today — `swiftctl` + client-go is the current "UI", with data
moving over the k8s API + pod annotations + unix sockets + CH-HTTP/QMP.

**The gateway is a multi-cluster hub (D2).** It runs in a hub cluster, discovers member
clusters from a registry CRD (D2a), holds an impersonation-capable credential per member,
and fans out / merges across the fleet. The UI talks to one endpoint and sees the whole
fleet, with a `cluster` dimension on every resource.

**Repo split (decided):** the Angular UI lives in a **separate repo** (`kubeswift-ui`); the
**gateway + proto + fleet-registry CRD live here** (the gateway imports `api/swift/v1alpha1`
+ client-go and is tightly coupled to the operator's data model). The proto is the versioned
seam, so both sides develop in parallel against a frozen contract.

## 2. Architecture

```
Browser  (Angular static — separate repo)
   │  Connect / gRPC-Web (data)   +   raw WebSocket (consoles)
   ▼
kubeswift-gateway          (NEW — this repo; runs in a HUB cluster)
   ├─ Fleet registry   watch fleet.kubeswift.io Cluster + per-member Secret
   ├─ Per-cluster pool client-go + informer cache, one per member
   ├─ Read plane       list/get/watch → server-stream (fan-out + merge)
   ├─ Write plane      CRUD + lifecycle/snapshot/migrate (shared w/ swiftctl)
   ├─ Telemetry plane  per-cluster Prometheus (cAdvisor join on guest label)
   ├─ Console plane    WS ↔ target cluster's launcher serial port
   └─ Auth             TokenReview(user) → impersonate per member cluster (D1)
   │
   │  client-go, impersonating the UI user, per member
   ▼
Member cluster A   ·   Member cluster B   ·   Member cluster C
  each: k8s API + CRDs + controllers + swiftletd + Prometheus
```

Four **planes** (read / write / telemetry / console) over a **fleet** foundation (registry +
per-cluster client pool). The gateway is the only browser-facing surface; everything below it
is unchanged operator machinery. Almost all the work is **additive** — the existing
controllers/CRDs/swiftletd barely change; only telemetry (optional O5) and consoles (optional
swiftletd serial-on-a-port) need operator-side touches, and only when the relevant decision
picks that path. The genuinely new surface is the **hub** itself: the fleet-registry CRD, the
client pool, and the per-request impersonation.

## 3. Starting point (what exists today)

| Need | Today |
|---|---|
| API the browser calls | none (k8s API only; `swiftctl` uses client-go) |
| multi-cluster fan-out | none (`swiftctl` is single-cluster via the kubeconfig context) |
| proto / buf / codegen | none |
| per-VM metrics | cAdvisor on launcher pods via Prometheus, joined on `swift.kubeswift.io/guest` (**never pod name** — post-migration pods are `<guest>-mig-<uid>`); O5 swiftletd-counters deferred to v2 |
| fleet-state metrics | `kubeswift_*` gauges (O2 state collector) |
| serial console | a `swiftletd` serial unix socket; `swiftctl console` bridges it |
| graphical console | none — CH has no clean `-vnc` (QEMU/GPU path only) |
| action logic | in `internal/cli` (swiftctl) |
| admission validation | the existing CRD webhooks (fire regardless of caller) |

## 4. Decisions (RESOLVED 2026-06-22)

| # | Decision | Resolved | Consequence |
|---|---|---|---|
| **D1** | Auth model | **Impersonation** | Gateway validates the user token (TokenReview) → per-request impersonating client; existing k8s RBAC + Model-A namespace tenancy carry into the UI. Requires an impersonation-capable credential **in every member cluster** and, for authorization to be meaningful fleet-wide, a **shared identity provider** across the fleet (see D2/D2a). |
| **D2** | Cluster scope | **Multi-cluster aggregator** | The gateway is a **hub** that fans out across member clusters and merges their inventories. Threads a `cluster` dimension through the whole contract and enlarges every plane. |
| **D2a** | Cluster registry | **CRD registry in a hub** | New `fleet.kubeswift.io/v1alpha1` `Cluster` object + a per-member Secret (API endpoint + impersonation-capable credential + Prometheus endpoint); the gateway watches it and maintains a per-cluster client pool. Add a cluster = `kubectl apply`. |
| **D3** | API stability | **v1alpha1** | Breakable while the surface settles; `buf breaking` gates regressions; the UI repo pins a buf-published version; promote to v1 once real screens exercise it. |
| **D4** | Telemetry source | **cAdvisor via Prometheus (v1)** | Per-member Prometheus endpoint registered alongside each `Cluster`; join on `swift.kubeswift.io/guest`; every series carries a `cluster` label. O5 `vm.counters` is a v2 add (no proto break). |
| **D5** | Console transport | **swiftletd serial-on-a-port (medium-term); exec-pipe to bootstrap** | Gateway dials the **target cluster's** launcher serial port → WebSocket. Bootstrap path uses the k8s exec API via the impersonating per-cluster client until the swiftletd port lands. |
| **D6** | Deploy shape | **Separate containers** | UI = static nginx image (built from the `kubeswift-ui` repo); gateway = hub binary (this repo); the chart references both behind `gateway.enabled`. Independent release cadence. |
| **D7** | Gateway language | **Go** | Reuses CRD types, `internal/scheme`, client-go informers, impersonation, and the swiftctl action logic (Plane 4 extraction). Connect-Go is first-class; multi-cluster client pools are idiomatic client-go. |
| **D8** | proto shape | **Hand-written, UI-shaped** | Curated denormalized/aggregated views (e.g. `GetGuestDetail`) with the `cluster` dimension + fleet roll-ups; decoupled from CRD internals so CRD changes don't force UI breaks. |

## 5. Backlog — by plane

### Plane F — fleet registry + client pool (P0 — the multi-cluster foundation)
- [ ] `fleet.kubeswift.io/v1alpha1` `Cluster` CRD: API-endpoint ref, credential Secret ref, Prometheus endpoint, display name/labels, `status` (Ready / unreachable / last-sync). `make generate` + chart sync.
- [ ] Per-member **client pool**: watch `Cluster` objects → build/refresh an impersonation-capable client-go + informer cache per member; per-cluster health/Ready; drop + clean up on delete.
- [ ] Credential model: per-member Secret holding an impersonation-capable token/kubeconfig; the hub SA reads them. Document the **shared-IdP requirement** for cross-cluster impersonation, and the degradation when clusters don't federate.
- [ ] `ClusterService.List/Watch` so the UI's cluster selector + per-cluster health render.

### Plane 0 — the seam (P0)
- [ ] `proto/kubeswift/v1/` messages (Guest, Image, Kernel, SeedProfile, GuestClass, GPUProfile, GPUNode, Pool, Snapshot, Restore, Migration, SnapshotSchedule + conditions) — each carrying a **`cluster` field**; service defs where every List/Get/Watch takes a **cluster selector** (one / many / all).
- [ ] `buf.yaml` / `buf.gen.yaml` (connect-go), `make proto`, generated Go under `gen/`.
- [ ] CI: `buf lint` + `buf breaking` (vs the last tag); `buf push` so the UI repo pins a version.

### Plane 1 — gateway service (P0/P1) — `cmd/kubeswift-gateway`
- [ ] Connect-Go server (Connect + gRPC-Web + server-streaming), CORS, health/readiness, graceful shutdown, self-metrics.
- [ ] **Fan-out + merge** across the selected member clusters; per-cluster informer cache (not one); per-cluster error surfacing.
- [ ] controller-runtime client reusing `internal/scheme`.
- [ ] Helm: a `gateway` Deployment + Service (+ optional Ingress), the **hub SA + impersonation RBAC**, image via the `imageTag` helper, a `gateway.enabled` toggle.
- [ ] Build/release: add `kubeswift-gateway` to `make build-images` / the push matrix / `release-stable`.

### Plane 2 — auth (P0 design, P1 build) — security-critical
- [ ] D1 implemented (impersonation): token validation (TokenReview) → a **per-member** impersonating client.
- [ ] Per-RPC authz (SubjectAccessReview) on any non-impersonated path.
- [ ] Credential store handling: the hub holds impersonation-capable credentials to every member — rotation, scoping, hub RBAC.
- [ ] `/security-engineer` review: gateway RBAC, the **hub credential blast radius**, console-proxy escape surface, impersonation scoping, cross-cluster identity.

### Plane 3 — read (P0 minimal → P1 full)
- [ ] `GuestService.List` — **per-cluster server-side pagination + field-selector filter + sort, merged** across the selected clusters, `cluster` on every row (the UI virtual-scrolls a paged/filtered server view, not the whole fleet) — and `.Get`.
- [ ] `GuestService.Watch` — multiplexed server-stream over per-cluster informers → push updates (RxJS streams hang off these; no polling).
- [ ] List/Get/Watch for the other resources (Image/Pool/Snapshot/Restore/Migration/Schedule/…).
- [ ] `GuestService.GetDetail` — aggregated: status + launcher pod + recent Events + GPU + network + storage in one call.
- [ ] `EventService.Watch` (by involvedObject); `InventoryService` (clusters, nodes, namespaces for the GCP-style selectors, fleet counts from the `kubeswift_*` gauges).

### Plane 4 — write (P1)
- [ ] **Extract `swiftctl`'s action logic → `internal/actions`** so the CLI and the gateway call the same code (else they drift).
- [ ] CRUD across the CRDs + lifecycle (start/stop/restart) + snapshot/restore/migrate/schedule RPCs, routed to the target cluster.
- [ ] Image upload/import data path (stream a qcow2 → SwiftImage) — a binary path, separate from the JSON RPCs.
- [ ] Surface admission-webhook errors cleanly to the UI.

### Plane 5 — telemetry (P1)
- [ ] `TelemetryService.StreamVMMetrics` (server-stream): a **per-cluster** Prometheus client (endpoint from the `Cluster` object) + curated PromQL + the `swift.kubeswift.io/guest` join; `cluster` label on every series.
- [ ] A metrics catalog (series / units / scrape interval) the UI charts against.
- [ ] (v2) O5 swiftletd `vm.counters` + PodMonitor for richer guest-internal metrics.

### Plane 6 — console (P1 serial, P2 graphical)
- [ ] Raw-WebSocket serial endpoint ↔ the **target cluster's** guest serial socket (transport per D5).
- [ ] (optional) WS-SSH via the launcher pod (`swiftctl ssh` exists).
- [ ] (P2) noVNC: a per-guest VNC server (virtio-gpu / QEMU-only for GPU guests) + a WS proxy — **a backend feature, not a UI wrap; not v1.**

### Cross-cutting (P1)
- [ ] Watch-stream backpressure / per-client limits — now **N clusters × N tabs**; must not OOM the gateway.
- [ ] **Partial-fleet availability** — an unreachable member yields partial results + a per-cluster error, never a failed whole-fleet query (no silent failures).
- [ ] API reference (buf-generated) + a gateway operator doc (deploy / fleet registry / auth + shared IdP / Prometheus wiring / Ingress + cert).
- [ ] Gateway self-observability dashboard (fits the O-program).

## 6. The P0 cut — minimum to unblock UI scaffolding
1. `fleet.kubeswift.io/v1alpha1` `Cluster` CRD + the gateway's **per-member client pool** (watch registry → impersonating client + informer per member) + `ClusterService.List/Watch`.
2. `proto/kubeswift/v1` core messages (with the `cluster` dimension) + `GuestService` (List + Watch, cluster-selectable) + `Telemetry`/`Console` **stubs**.
3. buf codegen + CI lint/breaking.
4. `cmd/kubeswift-gateway` serving cross-cluster SwiftGuest List/Watch **read-only**, impersonation wired (a single SA-trusted member is an acceptable dev stub to start).
5. Helm `gateway.enabled` + image + the hub SA/RBAC.

→ The UI can then build the shell, the **cluster + namespace selectors**, and a **live,
fleet-merged inventory table** against real data. Write / telemetry / consoles layer on after.

## 7. Risks & non-goals

**Non-goals (this repo, this iteration):** the Angular app (separate repo); noVNC graphical
console in v1; O5 rich telemetry in v1. *(Multi-cluster is now in scope — D2.)*

**Risks:**
- **Cross-cluster identity (load-bearing).** Impersonation only authorizes where the subject is
  bound; the fleet should share an IdP (OIDC). If clusters don't federate, the gateway must
  document and surface the degradation. Promote to a P0 design note.
- **Hub credential blast radius.** The hub holds impersonation-capable credentials to every
  member — the fleet's highest-value target. Security review owns the credential store,
  rotation, and hub RBAC.
- **Auth surface.** A gateway fronting mutations + live consoles is the project's most
  security-sensitive component; D1 + the security review are load-bearing.
- **Fan-out watch scale.** N clusters × N tabs each watching the fleet; needs explicit
  backpressure / per-cluster limits.
- **Partial-fleet availability.** A single unreachable member must not fail the whole-fleet
  query — partial results + a per-cluster error surface (no silent failures).
- **swiftctl ↔ gateway drift** — mitigated by the `internal/actions` extraction (Plane 4).
- **CH graphical-console gap** — noVNC is a real backend feature, not a UI wrap; do not scope
  it into v1.
- **API churn** — `v1alpha1` + `buf breaking` keep it honest; the `cluster` dimension and the
  aggregated views will iterate as the UI surfaces real needs.

## 8. Cross-references
- UI repo: `kubeswift-ui` (separate; Angular static, Material 3, RxJS, static build).
- Telemetry: the O-program — [`docs/design/observability.md`](observability.md) + the
  cAdvisor-join recipe (`docs/observability/`).
- Action logic today: `internal/cli` (swiftctl).
- CRDs: `api/**`; the fleet-registry CRD: `api/fleet/v1alpha1`.
- Gateway operator guide: [`docs/ui/gateway.md`](../ui/gateway.md); samples: `config/samples/gateway/`.

## 9. P0 — shipped + cluster-validated (2026-06-22)

The P0 cut (§6) is built and proven end-to-end:

| PR | Increment |
|---|---|
| #259 | `fleet.kubeswift.io/v1alpha1` Cluster CRD (the registry) |
| #260 | `kubeswift.v1` Connect contract + buf codegen + CI |
| #261 | gateway skeleton + `ClusterService` |
| #263 | per-cluster client pool + impersonation + `GuestService` fan-out |
| #264 | Helm `gateway.enabled` + image + hub RBAC + release |

**Cluster validation:** the gateway (`kubeswift-gateway:sha-6fe5e7d`, `authMode=insecure`) was deployed
on a hub and registered **two independent real clusters** as fleet members (the hub itself via an
in-cluster SA, and a separate k0s 1.35 cluster reachable over the network). `ClusterService.ListClusters`
returned both Ready with their real Kubernetes versions (v1.34.3 + v1.35.3); `GuestService.ListGuests`
**merged** guests from the cluster that had them — cluster dimension + phase + boot source all mapped —
**and** surfaced the other cluster's missing `swiftguests` CRD as a **per-cluster error** rather than
failing the whole-fleet query. That is the no-silent-failures partial-fleet property (D2) working in
production.

The separate `kubeswift-ui` repo can now build its shell + cluster/namespace selectors + a live,
fleet-merged inventory against this gateway. **Remaining backend work is the P1+ planes** (§5): the write
plane (CRUD/lifecycle via an `internal/actions` extraction from swiftctl), telemetry (Prometheus →
`TelemetryService`), consoles (serial WebSocket), the aggregated `GetGuestDetail`, and `Cluster.status`
guest-count enrichment (the pool writes Ready/version today; per-cluster guest counts are P1).
