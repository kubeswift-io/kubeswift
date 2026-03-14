## Why

Developers need a reproducible, verification-focused path to validate that a local KubeSwift cluster can boot a Linux cloud guest end-to-end. The existing `test/smoke/boot-test.sh` and `docs/first-boot.md` provide a starting point but lack explicit verification steps for each stage (SwiftImage Ready, SwiftGuest scheduling, seed rendering, swiftletd launching Cloud Hypervisor, status conditions). Without documented prerequisites, exact commands, expected conditions, and failure checks, developers cannot reliably confirm that their cluster is working or diagnose where the pipeline breaks.

## What Changes

- Add a verification-focused smoke test specification and implementation
- Document exact local cluster prerequisites (CRDs, controllers, swiftletd image, RBAC, node requirements)
- Define verification steps with exact manifests, commands, expected conditions, and failure checks for:
  - SwiftImage reaching Ready
  - SwiftGuest scheduling (pod creation, placement)
  - Seed rendering and mount path correctness
  - swiftletd launching Cloud Hypervisor
  - SwiftGuest reaching Running with clear status conditions (Resolved, PodScheduled, GuestRunning)
- Extend or refactor `test/smoke/boot-test.sh` to align with the verification spec
- Add or update docs (e.g., `docs/first-boot.md`, `docs/smoke-verification.md`) with reproducible commands

## Capabilities

### New Capabilities

- `first-boot-cluster-smoke-verification`: Verification-focused smoke testing for first-boot cluster flow. Covers SwiftImage Ready, SwiftGuest scheduling, seed rendering and mount path, swiftletd launching Cloud Hypervisor, and SwiftGuest Running with status conditions. Includes exact prerequisites, manifests, commands, expected conditions, and failure checks for local developer use.

### Modified Capabilities

- (none)

## Impact

- **Paths:** `test/smoke/`, `docs/`, `config/samples/`, `Makefile`
- **Scripts:** `test/smoke/boot-test.sh` (extend or refactor)
- **Docs:** `docs/first-boot.md`, new `docs/smoke-verification.md` (or equivalent)
- **Dependencies:** Existing CRDs, controllers, swiftletd image, config/samples manifests
- **Out of scope:** Production CI/CD pipelines, performance benchmarks, migration, snapshots, Windows support
