## Context

KubeSwift has controllers (`cmd/controller-manager`), CRDs (`config/crd/`), RBAC for swiftletd (`config/rbac/`), samples (`config/samples/`), and a smoke test (`test/smoke/boot-test.sh`). The controller-manager runs SwiftImage and SwiftGuest controllers and webhooks in-process. Swiftletd runs in SwiftGuest pods as the launcher container and may also run as a DaemonSet on nodes. The repository may already have partial deployment manifests; this design defines the minimal, webhook-free path that satisfies the first-boot smoke test prerequisites.

## Goals / Non-Goals

**Goals:**

- Provide container build definitions for controller-manager and swiftletd
- Provide Kubernetes manifests: namespace, service accounts, controller-manager Deployment, swiftletd DaemonSet
- Provide a top-level install kustomization that composes the above
- Provide Makefile targets: `build-images`, `deploy`, `undeploy`, `load-images`
- Document the minimal install and smoke-test preparation flow

**Non-Goals:**

- ValidatingWebhookConfiguration, MutatingWebhookConfiguration
- Webhook TLS / cert-manager integration
- Release publishing
- Migration, snapshots, or new VM runtime features

## Decisions

### 1. Minimal install excludes webhook deployment

**Decision:** The minimal install kustomization SHALL include only: namespace, manager (controller-manager Deployment + RBAC + ServiceAccounts), daemonset (swiftletd DaemonSet). It SHALL NOT include a separate webhook Deployment or Service.

**Rationale:** Webhooks run in-process in the controller-manager. For minimal deploy we do not register them with the API server (no ValidatingWebhookConfiguration/MutatingWebhookConfiguration). The controller-manager still runs and serves webhooks internally; the API server simply does not call them. Create/update of SwiftGuest, SwiftImage, SwiftSeedProfile succeeds without admission; the controller reconciles normally.

**Alternative considered:** Include webhook Deployment. Rejected—adds TLS/cert complexity before first boot is validated.

### 2. Image build definitions

**Decision:** Use `images/controller-manager/Containerfile` (multi-stage Go build from `cmd/controller-manager`) and `images/swiftletd/Containerfile` (existing Rust + Cloud Hypervisor). Image names: `ghcr.io/projectbeskar/kubeswift/controller-manager:latest`, `ghcr.io/projectbeskar/kubeswift/swiftletd:latest` (per `internal/controller/swiftguest/constants.go` LauncherImage).

**Rationale:** Aligns with existing codebase; swiftletd image is referenced when the controller creates SwiftGuest pods.

### 3. Config layout

**Decision:** Use `config/namespace/`, `config/manager/`, `config/daemonset/`, `config/default/`. `config/default` composes namespace, manager, daemonset. Do NOT include `config/webhook` in the minimal install.

**Rationale:** Matches the user-specified layout; keeps minimal install simple.

### 4. Makefile targets

**Decision:** `build-images` SHALL build controller-manager and swiftletd only (no webhook-server for minimal). `deploy` SHALL apply CRDs first, then `kubectl apply -k config/default`. `undeploy` SHALL delete config/default, then CRDs. `load-images` SHALL load controller-manager and swiftletd into kind/minikube.

**Rationale:** Minimal path; webhook image is unnecessary for first boot.

### 5. Swiftletd DaemonSet

**Decision:** Include a swiftletd DaemonSet in the minimal install. It runs swiftletd on each node with privileged access and host paths for `/var/lib/kubeswift` and `/dev/kvm`. The SwiftGuest controller also creates pods with swiftletd as the launcher; both deployment models are supported.

**Rationale:** User scope includes swiftletd DaemonSet; supports node-level runtime for alternative deployment models.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| DaemonSet pods may crash without intent | Swiftletd expects intent file; DaemonSet provides structure for future work. SwiftGuest pods use intent from ConfigMap. |
| No admission validation/defaulting | Acceptable for first boot; controller handles reconciliation. Webhooks can be added later. |
| Image pull failures in air-gapped clusters | Document `load-images` for kind/minikube; operators must push to accessible registry for production. |

## Migration Plan

1. Add or complete `images/controller-manager/Containerfile`, `images/swiftletd/Containerfile`
2. Add or complete `config/namespace/`, `config/manager/`, `config/daemonset/`
3. Ensure `config/default/kustomization.yaml` composes namespace, manager, daemonset (exclude webhook for minimal)
4. Add or update Makefile targets
5. Add or update `docs/deploy.md` with minimal install and smoke-test preparation
6. **Rollback:** `make undeploy`; remove or revert added files

## Open Questions

- None. Scope is well-defined.
