# Production auth for the KubeSwift UI (OIDC + Kubernetes RBAC)

This is the operator runbook for turning the KubeSwift console from the dev
default (no login) into a multi-user, single-sign-on deployment where each
person's **Kubernetes RBAC** decides what they can see and do.

> Two decisions shape it (see [`docs/design/ui-phase-2-explorer-and-auth.md`](../design/ui-phase-2-explorer-and-auth.md)):
> **A1 — gateway-side OIDC**: the gateway verifies the IdP token itself, so the
> member API servers need *not* be OIDC-wired. **A2 — Kubernetes RBAC is the
> permission model**: the gateway impersonates the signed-in user, so their k8s
> Roles/ClusterRoles *are* their permissions.

## How it fits together

```
Browser (kubeswift-ui)
  │  1. OIDC login (Authorization Code + PKCE) ──► IdP (Keycloak)
  │  2. ID token ──► every gateway call (Authorization: Bearer)
  ▼
kubeswift-gateway  (auth-mode=oidc)
  │  3. verify the ID token against the issuer (JWKS)
  │  4. map claims → user + groups, then IMPERSONATE them
  ▼
Member cluster  ── Kubernetes RBAC gates the impersonated user ──►  allow / deny
```

Nothing in KubeSwift stores permissions: the IdP authenticates, and k8s RBAC
authorizes. A denial surfaces in the UI naming the user — never a silent gap.

## 1. Deploy an IdP (Keycloak)

Any OIDC provider works (Keycloak, Dex, a cloud IdP). Keycloak example — realm
`kubeswift`, a **public** client for the browser, a **groups** mapper, and a
group to bind to a role:

```bash
# In the Keycloak admin CLI (kcadm.sh), as the realm admin:
kcadm create realms -s realm=kubeswift -s enabled=true

# Public client: standard (Authorization Code + PKCE) flow for the browser.
kcadm create clients -r kubeswift \
  -s clientId=kubeswift-gateway -s enabled=true \
  -s publicClient=true -s standardFlowEnabled=true \
  -s 'redirectUris=["https://kubeswift-ui.example.com/*"]' \
  -s 'webOrigins=["https://kubeswift-ui.example.com"]'

# Emit a "groups" claim (bare group names) so RBAC can bind groups.
CID=$(kcadm get clients -r kubeswift -q clientId=kubeswift-gateway --fields id --format csv --noquotes | tail -1)
kcadm create clients/$CID/protocol-mappers/models -r kubeswift \
  -s name=groups -s protocol=openid-connect -s protocolMapper=oidc-group-membership-mapper \
  -s 'config."claim.name"=groups' -s 'config."full.path"=false' \
  -s 'config."access.token.claim"=true' -s 'config."id.token.claim"=true'

# A group + a user in it.
kcadm create groups -r kubeswift -s name=kubeswift-operators
kcadm create users -r kubeswift -s username=alice -s enabled=true -s email=alice@example.com -s emailVerified=true
kcadm set-password -r kubeswift --username alice --new-password '<password>'
# (add alice to kubeswift-operators in the admin UI or via kcadm)
```

> **Issuer reachability is the one gotcha.** The gateway verifies the token's
> issuer (its JWKS) and the **browser** drives the login against the same
> issuer — so Keycloak must be reachable, at **one** issuer URL, from *both* the
> browser and the in-cluster gateway. Expose Keycloak at a real hostname (an
> Ingress / LoadBalancer) resolvable everywhere; don't use an in-cluster-only
> Service name as the issuer.

## 2. Point the gateway at the IdP

Run the gateway in `oidc` mode (chart values or container args):

```bash
helm upgrade --install kubeswift oci://ghcr.io/projectbeskar/charts/kubeswift \
  -n kubeswift-system \
  --set gateway.enabled=true \
  --set gateway.authMode=oidc
# + the OIDC flags on the gateway container:
#   --oidc-issuer-url=https://keycloak.example.com/realms/kubeswift
#   --oidc-client-id=kubeswift-gateway
#   --oidc-username-claim=email      (default; or preferred_username)
#   --oidc-groups-claim=groups       (default)
#   --oidc-username-prefix= / --oidc-groups-prefix=   (optional, mirror apiserver)
```

The gateway verifies tokens itself, so the **member API servers do not need
`--oidc-*` flags**.

## 3. Point the UI at the IdP

The UI reads two runtime globals (its `config.js`, injected by the chart from
env). Setting both turns login on:

```js
window.__KUBESWIFT_OIDC_ISSUER__ = 'https://keycloak.example.com/realms/kubeswift';
window.__KUBESWIFT_OIDC_CLIENT_ID__ = 'kubeswift-gateway';
```

Unset → the UI runs with no login (dev/insecure mode).

## 4. Member-side RBAC (impersonation + the user's permissions)

Impersonation happens **on each member**, using that member's gateway
credential. Two grants per member:

```yaml
# (a) Let the gateway credential impersonate end users + groups.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: { name: kubeswift-gateway-impersonator }
rules:
  - apiGroups: [""]
    resources: ["users", "groups"]
    verbs: ["impersonate"]
# ...bound to the gateway's member ServiceAccount (see config/samples/gateway/).
```

```text
# (b) Bind your IdP subjects to KubeSwift roles. Do this from the UI's
#     Access tab, or with kubectl. The three predefined roles:
#       kubeswift-admin     — full control incl. managing access
#       kubeswift-operator  — start/stop, console, migrate, snapshots, view
#       kubeswift-viewer    — read-only
```

The predefined ClusterRoles are created on demand the first time you assign one
(no manifest to pre-apply). A binding maps a subject to a role, **cluster-wide**
(ClusterRoleBinding) or **per namespace** (RoleBinding).

## 5. Manage roles + access from the UI

The console's **Access** tab is a Kubernetes-RBAC editor (gated by the signed-in
user's own `rbac` permissions — i.e. you need the `kubeswift-admin` role, or the
`manage-rbac` capability):

- **Assign a role** to a Keycloak **User** or **Group**, cluster-wide or scoped
  to a namespace.
- **Build a custom role** from granular **capabilities** — `view-vms`,
  `manage-vms`, `console`, `migrate`, `manage-snapshots`, `view-resources`,
  `view-secrets`, `manage-rbac` — each mapping to a fixed set of RBAC rules. The
  three predefined roles are just capability compositions.
- **Review + remove** existing assignments.

Everything the editor does is plain Kubernetes RBAC (labelled
`kubeswift.io/role` / `kubeswift.io/access-binding`), so it is equally visible
and manageable with `kubectl`.

## Security notes

- **The hub holds every member's credential** — restrict who can read Secrets in
  the gateway namespace; scope each member credential to least privilege.
- **The bearer token is the user's** — served over TLS (terminate at the
  ingress; the gateway speaks h2c behind it). The console WebSocket carries the
  token in `?token=` (browsers can't set a WS auth header) — prefer short-lived
  tokens and TLS so it isn't logged.
- **`auth-mode=insecure` bypasses all of this** (no impersonation; every user
  inherits the gateway credential). Never use it in production — the gateway
  logs a warning at startup when it is on.

## See also

- [`gateway.md`](gateway.md) — the gateway itself + the auth-mode table.
- `config/samples/gateway/` — member-RBAC + explorer-RBAC samples.
