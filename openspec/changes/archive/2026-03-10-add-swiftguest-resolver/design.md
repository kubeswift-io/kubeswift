## Context

The SwiftGuest controller must reconcile SwiftGuest by creating a pod envelope and preparing runtime intent for swiftletd. The controller receives SwiftGuest, which references SwiftGuestClass, SwiftImage, and optionally SwiftSeedProfile. These are separate CRDs with partial specs; the controller cannot make runtime decisions (e.g., CPU, memory, disk path) by reading raw CRDs directly. A resolver pipeline fetches referenced resources, merges them with precedence, validates cross-object compatibility, and produces a normalized internal model. Once resolution succeeds, the reconciler operates exclusively on the resolved output—never on raw CRDs.

**Constraints:** internal/resolved/ per monorepo; add-core-kubeswift-api-types prerequisite; one root disk, one network, NoCloud seed for MVP.

## Goals / Non-Goals

**Goals:**

- Implement internal/resolved package with normalized model types
- Implement merge logic: SwiftGuest + SwiftGuestClass + SwiftImage + SwiftSeedProfile
- Apply merge precedence: guest explicit > class defaults > system defaults
- Add seed data materialization inputs to resolved model
- Add cross-object compatibility checks
- Ensure reconciler uses only resolved output after resolution
- Add unit tests for merge precedence and validation failures

**Non-Goals:**

- Full controller reconciliation (pod creation, status updates)
- Seed media generation (outputs)
- Runtime intent serialization for swiftletd

## Decisions

### 1. Reconciler does not operate on raw CRDs once resolution starts

**Flow:**

1. Controller receives SwiftGuest (reconcile request).
2. Controller invokes Resolver.Resolve(guest, ctx) which fetches GuestClass, Image, SeedProfile.
3. Resolver merges and validates; returns ResolvedGuest or ResolutionError.
4. If ResolutionError: controller sets condition, returns (no further work).
5. If ResolvedGuest: controller uses ResolvedGuest only for all subsequent logic (pod spec, runtime intent). Controller MUST NOT read guest.Spec, guestClass.Spec, etc. for runtime decisions.

**Rationale:** The resolved model is the single source of truth for "what to run." Raw CRDs are inputs to resolution; after resolution, they are irrelevant for runtime logic. This prevents bugs from inconsistent reads and makes the controller easier to reason about.

### 2. Merge precedence: guest explicit > class defaults > system defaults

| Layer | Source | Examples |
|-------|--------|----------|
| Guest explicit | SwiftGuest.Spec | runPolicy, any override fields on guest |
| Class defaults | SwiftGuestClass.Spec | cpu, memory, rootDisk (size, format) |
| System defaults | Resolver internal | architecture x86_64, bus virtio, firmware, etc. |

**Merge algorithm (per field):**

1. If SwiftGuest specifies the field (and it is a guest-level override): use it.
2. Else if SwiftGuestClass specifies the field: use it.
3. Else: use system default.

**Note:** SwiftGuest has few direct fields (imageRef, guestClassRef, seedProfileRef, runPolicy). Most "guest" overrides would come from future API extensions (e.g., SwiftGuest.Spec.Resources override). For MVP, guest explicit applies mainly to runPolicy; class provides cpu, memory, rootDisk; system provides architecture, bus, firmware.

### 3. Resolved model structure

The ResolvedGuest type MUST contain:

```
ResolvedGuest
├── GuestSettings      // architecture, firmware, bus, interface model, shutdown method
├── Resources          // cpu, memory (from class, merged)
├── RootDisk           // size, format, source path (from image), prepared info
├── Networks           // one network for MVP (interface model, etc.)
├── Seed               // materialization inputs: datasource, userData, metaData
├── Lifecycle          // runPolicy, start/stop intent
├── PreparedImage      // image path, format, size, ready
└── Meta              // guest name, namespace, uid (for pod naming, etc.)
```

**PreparedImage:** Resolved from SwiftImage; includes the path where the image is available (e.g., PVC mount path), format, and Ready state. The controller uses this to populate pod volumes and runtime intent.

**Seed:** Inputs for seed materialization—datasource type, userData, metaData. The actual generation of NoCloud media is a separate step (not in this change); the resolver only provides the inputs.

### 4. Package and file layout

```
internal/resolved/
├── types.go           # ResolvedGuest, GuestSettings, Resources, RootDisk, etc.
├── resolver.go        # Resolver interface, Resolve(ctx, guest) -> (ResolvedGuest, error)
├── merge.go           # merge logic: apply precedence per field
├── validate.go        # cross-object compatibility checks
└── defaults.go        # system defaults
```

**Rationale:** types.go holds the model; resolver.go orchestrates fetch + merge + validate; merge.go encapsulates precedence logic; validate.go holds compatibility checks; defaults.go holds system defaults. All under internal/resolved/ in github.com/projectbeskar/kubeswift.

### 5. Resolution pipeline steps

1. **Fetch:** Get SwiftGuestClass, SwiftImage, SwiftSeedProfile (if ref present) from API server.
2. **Validate existence:** All required refs must exist; SwiftImage must be Ready.
3. **Merge:** Apply precedence to produce ResolvedGuest.
4. **Cross-object compatibility:** Validate architecture match (guest vs image), format compatibility, etc.
5. **Return:** ResolvedGuest or ResolutionError with reason.

### 6. Cross-object compatibility checks

| Check | When | Action |
|-------|------|--------|
| imageRef exists | Fetch | ResolutionError if not found |
| SwiftImage Ready | Fetch | ResolutionError if not Ready |
| guestClassRef exists | Fetch | ResolutionError if not found |
| seedProfileRef exists (if specified) | Fetch | ResolutionError if not found |
| Image architecture matches guest architecture | After merge | ResolutionError if mismatch |
| Root disk format compatible with image format | After merge | ResolutionError if incompatible |
| Memory hotplug maxGuest >= guest memory | After merge | ResolutionError if invalid |

### 7. Seed data materialization inputs

ResolvedGuest.Seed MUST contain:

- Datasource: NoCloud (from SwiftSeedProfile)
- UserData: string (from SwiftSeedProfile)
- MetaData: string (from SwiftSeedProfile, optional)

If SwiftSeedProfile is not referenced, Seed is nil or empty (no cloud-init). The materialization step (separate change) uses these inputs to generate NoCloud media.

### 8. ResolutionError type

Resolver returns a structured error (ResolutionError) with:

- Reason: string (e.g., "SwiftImage not Ready", "architecture mismatch")
- AffectedResource: optional reference to the failing resource

The controller uses this to set a condition on SwiftGuest (e.g., Resolved=False, reason=Reason).

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Merge logic complexity | Unit tests for each precedence case; explicit field-by-field merge |
| API changes require resolver updates | Resolver is internal; version with API |
| Fetch failures (e.g., stale cache) | Controller retries; resolution is idempotent |
| Circular or deep references | MVP has flat refs only; no cycles |

## Migration Plan

1. Add internal/resolved/ types and resolver
2. Add merge, validate, defaults logic
3. Add unit tests
4. Controller (future change) invokes Resolver.Resolve and uses ResolvedGuest
5. **Rollback:** Remove internal/resolved/; revert controller to use raw CRDs (not recommended)

## Open Questions

- Whether ResolvedGuest should be serializable for runtime intent (e.g., JSON for swiftletd)—design assumes it can be; exact format deferred
- Whether to cache resolved output per reconcile (e.g., if CRDs unchanged)—optimization for later
