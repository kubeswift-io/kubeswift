## ADDED Requirements

### Requirement: Top-level directories

The repository github.com/projectbeskar/kubeswift MUST include the following top-level directories: `api/`, `cmd/`, `internal/`, `config/`, `rust/`, and `docs/`.

#### Scenario: All directories present

- **WHEN** the repository is bootstrapped
- **THEN** the directories api/, cmd/, internal/, config/, rust/, and docs/ exist at the repository root

#### Scenario: Paths are repository-relative

- **WHEN** documentation or build scripts reference these paths
- **THEN** paths are relative to the repository root (e.g., api/swift/v1alpha1/, rust/swiftletd/)

### Requirement: Go API types under api/

API types for swift.kubeswift.io, image.kubeswift.io, and seed.kubeswift.io MUST reside under `api/` with structure suitable for controller-runtime scheme registration.

#### Scenario: API package layout

- **WHEN** API types are added
- **THEN** they reside under api/swift/v1alpha1/, api/image/v1alpha1/, and api/seed/v1alpha1/

#### Scenario: Scheme registration possible

- **WHEN** the Go module is built
- **THEN** api/ packages can be imported and registered with a runtime.Scheme for controller-runtime

### Requirement: Go binaries under cmd/

The Go binaries controller-manager, webhook-server, and swiftctl MUST have placeholder entrypoints under `cmd/`. Each binary MUST have its own subdirectory.

#### Scenario: Binary placeholders exist

- **WHEN** the repository is bootstrapped
- **THEN** cmd/controller-manager/, cmd/webhook-server/, and cmd/swiftctl/ exist with main package stubs

#### Scenario: Binaries build

- **WHEN** `go build ./cmd/...` is run
- **THEN** controller-manager, webhook-server, and swiftctl binaries are produced (or build without error for placeholders)

### Requirement: Internal Go packages under internal/

Non-API Go code (controllers, webhooks, resolved-spec model, shared utilities) MUST reside under `internal/`. Packages under internal/ MUST NOT be intended for external import.

#### Scenario: Internal layout

- **WHEN** control-plane logic is added
- **THEN** it resides under internal/ (e.g., internal/controller/, internal/webhook/, internal/resolved/)

#### Scenario: Go internal visibility

- **WHEN** packages under internal/ are defined
- **THEN** they follow Go's internal package visibility rules (not importable by external modules)

### Requirement: Config under config/

CRD manifests, RBAC, Kustomize overlays, and sample manifests MUST reside under `config/`.

#### Scenario: Config layout

- **WHEN** configuration is added
- **THEN** config/ contains subdirectories such as crd/, rbac/, default/, samples/

#### Scenario: Config is usable by Kustomize or kubectl

- **WHEN** config/ is populated
- **THEN** manifests can be applied via kubectl or kustomize build

### Requirement: Rust workspace under rust/

All Rust crates MUST reside under `rust/`. The Rust workspace MUST include the crates swiftletd, swift-runtime, swift-seed, and swift-ch-client.

#### Scenario: Rust workspace structure

- **WHEN** the repository is bootstrapped
- **THEN** rust/Cargo.toml defines a workspace with members swiftletd, swift-runtime, swift-seed, swift-ch-client

#### Scenario: swiftletd binary placeholder

- **WHEN** rust/swiftletd is built
- **THEN** it produces a swiftletd binary (or compiles successfully as a placeholder)

#### Scenario: Library crates are libraries

- **WHEN** rust/swift-runtime, rust/swift-seed, or rust/swift-ch-client are built
- **THEN** they produce library crates that can be depended on by swiftletd

### Requirement: Documentation under docs/

Sample documentation describing the repository layout MUST exist under `docs/`.

#### Scenario: Layout doc exists

- **WHEN** the repository is bootstrapped
- **THEN** docs/ contains at least one document (e.g., repo-layout.md) describing the directory structure and purpose of api/, cmd/, internal/, config/, rust/, docs/

#### Scenario: Doc references correct paths

- **WHEN** the layout documentation is read
- **THEN** it describes paths within github.com/projectbeskar/kubeswift and uses KubeSwift naming (controller-manager, swiftletd, swiftctl, etc.)

### Requirement: Module and workspace roots

The repository MUST include `go.mod` at the root with module path `github.com/projectbeskar/kubeswift`. The Rust workspace MUST be defined under `rust/` (rust/Cargo.toml) or at the root with workspace members under rust/.

#### Scenario: Go module valid

- **WHEN** `go mod tidy` or `go build ./...` is run at the repository root
- **THEN** the Go module resolves and builds without error (for placeholder code)

#### Scenario: Rust workspace valid

- **WHEN** `cargo build` is run in rust/ (or at root with workspace including rust/)
- **THEN** all workspace members build without error (for placeholder code)
