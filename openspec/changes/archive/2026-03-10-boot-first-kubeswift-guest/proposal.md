## Why

Prerequisite changes implement isolated components: core APIs, resolver, SwiftImage controller, NoCloud seed rendering, launcher pod and runtime intent, swiftletd MVP. No integration path exists. A user who creates SwiftImage, SwiftSeedProfile, and SwiftGuest cannot boot a VM—mount paths may mismatch, runtime intent format may not align with swiftletd, and status fields may be inconsistent. This change fixes those integration gaps, adds sample manifests and a smoke test, and delivers the first bootable guest. It validates the KubeSwift control-plane → node-runtime handoff without introducing new architecture.

**Architecture fit:** KubeSwift separates control-plane orchestration (Go) from node-local VM realization (Rust swiftletd + Cloud Hypervisor). The pipeline is: Resolver → pod envelope + runtime intent → swiftletd → Cloud Hypervisor. This change confirms that pipeline works end-to-end. No new components; integration and verification only.

## What Changes

- Align mount paths between SwiftGuest controller (internal/controller/swiftguest/) and swiftletd (rust/swiftletd/): image, seed ConfigMap, runtime intent
- Align runtime intent JSON format with swiftletd expectations (disk path, seed path, resources)
- Fix status field mismatches: GuestRunning condition, phase, preparedArtifact reference
- Add sample manifests in config/samples/: SwiftImage (http source), SwiftSeedProfile, SwiftGuest
- Add docs/first-boot.md: prerequisites, apply order, verification steps, troubleshooting
- Add smoke test in test/smoke/: script that applies samples, waits for Ready/Running, asserts conditions, cleans up

**Intentionally excluded:**

- Live migration, snapshots, VFIO, vhost-user
- Multi-disk, multi-network
- Serial console wiring
- Production hardening (retries, backoff, observability)

## Capabilities

### New Capabilities

- `boot-first-guest`: Integration fixes (mount paths, runtime intent format, status fields); sample manifests (SwiftImage, SwiftSeedProfile, SwiftGuest); docs/first-boot.md; smoke test in test/smoke/; first bootable Linux cloud guest.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: config/samples/, docs/, test/smoke/, internal/controller/swiftguest/, internal/controller/swiftimage/, rust/swiftletd/
- **Prerequisites** (must be implemented first): add-core-kubeswift-api-types, add-swiftguest-resolver, implement-swiftimage-controller, implement-seed-rendering-for-nocloud, add-launcher-pod-and-runtime-intent, implement-swiftletd-mvp
- **Dependencies**: SwiftImage status.preparedArtifact.pvcRef; ResolvedGuest; runtime intent format from add-launcher-pod-and-runtime-intent; swiftletd intent/seed/image paths
- **Risks**: Sample image URL may go stale; image import can be slow; node must have Cloud Hypervisor binary
- **Rollback**: Revert patches to internal/controller/ and rust/swiftletd/; delete config/samples/, docs/first-boot.md, test/smoke/; no CRD or API changes; existing SwiftGuest/SwiftImage resources unchanged
