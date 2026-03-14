## ADDED Requirements

### Requirement: SwiftGuest API type in swift.kubeswift.io/v1alpha1

SwiftGuest MUST be defined as a Go type in api/swift/v1alpha1/ with package swift.kubeswift.io and version v1alpha1. The spec MUST include imageRef, guestClassRef, seedProfileRef (optional), and runPolicy.

#### Scenario: SwiftGuest type exists

- **WHEN** the API package is built
- **THEN** the SwiftGuest type exists in api/swift/v1alpha1/swiftguest_types.go

#### Scenario: SwiftGuest spec has required fields

- **WHEN** a SwiftGuest is created
- **THEN** the spec includes imageRef and guestClassRef (required); seedProfileRef and runPolicy (optional)

#### Scenario: SwiftGuest has status subresource

- **WHEN** the SwiftGuest CRD is generated
- **THEN** the CRD includes subresources.status for the status field

#### Scenario: No KubeVirt naming

- **WHEN** SwiftGuest types are defined
- **THEN** they do NOT use KubeVirt names (e.g., VirtualMachine, VirtualMachineInstance)

### Requirement: SwiftGuestClass API type in swift.kubeswift.io/v1alpha1

SwiftGuestClass MUST be defined as a Go type in api/swift/v1alpha1/ with spec fields cpu, memory, and rootDisk. RootDisk MUST include size and format (raw or qcow2).

#### Scenario: SwiftGuestClass type exists

- **WHEN** the API package is built
- **THEN** the SwiftGuestClass type exists in api/swift/v1alpha1/swiftguestclass_types.go

#### Scenario: SwiftGuestClass spec has root disk

- **WHEN** a SwiftGuestClass is created
- **THEN** the spec includes rootDisk with size (Quantity) and format (raw | qcow2)

#### Scenario: One root disk only

- **WHEN** SwiftGuestClass spec is defined
- **THEN** it defines exactly one root disk (no array of disks)

### Requirement: SwiftImage API type in image.kubeswift.io/v1alpha1

SwiftImage MUST be defined as a Go type in api/image/v1alpha1/ with spec fields source and format. Format MUST be explicitly declared (raw or qcow2); no format guessing.

#### Scenario: SwiftImage type exists

- **WHEN** the API package is built
- **THEN** the SwiftImage type exists in api/image/v1alpha1/swiftimage_types.go

#### Scenario: SwiftImage format explicit

- **WHEN** a SwiftImage is created
- **THEN** the spec includes format (raw | qcow2); the system does NOT infer format from source

#### Scenario: SwiftImage has status subresource

- **WHEN** the SwiftImage CRD is generated
- **THEN** the CRD includes subresources.status for phase and conditions

#### Scenario: SwiftImage immutable when ready

- **WHEN** SwiftImage status is Ready
- **THEN** spec mutations are rejected (enforced by webhook or controller; API contract documented)

### Requirement: SwiftSeedProfile API type in seed.kubeswift.io/v1alpha1

SwiftSeedProfile MUST be defined as a Go type in api/seed/v1alpha1/ with spec fields datasource, userData, and metaData (optional). MVP MUST support datasource NoCloud only.

#### Scenario: SwiftSeedProfile type exists

- **WHEN** the API package is built
- **THEN** the SwiftSeedProfile type exists in api/seed/v1alpha1/swiftseedprofile_types.go

#### Scenario: NoCloud datasource supported

- **WHEN** a SwiftSeedProfile is created
- **THEN** datasource MAY be NoCloud; other datasources (ConfigDrive, Ignition) are out of scope for MVP

#### Scenario: UserData for cloud-init

- **WHEN** a SwiftSeedProfile specifies userData
- **THEN** the value is used to generate NoCloud datasource media; KubeSwift does NOT reimplement cloud-init

### Requirement: Package layout under api/

API types MUST reside under api/ with structure api/<group>/v1alpha1/ for swift, image, and seed. Shared types (e.g., LocalObjectReference, conditions) MUST reside in api/shared/ or a common package.

#### Scenario: Package paths correct

- **WHEN** API types are imported
- **THEN** swift types are in github.com/projectbeskar/kubeswift/api/swift/v1alpha1, image in api/image/v1alpha1, seed in api/seed/v1alpha1

#### Scenario: Shared types available

- **WHEN** SwiftGuest or SwiftImage references LocalObjectReference
- **THEN** the type is defined in api/shared/ or a package that avoids circular imports

### Requirement: CRD generation from Go types

CRDs MUST be generated from Go types using controller-gen (or equivalent). Generated manifests MUST be placed in config/crd/bases/. CRDs MUST include OpenAPI schema for validation.

#### Scenario: CRDs generated

- **WHEN** `make generate` or equivalent is run
- **THEN** config/crd/bases/ contains YAML for SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile

#### Scenario: CRD schema validation

- **WHEN** a CRD is applied
- **THEN** the CRD includes spec.validation.openAPIV3Schema with type constraints for required fields

### Requirement: Sample YAML for each resource

Sample YAML manifests MUST exist for SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile in config/samples/.

#### Scenario: Sample manifests exist

- **WHEN** the repository is inspected
- **THEN** config/samples/ contains swiftguest.yaml, swiftguestclass.yaml, swiftimage.yaml, swiftseedprofile.yaml

#### Scenario: Samples are valid

- **WHEN** sample YAML is applied against a cluster with CRDs installed
- **THEN** each sample creates a valid resource (or documents required cluster state)

### Requirement: Validation boundaries documented

Validation boundaries MUST be documented: CRD schema for types and required fields; webhook for cross-field and reference validation (future); controller for runtime validation (future).

#### Scenario: Schema validates types

- **WHEN** a user submits a SwiftGuest with negative cpu in guestClassRef
- **THEN** the CRD schema (or referenced SwiftGuestClass validation) rejects invalid values where applicable

#### Scenario: Default assignment documented

- **WHEN** defaults are assigned (webhook vs resolver vs controller)
- **THEN** the design document specifies which component assigns each default
