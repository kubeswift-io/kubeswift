# KubeSwift Repository Layout

Repository: github.com/projectbeskar/kubeswift

## Directories

| Path | Purpose |
|------|---------|
| api/ | Go API types (swift, image, seed) |
| cmd/ | Go binaries: controller-manager, webhook-server, swiftctl |
| internal/ | Go internal packages (controller, webhook, resolved) |
| config/ | CRDs, RBAC, Kustomize, samples |
| rust/ | Rust workspace (swiftletd, swift-runtime, swift-seed, swift-ch-client) |
| docs/ | Documentation |

## Binaries

| Binary | Path | Purpose |
|--------|------|---------|
| controller-manager | cmd/controller-manager/ | Controllers, reconciliation |
| webhook-server | cmd/webhook-server/ | Admission/mutation webhooks |
| swiftctl | cmd/swiftctl/ | CLI for operators |
| swiftletd | rust/swiftletd/ | Node daemon; launches Cloud Hypervisor |

## Rust crates

| Crate | Path | Purpose |
|-------|------|---------|
| swiftletd | rust/swiftletd/ | Node daemon; orchestrates VM lifecycle |
| swift-runtime | rust/swift-runtime/ | Per-guest runtime directory setup |
| swift-seed | rust/swift-seed/ | NoCloud seed media generation |
| swift-ch-client | rust/swift-ch-client/ | Cloud Hypervisor API client (Unix socket) |
