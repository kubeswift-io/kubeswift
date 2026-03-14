## 1. Repository setup

- [ ] 1.1 Create directory layout: `cmd/manager/`, `cmd/swiftletd/`, `pkg/apis/`, `pkg/controllers/`, `internal/resolved/`, `config/`, `docs/architecture/`
- [ ] 1.2 Add `go.mod` with module path `github.com/projectbeskar/kubeswift` and dependencies (controller-runtime, etc.)
- [ ] 1.3 Add root `Cargo.toml` workspace with `swiftletd` crate under `swiftletd/`
- [ ] 1.4 Add `.gitignore` for Go and Rust build artifacts

## 2. API types and CRDs

- [ ] 2.1 Add `pkg/apis/swift/v1alpha1/` with SwiftGuest and SwiftGuestClass types (spec, status, no controller logic)
- [ ] 2.2 Add `pkg/apis/image/v1alpha1/` with SwiftImage type (spec, status)
- [ ] 2.3 Add `pkg/apis/seed/v1alpha1/` with SwiftSeedProfile type (spec, status)
- [ ] 2.4 Add `config/crd/` with CRD manifests for SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile
- [ ] 2.5 Add `config/rbac/` with ServiceAccount, Role, RoleBinding for manager
- [ ] 2.6 Add `make generate` or equivalent to regenerate CRDs from Go types

## 3. Control plane manager scaffold

- [ ] 3.1 Add `cmd/manager/main.go` that initializes manager, scheme, and client (no reconciliation)
- [ ] 3.2 Register SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile in scheme
- [ ] 3.3 Add `Makefile` or `make` targets for `build`, `run`, `generate`, `install`
- [ ] 3.4 Add `config/default/` with manager deployment manifest (Kustomize)

## 4. Node runtime scaffold

- [ ] 4.1 Create Rust crate `swiftletd/` with `src/main.rs` that prints a placeholder message and exits
- [ ] 4.2 Add `Cargo.toml` for swiftletd with minimal dependencies
- [ ] 4.3 Add `make build-swiftletd` or `cargo build` to produce swiftletd binary

## 5. Architecture documentation

- [ ] 5.1 Add `docs/architecture/overview.md` describing control plane, node plane, and runtime handoff
- [ ] 5.2 Add `docs/architecture/language-split.md` documenting Go vs Rust usage and boundaries
- [ ] 5.3 Add `docs/architecture/api-groups.md` listing swift.kubeswift.io, image.kubeswift.io, seed.kubeswift.io and primary resources
- [ ] 5.4 Add `docs/architecture/runtime-model.md` describing one-guest-per-pod and swiftletd role
- [ ] 5.5 Add `docs/architecture/image-vs-initialization.md` documenting separation of SwiftImage and SwiftSeedProfile

## 6. Sample manifests

- [ ] 6.1 Add `config/samples/` with example SwiftImage, SwiftGuestClass, SwiftSeedProfile, and SwiftGuest YAML
- [ ] 6.2 Add `config/samples/README.md` with brief usage instructions
