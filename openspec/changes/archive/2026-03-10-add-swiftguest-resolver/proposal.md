## Why

The SwiftGuest controller must produce a normalized, fully-resolved spec before creating the pod envelope or handing off to swiftletd. Raw CRDs (SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile) contain references and partial specs; the controller cannot operate on them directly for runtime decisions. A resolver pipeline merges these resources into a single internal model with defaults applied, cross-object compatibility validated, and seed materialization inputs prepared. This change implements the resolved-spec model and resolver so that the reconciler works exclusively on the resolved output—never on raw CRDs once resolution starts.

## What Changes

- Add internal/resolved package with normalized internal model types
- Implement merge logic for SwiftGuest + SwiftGuestClass + SwiftImage + SwiftSeedProfile
- Apply merge precedence: guest explicit fields > class defaults > system defaults
- Add seed data materialization inputs to the resolved model
- Add cross-object compatibility checks (architecture match, format compatibility, etc.)
- Ensure reconciler operates only on resolved output after resolution succeeds
- Add unit tests for merge precedence and validation failures

**Intentionally excluded:**

- Full SwiftGuest controller reconciliation (pod creation, status updates)
- Seed media generation (materialization outputs)
- Runtime intent serialization for swiftletd
- Multiple disks or networks beyond MVP

## Capabilities

### New Capabilities

- `swiftguest-resolver`: Internal resolved-spec model and resolver pipeline; merge logic with precedence guest > class > system; normalized model containing guest settings, resources, root disk, networks, seed inputs, lifecycle, prepared image info; cross-object compatibility checks; reconciler operates on resolved output only; package placement in internal/resolved/.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: internal/resolved/, internal/controller/ (resolver invocation)
- **Prerequisites**: add-core-kubeswift-api-types (API types)
- **Dependencies**: api/swift, api/image, api/seed packages
- **Risks**: Resolver logic is central to correctness; merge precedence must be well-tested
- **Rollback**: Remove internal/resolved/; controller would need to be reverted to match
