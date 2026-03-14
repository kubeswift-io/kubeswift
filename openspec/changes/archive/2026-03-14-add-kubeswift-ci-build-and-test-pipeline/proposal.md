## Why

Developers need a CI pipeline to validate that Go and Rust code builds, tests pass, and generated artifacts (CRDs, deepcopy) are up to date before PRs are merged. Without CI, the first-boot cluster smoke verification (add-first-boot-cluster-smoke-verification) can fail for reasons that could have been caught earlier: broken builds, missing `make generate`, or outdated CRDs. CI establishes the baseline quality gates that make smoke verification meaningful—it ensures the codebase compiles and tests pass before attempting cluster-level validation.

## What Changes

- Add GitHub Actions workflow(s) for repository CI
- Go: fmt, build, test; verify controller-manager, webhook-server, swiftctl build
- Rust: fmt, build, test; verify swiftletd build
- Code generation: verify `make generate` produces no changes (CRDs and deepcopy up to date)
- Container image build: validate swiftletd image builds (optional, where practical)
- Quality gates suitable for pull requests

## Capabilities

### New Capabilities

- `kubeswift-ci-build-and-test`: CI pipeline for Go and Rust build/test validation, code generation verification, and basic quality gates. Does not include production release, image publishing, signing, or changelog automation.

### Modified Capabilities

- (none)

## Impact

- **Paths:** `.github/workflows/`, `Makefile` (optional targets for CI)
- **Binaries:** controller-manager, webhook-server, swiftctl, swiftletd
- **Out of scope:** Versioned releases, publishing official images, signing, changelog automation, multi-arch release publishing
