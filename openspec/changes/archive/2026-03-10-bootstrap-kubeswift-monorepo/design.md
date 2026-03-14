## Context

KubeSwift uses a Go control plane (controller-runtime, Kubebuilder-style) and a Rust node runtime (swiftletd, Cloud Hypervisor integration). The repository currently has no implementation structure. This change bootstraps a monorepo layout that accommodates both ecosystems, establishes clear module boundaries, and provides placeholders for all planned binaries so that subsequent work lands in the correct locations.

**Constraints:** Single repository at github.com/projectbeskar/kubeswift; Go for control plane, Rust for node runtime; no libvirt; Cloud Hypervisor as sole VMM.

## Goals / Non-Goals

**Goals:**

- Create directory layout: `api/`, `cmd/`, `internal/`, `config/`, `rust/`, `docs/`
- Establish Go module boundaries suitable for controller-runtime
- Establish Rust workspace with multiple crates under `rust/`
- Add placeholder binaries: controller-manager, webhook-server, swiftctl (Go); swiftletd (Rust)
- Add sample docs describing the repo layout
- Explain rationale for the split

**Non-Goals:**

- Full controller or runtime implementation
- Working Cloud Hypervisor launch
- Full CI

## Decisions

### 1. Why `api/` instead of `pkg/apis/`

**Rationale:** `api/` is a common Kubernetes-project convention for API types and scheme definitions. It keeps API types at the top level, distinct from internal packages. Kubebuilder and controller-runtime projects often use `api/` or `apis/`; we use `api/` for brevity and to avoid confusion with `internal/`.

**Alternatives considered:** `pkg/apis/` (more nested; used in architecture-foundation design); `apis/` (similar). Chosen: `api/` to match user-specified layout and keep API surface prominent.

### 2. Why `cmd/` for binaries

**Rationale:** `cmd/` is the standard Go layout for executable entrypoints. Each subdirectory is a binary: `cmd/controller-manager/`, `cmd/webhook-server/`, `cmd/swiftctl/`. This separates "what runs" from "what is imported."

### 3. Why `internal/` for non-API Go code

**Rationale:** Go's `internal/` package visibility prevents external imports. Controllers, reconciliation logic, resolved-spec model, and shared utilities belong here. This enforces that the public API surface is `api/`; everything else is implementation detail.

### 4. Why `rust/` as a single parent for all Rust code

**Rationale:** Isolating Rust in `rust/` keeps the Go/Rust boundary explicit. The root `go.mod` and root `Cargo.toml` (or `rust/Cargo.toml` as workspace root) live in predictable places. Build scripts and CI can target `rust/` for Rust builds without scanning the whole tree. Multiple crates (`swiftletd`, `swift-runtime`, `swift-seed`, `swift-ch-client`) share the workspace and can depend on each other.

**Alternatives considered:** Rust crates at repo root (e.g., `swiftletd/`, `swift-runtime/`) mixed with Go dirs—rejected for clarity; single `rust/` parent is easier to reason about.

### 5. Why separate controller-manager and webhook-server

**Rationale:** controller-runtime typically runs controllers and webhooks in the same process, but some deployments prefer separate processes for scaling or security isolation. Placeholders for both allow either pattern; the design does not commit to a single binary yet.

### 6. Why swiftctl as a separate binary

**Rationale:** `swiftctl` is the operator/CLI tool (analogous to `kubectl` for Kubernetes). It is a Go binary that talks to the cluster; it does not run on nodes. Separate from controller-manager and webhook-server.

### 7. Rust crate split: swiftletd, swift-runtime, swift-seed, swift-ch-client

**Rationale:**

- **swiftletd**: Node daemon; orchestrates VM lifecycle, talks to Cloud Hypervisor. Binary crate.
- **swift-runtime**: Low-level VM runtime helpers (disk setup, device handling). Library crate used by swiftletd.
- **swift-seed**: Seed media generation (NoCloud, ConfigDrive). Library crate; used by control plane (Go) or node (Rust) depending on where generation runs—placeholder allows either.
- **swift-ch-client**: Cloud Hypervisor API client. Library crate; encapsulates Unix socket communication. Keeps CH-specific code isolated.

This split keeps concerns separate: CH client, seed generation, runtime helpers, and the daemon that ties them together.

## Repository structure

```
github.com/projectbeskar/kubeswift/
├── api/                          # Go API types (swift, image, seed)
│   ├── swift/v1alpha1/
│   ├── image/v1alpha1/
│   └── seed/v1alpha1/
├── cmd/
│   ├── controller-manager/       # Go: controllers, reconciliation
│   ├── webhook-server/           # Go: admission/mutation webhooks
│   └── swiftctl/                 # Go: CLI for operators
├── internal/                     # Go: non-API, not importable externally
│   ├── controller/
│   ├── webhook/
│   └── resolved/
├── config/                       # CRDs, RBAC, Kustomize, samples
│   ├── crd/
│   ├── rbac/
│   ├── default/
│   └── samples/
├── rust/                         # Rust workspace
│   ├── Cargo.toml                # Workspace root
│   ├── swiftletd/                # Binary: node daemon
│   ├── swift-runtime/            # Library: VM runtime helpers
│   ├── swift-seed/               # Library: seed media generation
│   └── swift-ch-client/          # Library: Cloud Hypervisor API client
├── docs/
│   └── repo-layout.md            # Sample doc describing layout
├── go.mod
├── go.sum
└── Makefile                      # build, generate, run targets
```

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Rust workspace at `rust/` may require path adjustments in CI | Document `cd rust && cargo build`; Makefile targets for Rust builds |
| controller-manager vs webhook-server: one or two binaries? | Placeholders for both; implementation can merge later if desired |
| swift-seed in Rust vs Go | Placeholder in Rust; generation could move to Go if preferred |
| api/ vs pkg/apis/ inconsistency with prior design | This change adopts api/ per user spec; update architecture docs to match |

## Migration Plan

1. Create directories and placeholder files
2. Add go.mod, Cargo.toml workspace, Makefile
3. Add docs/repo-layout.md
4. **Rollback:** Remove added content; no runtime impact

## Open Questions

- Whether webhook-server will be a separate binary or merged into controller-manager (placeholder supports both)
- Whether swift-seed will be implemented in Rust or Go (Rust placeholder for now)
