## ADDED Requirements

### Requirement: Go build and test validation

The CI pipeline MUST run Go fmt, build, and test. It MUST verify that controller-manager, webhook-server, and swiftctl build successfully.

#### Scenario: Go fmt passes

- **WHEN** CI runs the Go job
- **THEN** `go fmt` (or equivalent check) passes and no formatting changes are required

#### Scenario: Go build succeeds

- **WHEN** CI runs the Go job
- **THEN** `go build ./cmd/...` succeeds

#### Scenario: Go tests pass

- **WHEN** CI runs the Go job
- **THEN** `go test` (or equivalent) passes for the tested packages

### Requirement: Rust build and test validation

The CI pipeline MUST run Rust fmt check, build, and test. It MUST verify that swiftletd builds successfully.

#### Scenario: Rust fmt check passes

- **WHEN** CI runs the Rust job
- **THEN** `cargo fmt --check` passes

#### Scenario: Rust build succeeds

- **WHEN** CI runs the Rust job
- **THEN** `cargo build` (or `cargo build -p swiftletd`) succeeds

#### Scenario: Rust tests pass

- **WHEN** CI runs the Rust job
- **THEN** `cargo test` passes for the Rust workspace

### Requirement: Code generation verification

The CI pipeline MUST verify that generated code and CRDs are up to date. It MUST run `make generate` and fail if the generated output differs from the committed files.

#### Scenario: Generated code up to date

- **WHEN** CI runs the generate job
- **THEN** `make generate` produces no changes to `config/crd/bases/` or `zz_generated*` files

#### Scenario: Generated code outdated

- **WHEN** API types changed but `make generate` was not run
- **THEN** the generate job fails with a diff

### Requirement: Core binary build validation

The CI pipeline MUST verify that controller-manager, webhook-server, swiftctl, and swiftletd can be built.

#### Scenario: Go binaries build

- **WHEN** CI runs the Go job
- **THEN** controller-manager, webhook-server, and swiftctl build successfully

#### Scenario: swiftletd builds

- **WHEN** CI runs the Rust job
- **THEN** swiftletd builds successfully

### Requirement: Container image build validation (optional)

The CI pipeline MAY validate that the swiftletd container image builds. If implemented, it MUST use the Containerfile at `images/swiftletd/Containerfile` and build context `rust/`.

#### Scenario: swiftletd image builds

- **WHEN** CI runs the image build job (if present)
- **THEN** `docker build -f images/swiftletd/Containerfile rust/` succeeds
