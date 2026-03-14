## 1. Resolved model types

**Prerequisite:** add-core-kubeswift-api-types (API types) must be complete.

- [x] 1.1 Add internal/resolved/types.go with ResolvedGuest struct
- [x] 1.2 Add GuestSettings (architecture, firmware, bus, interface model, shutdown method)
- [x] 1.3 Add Resources (cpu, memory)
- [x] 1.4 Add RootDisk (size, format, source path, prepared info)
- [x] 1.5 Add Networks (one network for MVP)
- [x] 1.6 Add Seed (datasource, userData, metaData)
- [x] 1.7 Add Lifecycle (runPolicy)
- [x] 1.8 Add PreparedImage (path, format, size, ready)
- [x] 1.9 Add Meta (guest name, namespace, uid)
- [x] 1.10 Add ResolutionError type with Reason and AffectedResource

## 2. System defaults

- [x] 2.1 Add internal/resolved/defaults.go with system default values
- [x] 2.2 Define architecture default (x86_64)
- [x] 2.3 Define firmware, bus, interface model, shutdown method defaults
- [x] 2.4 Export defaults for use in merge logic

## 3. Merge logic

- [x] 3.1 Add internal/resolved/merge.go with merge functions
- [x] 3.2 Implement merge for Resources (cpu, memory from GuestClass; guest override if present)
- [x] 3.3 Implement merge for RootDisk (size, format from GuestClass; system default for format if absent)
- [x] 3.4 Implement merge for Lifecycle (runPolicy: guest > system default Running)
- [x] 3.5 Implement merge for GuestSettings (architecture, firmware, bus from class or system)
- [x] 3.6 Implement merge for Seed (from SwiftSeedProfile when referenced)
- [x] 3.7 Implement merge for PreparedImage (from SwiftImage when Ready)

## 4. Cross-object validation

- [x] 4.1 Add internal/resolved/validate.go with compatibility check functions
- [x] 4.2 Implement check: SwiftImage exists and is Ready
- [x] 4.3 Implement check: SwiftGuestClass exists
- [x] 4.4 Implement check: SwiftSeedProfile exists when referenced
- [ ] 4.5 Implement check: image architecture matches guest architecture (when both specified)
- [x] 4.6 Implement check: root disk format compatible with image format
- [ ] 4.7 Implement check: memory hotplug maxGuest >= guest memory (when hotplug specified)
- [x] 4.8 Return ResolutionError with reason for each failure case

## 5. Resolver pipeline

- [x] 5.1 Add internal/resolved/resolver.go with Resolver interface
- [x] 5.2 Implement Resolve(ctx, guest) -> (ResolvedGuest, error)
- [x] 5.3 Implement fetch step: get GuestClass, Image, SeedProfile from API
- [x] 5.4 Implement validate-existence step before merge
- [x] 5.5 Implement merge step: call merge functions
- [x] 5.6 Implement cross-object validation step after merge
- [x] 5.7 Return ResolvedGuest on success, ResolutionError on failure

## 6. Unit tests for merge precedence

- [x] 6.1 Add internal/resolved/merge_test.go
- [x] 6.2 Test: guest runPolicy overrides system default
- [x] 6.3 Test: class cpu used when guest has no override
- [x] 6.4 Test: class memory used when guest has no override
- [x] 6.5 Test: system default architecture when guest and class omit
- [x] 6.6 Test: class rootDisk size and format applied
- [x] 6.7 Test: guest explicit field wins over class (for override-capable fields)
- [x] 6.8 Test: seed from SwiftSeedProfile when referenced
- [x] 6.9 Test: no seed when SwiftSeedProfile not referenced

## 7. Unit tests for validation failures

- [x] 7.1 Add internal/resolved/validate_test.go or resolver_test.go
- [x] 7.2 Test: Resolve fails when SwiftImage does not exist
- [x] 7.3 Test: Resolve fails when SwiftImage is not Ready
- [x] 7.4 Test: Resolve fails when SwiftGuestClass does not exist
- [x] 7.5 Test: Resolve fails when SwiftSeedProfile does not exist (when referenced)
- [x] 7.6 Test: Resolve fails when architecture mismatch (guest vs image)
- [x] 7.7 Test: Resolve fails when memory hotplug maxGuest < guest memory
- [x] 7.8 Test: ResolutionError includes reason string
- [x] 7.9 Test: Resolve succeeds when all checks pass

## 8. Integration with controller (scaffold)

- [x] 8.1 Document in internal/controller/ or design doc: controller MUST call Resolver.Resolve first
- [x] 8.2 Document: on ResolutionError, controller sets condition and returns; on success, controller uses ResolvedGuest only
- [x] 8.3 Add example or stub in internal/controller/ showing Resolve invocation (if controller scaffold exists)
