# kubeswift-gateway (UI backend)

`kubeswift-gateway` is the browser-facing backend for the separate **kubeswift-ui**
app. It is a [Connect](https://connectrpc.com/) (gRPC-Web–compatible) server that
runs in a **hub** cluster, discovers **member** clusters from a registry CRD, and
fans the read plane out across the fleet — so the UI talks to one endpoint and
sees every cluster's VMs.

> Status: **v1alpha1**, opt-in. P0 ships the read plane (cluster selector + a
> cross-cluster SwiftGuest inventory). Write, telemetry, and consoles follow.

## Architecture

```
Browser (kubeswift-ui)
   │ Connect / gRPC-Web
   ▼
kubeswift-gateway  (HUB cluster)
   ├─ watches fleet.kubeswift.io Cluster objects (the registry)
   ├─ per member: a client built from the member's credential Secret,
   │   impersonating the end user (auth-mode=token)
   └─ ClusterService.List/Watch + GuestService.List/Watch (fan-out + merge)
   ▼
Member cluster A · Member cluster B · …   (each runs the KubeSwift operator)
```

The hub is just where the gateway runs; it may also be one of the member
clusters, or a dedicated management cluster.

## Install (on the hub)

The gateway ships with the KubeSwift chart, gated off by default:

```bash
helm upgrade --install kubeswift oci://ghcr.io/kubeswift-io/charts/kubeswift \
  -n kubeswift-system --create-namespace \
  --set gateway.enabled=true \
  --set gateway.authMode=token        # see "Authentication" below
```

This deploys the gateway Deployment + Service + its hub RBAC. Expose it to the
UI with `gateway.ingress.enabled=true` (+ `gateway.ingress.host`/`annotations`),
or a Service of `type: LoadBalancer`. The gateway serves Connect/gRPC-Web over
**h2c** — terminate TLS at the ingress and allow HTTP/2 to the backend.

For cert-manager TLS without hand-writing the `tls[]` block, set
`ingress.tlsAuto.enabled=true` + one of `ingress.tlsAuto.clusterIssuer` /
`ingress.tlsAuto.issuer` (on either `ui.ingress` or `gateway.ingress`); the chart
emits the `tls[]` entry (Secret `<host>-tls`, overridable) and the cert-manager
annotation. The raw `annotations`/`tls[]` fields stay as escape hatches
(`tlsAuto` wins on the issuer annotation when both are set).

## Register a member cluster

For each member, create — **in the gateway's namespace** — a credential Secret
and a `Cluster` object. The simplest credential is a kubeconfig:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: boba-kubeconfig
  namespace: kubeswift-system
type: Opaque
stringData:
  kubeconfig: |
    # a kubeconfig for the member, whose user is impersonation-capable (below)
---
apiVersion: fleet.kubeswift.io/v1alpha1
kind: Cluster
metadata:
  name: boba
  namespace: kubeswift-system
spec:
  server: https://boba.example.com:6443       # optional if the kubeconfig has it
  credentialSecretRef:
    name: boba-kubeconfig
  prometheusEndpoint: http://prometheus.monitoring.svc:9090   # optional (telemetry)
  displayName: Boba (lab)
```

Alternatively the Secret may carry a `token` (+ optional `ca.crt`) instead of a
`kubeconfig`, paired with `spec.server`.

The gateway picks the member up immediately, probes it, and writes status:

```bash
kubectl -n kubeswift-system get clusters
# NAME   SERVER                          READY   K8S       GUESTS   AGE
# boba   https://boba.example.com:6443   True    v1.34.3   7        1m
```

## Authentication (decision D1)

| `gateway.authMode` | Behaviour |
|---|---|
| `oidc` (**production**) | The UI logs the user in against an **IdP** (Keycloak/Dex) and sends the resulting OIDC **ID token**; the gateway verifies it against the issuer (`--oidc-issuer-url`/`--oidc-client-id`, JWKS auto-discovered), maps its claims to a user+groups (`--oidc-username-claim` default `email`, `--oidc-groups-claim` default `groups`, optional `--oidc-username-prefix`/`--oidc-groups-prefix`), and **impersonates** that subject against each member. Works even when the member API servers are *not* OIDC-wired (the gateway, not the apiserver, verifies the token). Full setup: [`auth.md`](auth.md). |
| `token` | The UI sends the end user's bearer token; the gateway validates it via the hub's **TokenReview**, then impersonates that user. Use when the cluster's API server is already OIDC-wired (or for ServiceAccount tokens). |
| `insecure` (**dev/lab only**) | No impersonation — member queries run as the member's own credential. **Every UI user inherits whatever that credential grants.** Do not use in production. |

For `oidc`/`token` to authorize uniformly across the fleet, the impersonated
subject (the OIDC username/groups) must be **bound in every member's RBAC** —
so the fleet should share one identity provider. Where members don't federate,
impersonation is only valid where the subject is bound.

### Member-side permissions

Impersonation happens **on the member**, using the member credential — so that
credential must be allowed to impersonate, and the impersonated users must be
able to read SwiftGuests. On each member:

```yaml
# Let the gateway's member credential impersonate end users + groups.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeswift-gateway-impersonator
rules:
  - apiGroups: [""]
    resources: ["users", "groups"]
    verbs: ["impersonate"]
---
# And let the (impersonated) users read VMs — bind the real users/groups to a
# role granting get/list/watch on swiftguests, scoped to their namespaces.
```

The **hub** SA needs no `impersonate` verb; the chart grants it only what it
needs on the hub (read `clusters` + credential Secrets, write `clusters/status`,
create `tokenreviews`).

## Verify

```bash
kubectl -n kubeswift-system port-forward svc/kubeswift-gateway 8080:8080 &
# List clusters (Connect over HTTP/1.1 JSON):
curl -s http://localhost:8080/kubeswift.v1.ClusterService/ListClusters \
  -H 'Content-Type: application/json' -d '{}'
# List guests across the fleet:
curl -s http://localhost:8080/kubeswift.v1.GuestService/ListGuests \
  -H 'Content-Type: application/json' -d '{}'
```

In `insecure` mode these work with no token; in `token` mode add
`-H "Authorization: Bearer <user-token>"`.

## Security notes

- **The hub holds credentials to every member** — it is the fleet's
  highest-value target. Restrict who can read Secrets in the gateway namespace,
  scope each member credential to the least privilege the UI needs, and rotate.
- **`insecure` mode is a footgun** — it bypasses per-user authorization. The
  gateway logs a warning at startup when it is on. Note the **write actions**
  below make this sharper: in `insecure` mode every UI user can start/stop VMs.
- **Write actions** — `GuestService.StartGuest`/`StopGuest` patch
  `swiftguests.spec.runPolicy` on the target member, as the impersonated user.
  `StopGuest` additionally **deletes the launcher pod** (the SwiftGuest stop
  guard is reactive — a runPolicy patch alone does not stop a running VM). So
  the acting subject needs `patch` on `swiftguests` **and** `delete` on `pods`
  there (see `config/samples/gateway/member-rbac.yaml`); a read-only audience
  gets a clean permission denial. In `token` mode the member's RBAC gates who
  can act.
- **Console** — a raw WebSocket at `/console?cluster=&namespace=&name=&token=`
  exec-bridges the guest's serial socket. It execs `socat` in the launcher
  pod, so the acting subject needs `create` on `pods/exec` — **powerful**
  (arbitrary in-pod commands); grant it only to console users. Because browsers
  can't set a WebSocket `Authorization` header, the bearer token rides the
  `?token=` query param — which **can land in proxy/access logs**; prefer a
  short-lived token, and a TLS-terminating ingress so the URL isn't on the wire.
  `insecure` mode needs no token (and then anyone can open any console).
- **Console transport — exec-pipe is the chosen transport for the hub** (not
  D5's "serial-on-a-port"). The gateway is a multi-cluster hub: it reaches a
  member's launcher pod *through that member's API server* (the impersonating
  client), never by dialing the pod's IP directly across cluster networks. The
  exec-pipe already rides that path (the `pods/exec` subresource). A
  swiftletd-serial-on-a-TCP-port transport would still have to tunnel through
  the API server (the `pods/portforward` subresource) for a cross-cluster hub —
  a lateral move (swap `exec`+`socat` for `portforward`, a different RBAC verb,
  the same API-server tunnel) at the cost of a swiftletd change. So the
  exec-pipe is kept; serial-on-a-port is only worth revisiting for a
  single-cluster, in-cluster console (where a direct pod dial is possible).
- **CORS** defaults to `*` (safe for a token-auth API with no cookies); pin
  `gateway.corsAllowOrigin` to the UI origin for a hardened install.

## See also

- `config/samples/gateway/` — runnable Secret + Cluster samples.
