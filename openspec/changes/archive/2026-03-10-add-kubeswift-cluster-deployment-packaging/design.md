## Context

The repository has `cmd/controller-manager` (controllers + webhooks in one process), `cmd/webhook-server` (stub, unused), `images/swiftletd/Containerfile`, CRDs (`config/crd/`), RBAC for swiftletd (`config/rbac/`), and samples (`config/samples/`). There is no controller-manager image build, no Deployment/DaemonSet manifests, and no kustomization for install. The Makefile builds binaries and swiftletd image but does not deploy. The smoke test consumes a deployed cluster; it is out of scope for this change except as the deployment path consumer.

## Goals / Non-Goals

**Goals:**

- Make the repo installable into a cluster
- Add all missing image build definitions and manifests
- Define exact repository paths for each artifact
- Add Makefile targets: build-images, deploy, undeploy, load-images
- Keep design consistent with existing code (controller-manager runs webhooks in-process; no separate webhook server)

**Non-Goals:**

- Smoke test changes (smoke test is consumer only)
- Release publishing, Helm, OLM
- Migration, snapshots, new runtime features
- CRD semantic changes

## Exact Repository Paths

| Artifact | Path | Purpose |
|----------|------|---------|
| Controller-manager image | `images/controller-manager/Containerfile` | Multi-stage build: Go binary from `cmd/controller-manager`, minimal runtime |
| Swiftletd image | `images/swiftletd/Containerfile` | Existing; no changes |
| Namespace | `config/deploy/base/namespace.yaml` | `kubeswift-system` |
| Service accounts | `config/deploy/base/serviceaccount.yaml` | controller-manager SA, swiftletd SA (if needed for DaemonSet) |
| Controller-manager Deployment | `config/deploy/base/controller-manager.yaml` | Deployment for `cmd/controller-manager` (controllers + webhooks) |
| Controller-manager RBAC | `config/deploy/base/controller-manager-rbac.yaml` | ClusterRole, ClusterRoleBinding for CRD reconciliation |
| Install kustomization | `config/deploy/base/kustomization.yaml` | Composes resources, sets images |

**Note:** swiftletd runs as the launcher container in each SwiftGuest Pod that the controller creates (`internal/controller/swiftguest/pod.go`). There is no swiftletd DaemonSet. The swiftletd image must be built and available; the controller references it via `LauncherImage` when creating Pods.
| Optional overlay | `config/deploy/default/kustomization.yaml` | Namespace/image overrides if needed |

**Image names:** `ghcr.io/projectbeskar/kubeswift/controller-manager:latest`, `ghcr.io/projectbeskar/kubeswift/swiftletd:latest` (per `internal/controller/swiftguest/constants.go` LauncherImage).

## Decisions

### No Separate Webhook Image

**Decision:** Do not add `images/webhook-server/Containerfile`. `cmd/webhook-server` is a stub. Webhooks run in-process in `cmd/controller-manager`. A single controller-manager image serves both controllers and webhooks.

**Rationale:** Matches `cmd/controller-manager/main.go`; avoids unused artifacts.

### Install Path

**Decision:** `kubectl apply -k config/deploy/base`. Apply CRDs first (`config/crd/`), then deploy base.

**Rationale:** Single base kustomization; overlays optional for env-specific patches.

### Makefile Targets

| Target | Purpose |
|--------|---------|
| `build-images` | Build controller-manager and swiftletd images |
| `deploy` | Apply CRDs, then `kubectl apply -k config/deploy/base` |
| `undeploy` | `kubectl delete -k config/deploy/base`, then delete CRDs |
| `load-images` | Load built images into local cluster (kind/minikube) for dev |

**Rationale:** Aligns with user requirements; `load-images` supports local dev without a registry.

### Webhook TLS

**Decision:** controller-runtime serves webhooks in-process. Webhooks require TLS. Use controller-runtime's built-in self-signed cert generation (cert dir, service name) or document cert-manager if needed. Defer full automation if it blocks initial deploy.

**Rationale:** controller-runtime supports this; cert-manager is optional for production.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Webhook cert blocks deploy | Use controller-runtime self-signed; document cert-manager for production |
| SwiftGuest pod needs /dev/kvm | Pod spec does not yet add /dev/kvm; document node requirements |
| Undeploy order | Remove deploy resources first, then CRDs |
