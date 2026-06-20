# Design: UI Backend Enablement — gateway, contract, and data planes

> Status: **PROPOSED** — backlog + decision record for the KubeSwift **backend** work that
> unlocks a separate `kubeswift-ui` (Angular static app). No code in this doc; it is the
> build plan + the open decisions. The Angular app itself is out of scope (separate repo).
> First deliverable: the **P0 cut** (§6).

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

**Repo split (decided):** the Angular UI lives in a **separate repo** (`kubeswift-ui`); the
**gateway + proto live here** (the gateway imports `api/swift/v1alpha1` + client-go and is
tightly coupled to the operator's data model). The proto is the versioned seam, so both
sides develop in parallel against a frozen contract.

## 2. Architecture

```
Browser  (Angular static — separate repo)
   │  Connect / gRPC-Web (data)        +        raw WebSocket (consoles)
   ▼
kubeswift-gateway   (NEW — this repo, cmd/kubeswift-gateway)
   ├─ Read plane       list / get / watch CRDs (informers) → server-stream
   ├─ Write plane      CRUD + lifecycle / snapshot / migrate (shared with swiftctl)
   ├─ Telemetry plane  Prometheus (cAdvisor join on swift.kubeswift.io/guest)
   └─ Console plane     WS ↔ serial.sock  (and later VNC)
   │  client-go  (impersonating the UI user — D1)
   ▼
Kubernetes API  +  CRDs / controllers  +  swiftletd  +  Prometheus
```

Four **planes** (read / write / telemetry / console). The gateway is the only
browser-facing surface; everything below it is unchanged operator machinery. Almost all
the work is **additive** — the existing controllers/CRDs/swiftletd barely change; only
telemetry (optional O5) and consoles (optional swiftletd serial-on-a-port) need operator-
side touches, and only if the relevant decision picks that path.

## 3. Starting point (what exists today)

| Need | Today |
|---|---|
| API the browser calls | none (k8s API only; `swiftctl` uses client-go) |
| proto / buf / codegen | none |
| per-VM metrics | cAdvisor on launcher pods via Prometheus, joined on `swift.kubeswift.io/guest` (**never pod name** — post-migration pods are `<guest>-mig-<uid>`); O5 swiftletd-counters deferred to v2 |
| fleet-state metrics | `kubeswift_*` gauges (O2 state collector) |
| serial console | a `swiftletd` serial unix socket; `swiftctl console` bridges it |
| graphical console | none — CH has no clean `-vnc` (QEMU/GPU path only) |
| action logic | in `internal/cli` (swiftctl) |
| admission validation | the existing CRD webhooks (fire regardless of caller) |

## 4. Open decisions (resolve before building)

| # | Decision | Options | Recommendation |
|---|---|---|---|
| **D1** | **Auth model** | (a) impersonation · (b) SA-trusted proxy | **(a) impersonation** — user authenticates (OIDC/k8s token) → gateway sets `Impersonate-User/Groups` → existing k8s RBAC applies per-user; makes Model-A multi-tenancy real in the UI. SA-trusted (gateway's own SA, every user inherits its power) only for single-admin dev. |
| **D2** | **Cluster scope** | (a) single-cluster · (b) multi-cluster aggregator | **(a) single-cluster** for v1 (the operator is per-cluster). Multi-cluster is a fundamentally different gateway; defer unless a hard requirement. |
| **D3** | **API stability** | `v1alpha1` · `v1` | **`v1alpha1`** — breakable until the surface settles; mirror the CRD tier. `buf breaking` gates regressions once tagged. |
| **D4** | **Telemetry source** | (a) cAdvisor-via-Prometheus · (b) O5 swiftletd `vm.counters` | **(a) for v1** (exists now), **(b) for v2** (richer guest-internal metrics; a swiftletd change, already roadmapped). |
| **D5** | **Console transport** | (a) gateway exec-into-pod + pipe · (b) swiftletd exposes serial on a port | **(b) medium-term** (cleaner to scale, no per-stream exec RBAC), **(a) acceptable to bootstrap.** (b) is a small swiftletd change. |
| **D6** | **Deploy shape** | (a) gateway + UI as separate containers · (b) `embed.FS` the UI into the gateway | **(a) start separate** (UI is a static nginx container the chart references); **(b)** later if you want one binary. |
| **D7** | **Gateway language** | Go · Rust | **Go** — imports `api/swift/v1alpha1` + client-go; Rust/kube-rs would re-derive the whole model. |
| **D8** | **proto shape** | hand-written curated · generated 1:1 from CRDs | **hand-written, UI-shaped** — denormalized/aggregated views, not raw CRD JSON. |

## 5. Backlog — by plane

### Plane 0 — the seam (P0)
- [ ] `proto/kubeswift/v1/` messages (Guest, Image, Kernel, SeedProfile, GuestClass, GPUProfile, GPUNode, Pool, Snapshot, Restore, Migration, SnapshotSchedule + conditions) and service defs.
- [ ] `buf.yaml` / `buf.gen.yaml` (connect-go), `make proto`, generated Go under `gen/`.
- [ ] CI: `buf lint` + `buf breaking` (vs the last tag); `buf push` so the UI repo pins a version.

### Plane 1 — gateway service (P0/P1) — `cmd/kubeswift-gateway`
- [ ] Connect-Go server (Connect + gRPC-Web + server-streaming), CORS, health/readiness, graceful shutdown, self-metrics.
- [ ] controller-runtime client reusing `internal/scheme`; informer cache backing the watches.
- [ ] Helm: a `gateway` Deployment + Service (+ optional Ingress), its SA + RBAC, image via the `imageTag` helper, a `gateway.enabled` toggle.
- [ ] Build/release: add `kubeswift-gateway` to `make build-images` / the push matrix / `release-stable`.

### Plane 2 — auth (P0 design, P1 build) — security-critical
- [ ] D1 implemented (impersonation): token validation (TokenReview) → per-request impersonating client.
- [ ] Per-RPC authz (SubjectAccessReview) on any non-impersonated path.
- [ ] `/security-engineer` review: gateway RBAC, console-proxy escape surface, impersonation scoping.

### Plane 3 — read (P0 minimal → P1 full)
- [ ] `GuestService.List` — **server-side pagination + field-selector filter + sort** (the UI virtual-scrolls a paged/filtered server view, not the whole fleet) — and `.Get`.
- [ ] `GuestService.Watch` — server-stream over an informer → push updates (RxJS streams hang off these; no polling).
- [ ] List/Get/Watch for the other resources (Image/Pool/Snapshot/Restore/Migration/Schedule/…).
- [ ] `GuestService.GetDetail` — aggregated: status + launcher pod + recent Events + GPU + network + storage in one call.
- [ ] `EventService.Watch` (by involvedObject); `InventoryService` (nodes, namespaces for the GCP-style selector, fleet counts from the `kubeswift_*` gauges).

### Plane 4 — write (P1)
- [ ] **Extract `swiftctl`'s action logic → `internal/actions`** so the CLI and the gateway call the same code (else they drift).
- [ ] CRUD across the CRDs + lifecycle (start/stop/restart) + snapshot/restore/migrate/schedule RPCs.
- [ ] Image upload/import data path (stream a qcow2 → SwiftImage) — a binary path, separate from the JSON RPCs.
- [ ] Surface admission-webhook errors cleanly to the UI.

### Plane 5 — telemetry (P1)
- [ ] `TelemetryService.StreamVMMetrics` (server-stream): a Prometheus client + curated PromQL + the `swift.kubeswift.io/guest` join.
- [ ] A metrics catalog (series / units / scrape interval) the UI charts against.
- [ ] (v2) O5 swiftletd `vm.counters` + PodMonitor for richer guest-internal metrics.

### Plane 6 — console (P1 serial, P2 graphical)
- [ ] Raw-WebSocket serial endpoint ↔ the guest serial socket (transport per D5).
- [ ] (optional) WS-SSH via the launcher pod (`swiftctl ssh` exists).
- [ ] (P2) noVNC: a per-guest VNC server (virtio-gpu / QEMU-only for GPU guests) + a WS proxy — **a backend feature, not a UI wrap; not v1.**

### Cross-cutting (P1)
- [ ] Watch-stream backpressure / per-client limits (N tabs each watching the fleet must not OOM the gateway).
- [ ] API reference (buf-generated) + a gateway operator doc (deploy / auth / Prometheus wiring / Ingress + cert).
- [ ] Gateway self-observability dashboard (fits the O-program).

## 6. The P0 cut — minimum to unblock UI scaffolding
1. `proto/kubeswift/v1` core messages + `GuestService` (List + Watch) + `Telemetry`/`Console` **stubs**.
2. buf codegen + CI lint.
3. `cmd/kubeswift-gateway` serving SwiftGuest List/Watch **read-only**; D1 decided (SA-trusted acceptable as a dev stub).
4. Helm `gateway.enabled` + image + RBAC.

→ The UI can then build the shell, the namespace selector, and a **live inventory table**
against real data. Write / telemetry / consoles layer on after.

## 7. Risks & non-goals

**Non-goals (this repo, this iteration):** the Angular app (separate repo); noVNC graphical
console in v1; O5 rich telemetry in v1; multi-cluster in v1.

**Risks:**
- **Auth surface** — a gateway fronting mutations + live consoles is the project's most
  security-sensitive component. D1 + the security review are load-bearing.
- **Watch-stream scale** — many browser tabs each watching the fleet; needs explicit
  backpressure / limits.
- **swiftctl ↔ gateway drift** — mitigated by the `internal/actions` extraction (Plane 4).
- **CH graphical-console gap** — noVNC is a real backend feature, not a UI wrap; do not
  scope it into v1.
- **API churn** — `v1alpha1` + `buf breaking` keep it honest; expect iteration as the UI
  surfaces real needs.

## 8. Cross-references
- UI repo: `kubeswift-ui` (separate; Angular static, Material 3, RxJS, static build).
- Telemetry: the O-program — [`docs/design/observability.md`](observability.md) + the
  cAdvisor-join recipe (`docs/observability/`).
- Action logic today: `internal/cli` (swiftctl).
- CRDs: `api/**`.
