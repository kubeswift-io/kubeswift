## 1. Workflow scaffolding

- [x] 1.1 Create `.github/workflows/ci.yaml` with triggers for push and pull_request to main
- [x] 1.2 Add path filters to skip CI when only docs or unrelated files change

## 2. Go job

- [x] 2.1 Add `go` job: setup Go, run `go fmt ./...`, `go build ./cmd/...`, `go test ./...`
- [x] 2.2 Use Go version from go.mod or pin to 1.25

## 3. Rust job

- [x] 3.1 Add `rust` job: setup Rust, run `cargo fmt --check`, `cargo build`, `cargo test`
- [x] 3.2 Use stable Rust or version from rust-toolchain.toml if present

## 4. Generate job

- [x] 4.1 Add `generate` job: run `make generate`, then `git diff --exit-code` on config/crd/bases and zz_generated files
- [x] 4.2 Fail job if diff is non-empty

## 5. swiftletd image job (optional)

- [x] 5.1 Add `swiftletd-image` job to build images/swiftletd Containerfile
- [x] 5.2 Make job optional or allow failure if Docker unavailable
