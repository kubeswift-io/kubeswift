## Why

The repository contains controllers, webhooks, Rust crates, CRDs, RBAC, sample resources, and a smoke test script, but lacks a complete, reproducible minimal deployment path for the controller-manager and swiftletd runtime. Without it, developers cannot reliably install KubeSwift into a cluster to run the first-boot smoke test. Minimal deployment packaging must come before webhook enablement because smoke testing should not depend on webhook TLS and admission plumbing in the first step—that adds unnecessary complexity and failure modes before the core boot path is validated.

## What Changes

- Add or complete container build definitions for controller-manager and swiftletd
- Add Kubernetes manifests: namespace (`kubeswift-system`), service accounts, controller-manager Deployment, swiftletd DaemonSet
- Add a top-level install kustomization that composes the above
- Add Makefile targets: `build-images`, `deploy`, `undeploy`, `load-images` (for local clusters)
- Document the minimal install and smoke-test preparation flow in `docs/deploy.md`

**Out of scope (explicitly excluded):**

- ValidatingWebhookConfiguration, MutatingWebhookConfiguration
- Webhook TLS / cert-manager integration
- Release publishing
- Migration, snapshots, or new VM runtime features

## Capabilities

### New Capabilities

- `minimal-kubeswift-cluster-deployment`: Defines the minimal cluster install path—container images, manifests, kustomizations, Makefile targets, and documentation—sufficient to support the existing boot smoke test without admission webhooks.

### Modified Capabilities

- None. The first-boot-cluster-smoke-verification spec's prerequisites (CRDs, controllers, swiftletd image, RBAC) remain unchanged; this change provides the deployment path to satisfy them.

## Impact

- **Paths:** `config/namespace/`, `config/manager/`, `config/daemonset/`, `config/default/`, `images/controller-manager/`, `images/swiftletd/`, `Makefile`, `docs/deploy.md`
- **Binaries:** `cmd/controller-manager`, `rust/swiftletd`
- **Dependencies:** Existing CRDs (`config/crd/`), RBAC for swiftletd (`config/rbac/`), samples (`config/samples/`)
- **Risks:** None. Adds new artifacts; does not modify controller or swiftletd code.
- **Rollback:** Remove manifests and Makefile targets; cluster state can be cleaned with `make undeploy` or equivalent.
