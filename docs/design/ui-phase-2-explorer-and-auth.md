# UI Phase 2 — Cluster Explorer + Production Auth

> Status: **design**, in progress. Builds on the P0/P1/P2 gateway + console
> (`docs/design/ui-backend-enablement.md`). Two arcs: a read-only **cluster
> explorer** (arc A, first) and **production multi-user auth** (arc B, next).

## Why

The console today shows SwiftGuests + migrations and can start/stop/console/
migrate them. That is a VM operator's view, not a cluster operator's. Two gaps:

1. **No cluster context.** An operator can't see the member's nodes, namespaces,
   networking (Services / NADs / NetworkPolicies), storage (PVCs / PVs /
   StorageClasses), config (Secrets / ConfigMaps), or the other KubeSwift CRDs
   (Images, Kernels, Classes, Pools, Snapshots, GPU). They have to drop to
   `kubectl`.
2. **No production auth.** The gateway supports `authMode=token` (TokenReview →
   per-user impersonation, D1) but the dev cluster runs `insecure`, and there is
   no browser login flow and no multi-user role story.

## Resolved decisions (this phase)

| # | Decision | Choice |
|---|---|---|
| E1 | Explorer fan-out model | **Per-cluster** (the explorer is "about *the* cluster" — the UI's cluster selector picks one). `cluster` is required, mirroring `ClusterService.ListNodes`. Fleet-wide browse can come later; the row already carries the cluster dimension. |
| E2 | Resource exposure shape | **One generic `ResourceService`** (list-by-kind over the impersonating dynamic client), not per-type RPCs. A server-owned **catalog** (`ListResourceKinds`) drives the UI's left nav, so "and more" is a gateway-side catalog edit, not a new RPC + screen each time. |
| E3 | Per-kind columns | The gateway **projects** each object into a `map<string,string> columns` (kind-specific: a Pod's node/ip/ready, a PVC's capacity/status/class, …). The proto stays generic; the projection (and its redaction) lives in one place. |
| E4 | **Secrets** | **Metadata only — never values.** The Secret projector emits `type` + the **key names** + a key count, and never reads `.data` values. (RBAC still gates *whether* a user may list Secrets at all; the gateway redaction is defense-in-depth so the UI never renders a value even to someone who could.) |
| E5 | Namespace filter | A global namespace selector in the toolbar threads `namespace` into every namespaced `ListResources`. Its options come from `ListResources(kind=namespaces)` — no extra RPC. |
| A1 | IdP integration point | **Gateway-side OIDC** (Dex / Keycloak). The gateway verifies the OIDC ID token itself (issuer JWKS) and maps claims → impersonated user+groups — works even when the member API servers are *not* OIDC-wired. (User decision.) |
| A2 | Role / permission model | **Kubernetes RBAC.** The impersonated subject's Roles/ClusterRoles *are* the permissions (composes with Model-A tenancy; one source of truth). "Define roles" = a UI editor over real k8s RBAC objects, a later sub-feature. (User decision.) |

## Arc A — Cluster explorer (read-only)

### Gateway: `ResourceService`

```proto
service ResourceService {
  rpc ListResourceKinds(ListResourceKindsRequest) returns (ListResourceKindsResponse);
  rpc ListResources(ListResourcesRequest) returns (ListResourcesResponse);
}
```

- `ListResourceKinds` returns the **catalog**: each `ResourceKind` is `{key,
  displayName, group, version, resource, namespaced, category, columns[]}`.
  `category` groups the left nav (Cluster / Workloads / Networking / Storage /
  Config / KubeSwift). `columns[]` is the ordered header the table renders.
- `ListResources(cluster, kind, namespace)` lists one kind on one member as the
  impersonated user (per-cluster, like `ListNodes`). Each item becomes a
  `Resource{ref, kind, createdAt, columns}`. A kind absent on that cluster (e.g.
  no Multus → no NADs), or an RBAC denial, returns a `ClusterError` — never a
  silent empty list.

The catalog is static gateway config (a Go slice). Each entry carries a
**projector** `func(*unstructured.Unstructured) map[string]string` that fills the
columns. Core kinds get specific projectors (Node roles/version/ready, Service
type/clusterIP/ports, PVC capacity/status/class, Secret type/keys, …); KubeSwift
CRDs and unknown kinds fall back to a generic `status.phase` + age projection.

**v1 catalog** (extensible): Nodes, Namespaces, StorageClasses,
PersistentVolumes (Cluster); Pods (Workloads); Services,
NetworkAttachmentDefinitions, NetworkPolicies (Networking);
PersistentVolumeClaims (Storage); Secrets, ConfigMaps (Config); SwiftImages,
SwiftKernels, SwiftGuestClasses, SwiftGuestPools, SwiftSnapshots, SwiftGPUNodes
(KubeSwift — the CRDs without a dedicated view; SwiftGuests/SwiftMigrations keep
theirs).

### RBAC

The impersonated user needs `get,list` on the browsable kinds. A new
`kubeswift-explorer-reader` ClusterRole in the member-RBAC sample bundles them,
with **Secrets as an explicit, separable rule** (grant only to audiences allowed
to browse Secret metadata). In `insecure` dev mode the gateway's own member
credential needs the same read.

### UI

A left **resource nav** (catalog-driven, grouped by category) + a global
**namespace filter** in the toolbar + a generic **resource table** (columns from
the selected kind, name/namespace/age always, the projected `columns` after).
Rows are clickable for a future detail drawer (out of scope for the first cut).

## Arc B — Production auth (next sub-phase)

### Gateway: a third auth mode, `oidc`

`buildAuthenticator` gains `oidc` alongside `insecure`/`token`. An
`oidcAuthenticator` verifies the request's bearer token as an **OIDC ID token**
against a configured issuer (`--oidc-issuer-url`, `--oidc-client-id`, JWKS
auto-discovered via `github.com/coreos/go-oidc`), then maps configurable claims
(`--oidc-username-claim` default `email`, `--oidc-groups-claim` default `groups`,
optional `--oidc-username-prefix`/`--oidc-groups-prefix`) → `Identity{User,
Groups}`. Everything downstream (impersonation, RBAC) is unchanged — this only
swaps *how the bearer token becomes an Identity*. TokenReview (`token`) stays for
clusters that *are* OIDC-wired at the API server.

### UI: OIDC login (PKCE)

A login route runs the OIDC Authorization Code + PKCE flow against the IdP
(Dex/Keycloak), stores the ID token, attaches it as `Authorization: Bearer` on
every Connect call and as `?token=` on the console WS (already supported), and
refreshes/redirects on 401. A `WhoAmI`-style call (or decoding the ID token)
shows the signed-in user.

### Roles: Kubernetes RBAC, with a UI editor later

v1 ships the auth + documents binding IdP subjects to k8s Roles (the
member-RBAC sample already shows the shape). A **RBAC editor** (list/create/bind
Roles & ClusterRoles from the UI, scoped by the user's own `rbac` permissions)
is a follow-on once the explorer's generic resource plumbing exists — it is just
two more kinds (Roles, RoleBindings) plus create/bind forms.

### Deployment recipe

Dex (federating Google/LDAP/GitHub) or Keycloak in the hub, the gateway pointed
at its issuer (`gateway.authMode=oidc` + the OIDC flags/secret), the UI given the
issuer + client-id. Documented in `docs/ui/auth.md` (arc B).

## Sequencing

1. **A1** — `ResourceService` proto + gateway (catalog, projectors, RBAC) + tests. ← *this PR*
2. **A2** — UI: nav + namespace filter + table. Cluster-validate.
3. **B1** — gateway `oidc` auth mode + flags + tests.
4. **B2** — UI OIDC login + token propagation.
5. **B3** — RBAC editor (Roles/Bindings as explorer kinds + forms) + auth docs + Dex/Keycloak recipe.

## Non-goals (this phase)

Write actions on arbitrary resources (the explorer is read-only — VM lifecycle
stays the typed Start/Stop/Migrate path); editing YAML in the UI; per-resource
detail drawers (a fast follow); packaging the UI as an image + chart toggle
(tracked separately).
