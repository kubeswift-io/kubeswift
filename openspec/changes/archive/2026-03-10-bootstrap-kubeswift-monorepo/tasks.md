## 1. Directory structure

- [ ] 1.1 Create top-level directories: api/, cmd/, internal/, config/, rust/, docs/
- [ ] 1.2 Create api/swift/v1alpha1/, api/image/v1alpha1/, api/seed/v1alpha1/
- [ ] 1.3 Create cmd/controller-manager/, cmd/webhook-server/, cmd/swiftctl/
- [ ] 1.4 Create internal/controller/, internal/webhook/, internal/resolved/
- [ ] 1.5 Create config/crd/, config/rbac/, config/default/, config/samples/
- [ ] 1.6 Create rust/swiftletd/, rust/swift-runtime/, rust/swift-seed/, rust/swift-ch-client/

## 2. Go module

- [ ] 2.1 Add go.mod with module path github.com/projectbeskar/kubeswift
- [ ] 2.2 Add controller-runtime and related dependencies to go.mod
- [ ] 2.3 Add placeholder main.go in cmd/controller-manager/ that exits successfully
- [ ] 2.4 Add placeholder main.go in cmd/webhook-server/ that exits successfully
- [ ] 2.5 Add placeholder main.go in cmd/swiftctl/ that exits successfully
- [ ] 2.6 Add minimal api package stubs (e.g., api/swift/v1alpha1/doc.go) so api/ imports resolve
- [ ] 2.7 Verify `go build ./...` succeeds

## 3. Rust workspace

- [ ] 3.1 Add rust/Cargo.toml as workspace root with members swiftletd, swift-runtime, swift-seed, swift-ch-client
- [ ] 3.2 Add rust/swiftletd/Cargo.toml (binary) with src/main.rs placeholder
- [ ] 3.3 Add rust/swift-runtime/Cargo.toml (lib) with src/lib.rs placeholder
- [ ] 3.4 Add rust/swift-seed/Cargo.toml (lib) with src/lib.rs placeholder
- [ ] 3.5 Add rust/swift-ch-client/Cargo.toml (lib) with src/lib.rs placeholder
- [ ] 3.6 Add swiftletd placeholder that prints a message and exits
- [ ] 3.7 Verify `cargo build` in rust/ succeeds

## 4. Build tooling

- [ ] 4.1 Add Makefile with target `build-go` for `go build ./cmd/...`
- [ ] 4.2 Add Makefile target `build-rust` for `cd rust && cargo build`
- [ ] 4.3 Add Makefile target `build` that runs both build-go and build-rust
- [ ] 4.4 Add .gitignore for Go and Rust build artifacts

## 5. Documentation

- [ ] 5.1 Add docs/repo-layout.md describing api/, cmd/, internal/, config/, rust/, docs/
- [ ] 5.2 Document each binary: controller-manager, webhook-server, swiftctl, swiftletd
- [ ] 5.3 Document each Rust crate: swiftletd, swift-runtime, swift-seed, swift-ch-client
- [ ] 5.4 Include repository-relative path examples for github.com/projectbeskar/kubeswift
