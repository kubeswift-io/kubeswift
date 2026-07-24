# The Resource Explorer (browse + RBAC-gated CRUD)

The Explorer is the console's generic resource browser and editor. It lists any
kind in the gateway's catalog — the KubeSwift CRDs **and** the common native
Kubernetes kinds — and lets you create, edit, and delete them. Every action runs
as **you**: the gateway impersonates the signed-in user, so your Kubernetes RBAC
is the only thing that decides what you can do. See [auth.md](auth.md) for how
impersonation is wired.

> The console never decides your permissions. Before it *offers* an action it
> asks the API server "can I?" — a `SelfSubjectAccessReview`, evaluated as you —
> and the API server re-checks again on the real write. Two gates, one source of
> truth: your RBAC.

## What you can browse

The left nav is grouped by category and driven by a server-owned catalog:

| Category | Kinds |
|---|---|
| Cluster | Nodes, Namespaces, Storage Classes, Persistent Volumes |
| Workloads | Deployments, StatefulSets, DaemonSets, ReplicaSets, Jobs, CronJobs, Pods |
| Networking | Services, Ingresses, Network Attachments, Network Policies |
| Storage | Persistent Volume Claims |
| Config | Secrets, Config Maps |
| Access | Service Accounts, Roles, RoleBindings, ClusterRoles, ClusterRoleBindings |
| KubeSwift | Images, Kernels, Guest Classes, Guest Pools, Sandboxes, Sandbox Pools, Seed Profiles, Snapshots, Snapshot Schedules, Restores, GPU Profiles, GPU Nodes |

The console is **per-cluster** — pick a member in the top selector. A namespace
filter scopes namespaced kinds; cluster-scoped kinds (Nodes, ClusterRoles, …)
ignore it.

Two things are read-only by nature, so they never show a **New** button even for
an admin: **Nodes** (kubelet-owned) and **GPU Nodes** (discovery-owned, no spec).
**Secrets** list their key *names* only — never their values (see below).

## How the gating works

When you select a kind and namespace, the console batch-asks the gateway whether
you may `create` / `update` / `delete` it there, and renders only what you're
allowed:

```
select kind + namespace
        │
        ▼
  ResourceService.CanI   ──►  member API server (SelfSubjectAccessReview, as you)
        │                         allow / deny per verb
        ▼
  New  … shown only if you can create   (and the kind is authorable)
  Delete … shown only if you can delete
  Edit … opens the guided form (or YAML editor) if you can update, otherwise View (read-only, Save hidden)
```

- **Fail-closed.** Until the check resolves — or if it errors — the actions stay
  hidden.
- **Best-effort UX, not the boundary.** The gate you see is a courtesy so the
  console doesn't offer you a button that will bounce. The *enforcement* is the
  API server on the real call. A name-scoped Role (`resourceNames:`) that a
  blanket `CanI` can't fully model will still deny the actual write — that
  surfaces in the action banner, never as a silent no-op.
- **No extra gateway power.** `CanI` is a self-review; it reports only *your*
  effective access and grants nothing.

## Creating and editing resources

Most authorable kinds have a **guided form** — smart pickers, conditional
sections, sane defaults — and every form carries a **Form ⇄ YAML** toggle in its
header. Fill in fields, flip to YAML to hand-tune anything the form doesn't
model, flip back. YAML stays the source of truth for unmodelled fields, so the
toggle is lossless. Kinds without a bespoke form open the raw YAML editor
directly from **New**.

Guided forms today:

- **KubeSwift:** Guest Class, Guest Pool, Image, Kernel, Seed Profile, GPU
  Profile, Sandbox, Sandbox Pool, Snapshot Schedule.
- **Workloads:** Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob —
  a shared pod-template editor (containers, command/env, ports, probes, volumes,
  security context).
- **Networking / Config:** Service, Ingress, ConfigMap, Secret.
- **Access:** ServiceAccount, Role, ClusterRole, RoleBinding, ClusterRoleBinding.

**Edit** on a row opens the same form, pre-populated. The form *merges* your
changes onto the loaded object, so fields it doesn't model survive the round
trip. Both New and Edit submit through the same impersonated apply, so RBAC and
the admission webhooks gate the write — a denial lands in the form's banner.

**Snapshots and Restores are contextual**, not list-`New` actions — they operate
*on* an object, the same as Migrate:

- **Snapshot** — a button on a SwiftGuest's detail drawer.
- **Restore** — a button on a SwiftSnapshot's detail drawer.

## Secrets: create / rotate only

The gateway **redacts Secret values on read** (`data` and `stringData` are
stripped before anything leaves the hub — the "E4" rule). The console therefore
**never receives secret values** and cannot show, copy, or pre-fill them.

What that means in practice:

- The **Secret form writes via `stringData`** (write-only). Three shapes: an
  **Opaque** key/value set, a **registry** pull credential (builds a
  `.dockerconfigjson`), or a **TLS** cert/key pair. On apply the value is set on
  the cluster and is never echoed back to the browser.
- **Rotating** a secret = create/apply it again with the same name and new
  values. Server-side apply overwrites the keys you send.
- **Editing a secret's YAML** (metadata, labels, type) is safe: because the value
  fields were redacted out of what you loaded, applying the edited object leaves
  the existing values untouched — you won't accidentally blank them.
- **Consequence:** use the console to **create or rotate** secrets, not to
  inspect them. If you must read a value, `kubectl get secret … -o yaml` (with
  your own RBAC) is the path — deliberately outside the web console.

## Granting access (RBAC recipes)

Because impersonation makes your Kubernetes RBAC the permission model, you shape
what a user sees by binding Roles to them (or to their OIDC groups). A few
starting points — bind with a `RoleBinding` (namespaced) or `ClusterRoleBinding`
(fleet-wide):

**View-only in one namespace** (browse everything, no New/Edit/Delete):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: kubeswift-viewer, namespace: team-a }
rules:
  - apiGroups: ["", "apps", "batch", "networking.k8s.io", "swift.kubeswift.io", "sandbox.kubeswift.io", "image.kubeswift.io", "kernel.kubeswift.io", "seed.kubeswift.io", "gpu.kubeswift.io", "snapshot.kubeswift.io"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]
```

**Manage ConfigMaps + Services in one namespace** (New/Edit/Delete light up for
just those two):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: kubeswift-cm-svc-editor, namespace: team-a }
rules:
  - apiGroups: [""]
    resources: ["configmaps", "services"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

The predefined KubeSwift roles from the console's **Access** tab
([auth.md](auth.md#5-manage-roles--access-from-the-ui)) cover the common
VM-operator personas; the Explorer's gating reflects whatever is bound to the
user, native kinds included.

## Notes

- The catalog is server-owned (`internal/gateway/resource_catalog.go`); surfacing
  a new kind is a gateway change, not a per-cluster config.
- All CRUD is the impersonated user — there is no console service account acting
  on your behalf. If the console shows nothing for a kind, you likely lack
  `list` on it.
