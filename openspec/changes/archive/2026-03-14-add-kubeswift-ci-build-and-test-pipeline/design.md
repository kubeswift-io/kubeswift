## Context

KubeSwift has no CI today. The Makefile provides `build`, `build-go`, `build-rust`, `generate`, and `smoke-test`. Go binaries: `cmd/controller-manager`, `cmd/webhook-server`, `cmd/swiftctl`. Rust: `rust/swiftletd` and crates. CRDs are generated via `make generate` into `config/crd/bases/`. The first-boot smoke verification (add-first-boot-cluster-smoke-verification) assumes a working build; CI must validate that baseline before cluster tests.

## Goals / Non-Goals

**Goals:**

- Add GitHub Actions workflow(s) for Go and Rust build/test validation
- Verify generated code and CRDs are up to date
- Verify controller-manager, webhook-server, swiftctl, swiftletd build
- Basic quality gates suitable for pull requests

**Non-Goals:**

- Versioned releases, publishing official images, signing, changelog automation, multi-arch release publishing

## Decisions

### 1. Single workflow file

**Decision:** Use one workflow file `.github/workflows/ci.yaml` (or `build-test.yaml`) with multiple jobs.

**Rationale:** Keeps CI simple; one file to maintain. Alternative: separate workflows per language—rejected for MVP.

### 2. Job structure

**Decision:** Jobs: `go`, `rust`, `generate`, optionally `swiftletd-image`. Jobs run in parallel where possible.

**Paths:**
- Go: `./cmd/...`, `./api/...`, `./internal/...`
- Rust: `rust/` (workspace root)
- Generate: `make generate`; diff `config/crd/bases/` and `zz_generated.*`
- swiftletd image: `docker build -f images/swiftletd/Containerfile rust/`

### 3. Go job

**Decision:** `go fmt`, `go build ./cmd/...`, `go test ./...` (or `./internal/...` if full repo test is heavy). Use `actions/setup-go` with Go version from `go.mod` or fixed 1.25.

**Rationale:** Matches `make build-go`; fmt catches style drift; test catches regressions.

### 4. Rust job

**Decision:** `cargo fmt --check`, `cargo build`, `cargo test`. Use `actions-rs/toolchain` or `dtolnay/rust-toolchain` with version from `rust-toolchain.toml` if present, else stable.

**Rationale:** Matches `make build-rust`; fmt --check is non-modifying.

### 5. Generate job

**Decision:** Run `make generate`, then `git diff --exit-code` on `config/crd/bases/` and any `zz_generated*` files. Fail if diff is non-empty.

**Rationale:** Ensures PRs include generated code when API types change.

### 6. swiftletd image job (optional)

**Decision:** Add job to build swiftletd container image. Use `docker build` or `docker/build-push-action` with `push: false`. Mark as optional or allow failure if Docker-in-Docker is unavailable.

**Rationale:** Validates Containerfile; may be skipped on some runners.

### 7. Triggers

**Decision:** On `push` to main and `pull_request` to main. Path filters: `**/*.go`, `**/Cargo.toml`, `**/api/**`, `**/config/crd/**`, `**/images/**` to avoid unnecessary runs.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Docker not available in runner | Make swiftletd-image job optional or use `docker/build-push-action` with `no-cache` |
| Go/Rust version drift | Pin versions in workflow; add `go.mod` / `rust-toolchain.toml` if needed |
| Long test times | Limit to `./internal/...` or add timeout |
