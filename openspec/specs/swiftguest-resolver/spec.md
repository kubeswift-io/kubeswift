# swiftguest-resolver Specification

## Purpose

The SwiftGuest resolver normalizes SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile into a single ResolvedGuest model. Controllers MUST use ResolvedGuest for runtime decisions and MUST NOT read raw CRD specs after resolution.

## Requirements

### Requirement: Internal resolved package

The resolved-spec model and resolver pipeline MUST reside in internal/resolved/ in github.com/projectbeskar/kubeswift.

#### Scenario: Package exists

- **WHEN** the repository is inspected
- **THEN** internal/resolved/ contains types, resolver, merge, validate, and defaults logic

#### Scenario: Paths repository-relative

- **WHEN** imports reference the resolved package
- **THEN** the path is github.com/projectbeskar/kubeswift/internal/resolved

### Requirement: Resolved model contains required sections

The ResolvedGuest type MUST contain normalized guest settings, resources, root disk, networks, seed (materialization inputs), lifecycle, and prepared image info.

#### Scenario: ResolvedGuest has all sections

- **WHEN** ResolvedGuest is produced by the resolver
- **THEN** it includes GuestSettings, Resources, RootDisk, Networks, Seed, Lifecycle, PreparedImage, and Meta (or equivalent named fields)

#### Scenario: PreparedImage has path and format

- **WHEN** resolution succeeds with a Ready SwiftImage
- **THEN** PreparedImage contains the image path (or volume reference), format, and ready state

#### Scenario: Seed contains materialization inputs

- **WHEN** SwiftGuest references a SwiftSeedProfile
- **THEN** ResolvedGuest.Seed contains datasource, userData, and metaData (or equivalent) for NoCloud generation

### Requirement: Merge precedence guest explicit over class over system

Merge precedence MUST be: guest explicit fields > class defaults > system defaults. For each field, the resolved value is the first non-empty value from guest, then class, then system.

#### Scenario: Guest runPolicy overrides class

- **WHEN** SwiftGuest specifies runPolicy Stopped and SwiftGuestClass has no runPolicy
- **THEN** ResolvedGuest.Lifecycle.RunPolicy is Stopped

#### Scenario: Class cpu used when guest has no override

- **WHEN** SwiftGuest does not specify cpu and SwiftGuestClass specifies cpu: "2"
- **THEN** ResolvedGuest.Resources.CPU is 2

#### Scenario: System default when guest and class omit

- **WHEN** neither SwiftGuest nor SwiftGuestClass specifies architecture
- **THEN** ResolvedGuest.GuestSettings.Architecture is the system default (e.g., x86_64)

#### Scenario: Guest override wins over class

- **WHEN** SwiftGuest specifies a field that overrides SwiftGuestClass (for fields that support override)
- **THEN** the guest value is used in ResolvedGuest

### Requirement: Reconciler does not operate on raw CRDs after resolution

Once resolution succeeds, the reconciler MUST operate only on the ResolvedGuest output. The reconciler MUST NOT read SwiftGuest.Spec, SwiftGuestClass.Spec, SwiftImage.Spec, or SwiftSeedProfile.Spec for runtime decisions (pod creation, runtime intent).

#### Scenario: Controller uses ResolvedGuest for pod spec

- **WHEN** the controller creates or updates the pod envelope
- **THEN** it uses ResolvedGuest.Resources, ResolvedGuest.RootDisk, etc.—not raw CRD spec fields

#### Scenario: Controller does not read raw CRDs for runtime logic

- **WHEN** the controller performs reconciliation (pod creation, status update)
- **THEN** it does NOT read guest.Spec.CPU, guestClass.Spec.Memory, etc. for runtime decisions; it uses ResolvedGuest only

### Requirement: Resolver merges all four resources

The resolver MUST merge SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile (when referenced) into a single ResolvedGuest.

#### Scenario: All four resources merged

- **WHEN** Resolve is called with a SwiftGuest that references GuestClass, Image, and SeedProfile
- **THEN** the resolver fetches all three and merges them into ResolvedGuest

#### Scenario: SeedProfile optional

- **WHEN** SwiftGuest does not reference SwiftSeedProfile
- **THEN** ResolvedGuest.Seed is nil or empty; resolution succeeds without seed

### Requirement: Cross-object compatibility checks

The resolver MUST perform cross-object compatibility checks and return ResolutionError when checks fail.

#### Scenario: SwiftImage not Ready

- **WHEN** the referenced SwiftImage status is not Ready
- **THEN** Resolve returns ResolutionError; ResolvedGuest is not produced

#### Scenario: imageRef missing

- **WHEN** the referenced SwiftImage does not exist
- **THEN** Resolve returns ResolutionError

#### Scenario: guestClassRef missing

- **WHEN** the referenced SwiftGuestClass does not exist
- **THEN** Resolve returns ResolutionError

#### Scenario: Architecture mismatch

- **WHEN** SwiftGuest (or GuestClass) architecture does not match SwiftImage architecture
- **THEN** Resolve returns ResolutionError

#### Scenario: seedProfileRef missing when specified

- **WHEN** SwiftGuest references SwiftSeedProfile but it does not exist
- **THEN** Resolve returns ResolutionError

### Requirement: Seed materialization inputs

ResolvedGuest.Seed MUST contain the inputs required for seed media generation: datasource type (NoCloud), userData, and metaData (optional). These are passed through from SwiftSeedProfile without transformation (except for resolution of SecretRef if present).

#### Scenario: Seed inputs from SwiftSeedProfile

- **WHEN** SwiftGuest references a SwiftSeedProfile with userData "#!/bin/bash" and metaData "instance-id: test"
- **THEN** ResolvedGuest.Seed contains datasource NoCloud, userData "#!/bin/bash", metaData "instance-id: test"

#### Scenario: No seed when no profile

- **WHEN** SwiftGuest does not reference SwiftSeedProfile
- **THEN** ResolvedGuest.Seed is nil or empty; no seed materialization needed

### Requirement: ResolutionError is structured

Resolve MUST return a structured error (ResolutionError or equivalent) that includes a reason and optionally the affected resource reference. The controller uses this to set a condition on SwiftGuest.

#### Scenario: ResolutionError has reason

- **WHEN** Resolve fails (e.g., image not Ready)
- **THEN** the returned error includes a reason string (e.g., "SwiftImage not Ready")

#### Scenario: Controller can set condition from error

- **WHEN** the controller receives ResolutionError
- **THEN** it can set a condition on SwiftGuest (e.g., Resolved=False, reason=error.Reason)
