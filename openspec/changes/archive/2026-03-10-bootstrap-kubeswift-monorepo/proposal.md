## Why

KubeSwift needs a concrete monorepo layout before implementation can proceed. The architecture foundation (Go control plane, Rust node runtime) is established, but the repository currently lacks the directory structure, module boundaries, and binary placeholders required for development. This change bootstraps the repository with a layout that separates control-plane code from node-runtime code, establishes clear module boundaries for controller-runtime and multiple Rust crates, and provides placeholders for all planned binaries so that subsequent work can land in the correct locations.

## What Changes

- Create monorepo layout with `api/`, `cmd/`, `internal/`, `config/`, `rust/`, and `docs/` directories.
- Establish Go module boundaries suitable for controller-runtime (API types, scheme registration, manager setup).
- Establish Rust workspace under `rust/` with multiple crates: `swiftletd`, `swift-runtime`, `swift-seed`, `swift-ch-client`.
- Add placeholder binaries: controller-manager, webhook-server, swiftctl (Go); swiftletd (Rust).
- Add sample docs describing the repo layout.
- Add `go.mod`, `Cargo.toml` workspace, and minimal build scaffolding.

**Intentionally excluded:**

- Full controller implementation
- Full runtime implementation
- Working Cloud Hypervisor launch
- Full CI

## Capabilities

### New Capabilities

- `monorepo-layout`: Repository structure requirements for github.com/projectbeskar/kubeswift, including directory layout (api/, cmd/, internal/, config/, rust/, docs/), Go module boundaries for controller-runtime, Rust workspace with swiftletd, swift-runtime, swift-seed, swift-ch-client crates, and placeholder binaries (controller-manager, webhook-server, swiftctl, swiftletd).

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: `api/`, `cmd/`, `internal/`, `config/`, `rust/`, `docs/`
- **Binaries**: controller-manager, webhook-server, swiftctl (Go); swiftletd (Rust)
- **Rust crates**: swiftletd, swift-runtime, swift-seed, swift-ch-client under `rust/`
- **Risks**: Layout decisions affect future refactors; keeping placeholders minimal limits lock-in.
- **Dependencies**: Go toolchain, Rust toolchain
- **Rollback**: Remove added directories and files; no runtime impact.
