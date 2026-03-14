## ADDED Requirements

### Requirement: SwiftGuest admission validation

The admission webhook MUST validate SwiftGuest create and update requests. Validation MUST enforce syntax rules only (no cross-resource cluster lookups).

#### Scenario: Exactly one boot source

- **WHEN** a SwiftGuest is created or updated with multiple boot sources (e.g., imageRef and another boot source)
- **THEN** the webhook rejects the request with an error indicating exactly one boot source is required

#### Scenario: Single boot source accepted

- **WHEN** a SwiftGuest is created with imageRef as the only boot source
- **THEN** the webhook accepts the request (subject to other validation rules)

#### Scenario: runPolicy valid enum

- **WHEN** a SwiftGuest specifies runPolicy with a value other than Running or Stopped
- **THEN** the webhook rejects the request

#### Scenario: Required refs present

- **WHEN** a SwiftGuest is created without imageRef or guestClassRef
- **THEN** the webhook rejects the request

### Requirement: SwiftGuest memory hotplug validation

When SwiftGuest or SwiftGuestClass specifies memory hotplug, maxGuest MUST be >= guest memory. This validation MAY be enforced at admission (if both values are in the request) or at reconcile time.

#### Scenario: Memory hotplug maxGuest too low

- **WHEN** a SwiftGuest or SwiftGuestClass specifies memory hotplug with maxGuest less than guest memory
- **THEN** the webhook or controller rejects or fails the request with an error

#### Scenario: Memory hotplug maxGuest valid

- **WHEN** maxGuest >= guest memory
- **THEN** the webhook accepts (subject to other rules)

### Requirement: SwiftImage admission validation

The admission webhook MUST validate SwiftImage create and update requests. Validation MUST enforce syntax rules only.

#### Scenario: Image source exactly one type

- **WHEN** a SwiftImage specifies both URL and PVC in source (or neither)
- **THEN** the webhook rejects the request with an error indicating exactly one source type is required

#### Scenario: Image source exactly one type accepted

- **WHEN** a SwiftImage specifies only URL or only PVC in source
- **THEN** the webhook accepts the request (subject to other validation rules)

#### Scenario: Image format explicit

- **WHEN** a SwiftImage is created without format or with empty format
- **THEN** the webhook rejects the request; format MUST be explicit (raw or qcow2)

#### Scenario: Image format explicit accepted

- **WHEN** a SwiftImage specifies format as raw or qcow2
- **THEN** the webhook accepts the request (subject to other validation rules)

### Requirement: SwiftSeedProfile admission validation

The admission webhook MUST validate SwiftSeedProfile create and update requests. NoCloud cannot include unsupported combinations.

#### Scenario: NoCloud unsupported combinations rejected

- **WHEN** a SwiftSeedProfile with datasource NoCloud includes fields or combinations not supported by NoCloud (e.g., ConfigDrive-specific fields)
- **THEN** the webhook rejects the request with an error

#### Scenario: NoCloud valid combination accepted

- **WHEN** a SwiftSeedProfile with datasource NoCloud has valid userData and no unsupported fields
- **THEN** the webhook accepts the request

#### Scenario: Non-NoCloud datasource rejected for MVP

- **WHEN** a SwiftSeedProfile specifies datasource other than NoCloud (e.g., ConfigDrive, Ignition)
- **THEN** the webhook rejects the request for MVP scope

### Requirement: Image architecture matches guest architecture

When both SwiftGuest and SwiftImage specify architecture, the image architecture MUST match the guest architecture. This MAY be enforced at admission (if both in same namespace and resolvable) or at reconcile time by the controller.

#### Scenario: Architecture mismatch rejected

- **WHEN** a SwiftGuest references a SwiftImage and the guest architecture does not match the image architecture
- **THEN** the request is rejected (at admission or reconcile) with an error

#### Scenario: Architecture match accepted

- **WHEN** guest and image architectures match (or architecture is unspecified)
- **THEN** the request is accepted

### Requirement: SwiftGuest defaulting

The admission webhook MUST apply defaults to SwiftGuest when fields are omitted:

- runPolicy: Running
- architecture: x86_64 (when field exists)
- firmware: default for Cloud Hypervisor (when field exists)
- bus: virtio (when field exists)
- interface model: virtio (when field exists)
- shutdown method: ACPI (when field exists)

#### Scenario: runPolicy defaulted

- **WHEN** a SwiftGuest is created without runPolicy
- **THEN** the mutating webhook sets runPolicy to Running

#### Scenario: runPolicy not overwritten

- **WHEN** a SwiftGuest is created with runPolicy Stopped
- **THEN** the mutating webhook does not change runPolicy

#### Scenario: Architecture defaulted when omitted

- **WHEN** a SwiftGuest is created without architecture and the field exists in the API
- **THEN** the mutating webhook sets architecture to x86_64

### Requirement: SwiftImage defaulting

The admission webhook MUST apply defaults to SwiftImage when fields are omitted. Format default: raw (when allowed by API).

#### Scenario: Format defaulted when omitted

- **WHEN** a SwiftImage is created without format and the API allows defaulting
- **THEN** the mutating webhook sets format to raw

### Requirement: SwiftSeedProfile defaulting

The admission webhook MUST apply defaults to SwiftSeedProfile when fields are omitted. datasource default: NoCloud for MVP.

#### Scenario: Datasource defaulted

- **WHEN** a SwiftSeedProfile is created without datasource
- **THEN** the mutating webhook sets datasource to NoCloud

### Requirement: Syntax validation separate from cross-resource checks

Syntax validation (single-resource, no cluster lookups) MUST be implemented in the admission webhook. Cross-resource resolution checks (reference existence, image Ready state) MUST be implemented in the controller at reconcile time.

#### Scenario: Webhook does not fetch referenced resources

- **WHEN** the admission webhook validates a SwiftGuest with imageRef
- **THEN** the webhook does NOT call the API server to fetch the referenced SwiftImage

#### Scenario: Controller performs cross-resource checks

- **WHEN** the controller reconciles a SwiftGuest
- **THEN** the controller fetches SwiftImage, SwiftGuestClass, SwiftSeedProfile and validates references exist and are Ready where applicable

### Requirement: No business logic in webhook

The webhook MUST NOT implement business logic beyond validation and defaulting. It MUST NOT create or update other resources, trigger imports, or perform reconciliation.

#### Scenario: Webhook does not create resources

- **WHEN** the webhook processes a create or update request
- **THEN** the webhook does NOT create or update Pods, ConfigMaps, or any other resources

#### Scenario: Webhook only validates and defaults

- **WHEN** the webhook processes a request
- **THEN** the webhook either rejects (validation) or returns the object with defaults applied (mutation)

### Requirement: Webhook package placement

Webhook handlers MUST reside in internal/webhook/ with structure internal/webhook/swiftguest/, internal/webhook/swiftimage/, internal/webhook/swiftseedprofile/.

#### Scenario: Handlers in internal/webhook

- **WHEN** the repository is inspected
- **THEN** validation and defaulting logic resides under internal/webhook/ in github.com/projectbeskar/kubeswift

#### Scenario: Layout consistent with monorepo

- **WHEN** webhook files are added
- **THEN** paths are repository-relative and consistent with api/, cmd/, internal/, config/ layout
