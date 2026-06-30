# Repository Layout

github.com/kubeswift-io/kubeswift

## Top-level directories

| Path | Purpose |
|------|---------|
| `api/` | CRD types (swift, image, seed, kernel) |
| `cmd/` | controller-manager, swiftctl |
| `internal/` | Controllers, webhooks, seed, runtimeintent, resolved |
| `config/` | CRDs, RBAC, Kustomize, samples |
| `charts/` | Helm chart (OCI) |
| `images/` | Containerfiles (controller-manager, swiftletd) |
| `rust/` | swiftletd, swift-runtime, swift-seed, swift-ch-client |
| `build/` | Kernel profiles (faas-minimal) |
| `hack/` | version.sh, chart-version.sh |
| `test/` | Smoke tests |

## Key paths for developers

| What | Where |
|------|-------|
| SwiftGuest controller | `internal/controller/swiftguest/` |
| SwiftImage controller | `internal/controller/swiftimage/` |
| SwiftKernel controller | `internal/controller/swiftkernel/` |
| Pod creation | `internal/controller/swiftguest/pod.go` |
| Launcher image constant | `internal/controller/swiftguest/constants.go` |
| Seed rendering | `internal/seed/` |
| Runtime intent | `internal/runtimeintent/` |
| Resolution (resolver) | `internal/resolved/` |
| swiftletd entrypoint | `rust/swiftletd/` |
| faas-minimal kernel profile | `build/kernels/faas-minimal/` |

## Config layout

| Path | Purpose |
|------|---------|
| `config/crd/` | CRD manifests |
| `config/default/` | Install entrypoint (namespace, manager, daemonset) |
| `config/rbac/` | swiftletd Role/RoleBinding (apply per namespace) |
| `config/samples/` | Sample manifests |
| `config/overlays/webhook/` | Webhook overlay (cert-manager) |

## Rust crates

| Crate | Purpose |
|-------|---------|
| swiftletd | Launcher; reads intent, builds seed, runs Cloud Hypervisor |
| swift-runtime | Runtime dir setup |
| swift-seed | NoCloud media generation |
| swift-ch-client | Cloud Hypervisor API (Unix socket) |

[Deploy](../install/local-cluster.md) · [Architecture](../architecture.md)
