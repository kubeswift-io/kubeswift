# Developer Getting Started

Quick start for contributors.

## Prerequisites

| Tool | Purpose |
|------|---------|
| Go 1.21+ | controller-manager, swiftctl |
| Rust (stable) | swiftletd, swift-runtime, swift-seed, swift-ch-client |
| Docker | Building images |
| kubectl | Deploy, test |
| kind or minikube | Local cluster (1.28+) |
| Helm 3+ | Chart packaging (optional) |

## Clone and build

```bash
git clone https://github.com/projectbeskar/kubeswift.git
cd kubeswift
make build
```

Builds Go (`cmd/controller-manager`, `cmd/swiftctl`) and Rust (`rust/swiftletd`, ...).

## Deploy locally

```bash
make build-images
make load-images   # loads into kind/minikube
make deploy
```

[Local cluster install](../install/local-cluster.md)

## Run smoke test

```bash
make smoke-test
```

Requires KubeSwift deployed and worker nodes with KVM. Run [preflight](../operator/worker-node-preflight.md) first.

## Navigate the repo

| Path | Contents |
|------|----------|
| `cmd/` | Go entrypoints (controller-manager, swiftctl) |
| `internal/` | Controllers, webhooks, seed, runtimeintent |
| `api/` | CRD types (swift, image, seed) |
| `rust/` | swiftletd and Cloud Hypervisor integration |
| `config/` | CRDs, RBAC, samples, Kustomize |
| `charts/` | Helm chart |

[Repo layout](repo-layout.md) · [Build](build.md) · [Testing](testing.md)
