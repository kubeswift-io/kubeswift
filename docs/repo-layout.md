# KubeSwift Repository Layout

Repository: github.com/kubeswift-io/kubeswift

## Directories

| Path | Purpose |
|------|---------|
| api/ | Go API types (swift, image, seed) |
| hack/ | Version scripts (version.sh, chart-version.sh) |
| cmd/ | Go binaries: controller-manager, swiftctl |
| internal/ | Go internal packages (controller, webhook, resolved) |
| config/ | CRDs, RBAC, Kustomize, samples, deploy manifests |
| charts/ | Helm chart for OCI install (`oci://ghcr.io/kubeswift-io/charts/kubeswift`) |
| images/ | Container build definitions (controller-manager, swiftletd) |
| rust/ | Rust workspace (swiftletd, swift-runtime, swift-seed, swift-ch-client) |
| docs/ | Documentation |

## Binaries

| Binary | Path | Purpose |
|--------|------|---------|
| controller-manager | cmd/controller-manager/ | Controllers, reconciliation; serves admission webhooks when `--webhook-enabled=true` |
| swiftctl | cmd/swiftctl/ | CLI for operators |
| swiftletd | rust/swiftletd/ | Node daemon; launches Cloud Hypervisor |

## Config layout

| Path | Purpose |
|------|---------|
| config/namespace/ | kubeswift-system namespace |
| config/manager/ | controller-manager Deployment, RBAC, ServiceAccounts, webhook Service |
| config/webhook/ | Certificate, Issuer, ValidatingWebhookConfiguration, MutatingWebhookConfiguration (webhooks served by controller-manager) |
| config/daemonset/ | swiftletd DaemonSet |
| config/default/ | Install entrypoint; composes namespace, manager, daemonset |

See [deploy.md](deploy.md) for deployment instructions.

## Rust crates

| Crate | Path | Purpose |
|-------|------|---------|
| swiftletd | rust/swiftletd/ | Node daemon; orchestrates VM lifecycle |
| swift-runtime | rust/swift-runtime/ | Per-guest runtime directory setup |
| swift-seed | rust/swift-seed/ | NoCloud seed media generation |
| swift-ch-client | rust/swift-ch-client/ | Cloud Hypervisor API client (Unix socket) |
