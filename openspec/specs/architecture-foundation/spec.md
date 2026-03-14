# architecture-foundation Specification

## Purpose
TBD - created by archiving change create-kubeswift-architecture-foundation. Update Purpose after archive.
## Requirements
### Requirement: Control plane implemented in Go

The KubeSwift control plane (controllers, API types, webhooks, manager binary) MUST be implemented in Go. The implementation MUST use controller-runtime and Kubebuilder-style APIs.

#### Scenario: Control plane builds as Go binary

- **WHEN** the control plane is built
- **THEN** the manager binary is produced from Go source under `cmd/manager/` in github.com/projectbeskar/kubeswift

#### Scenario: API types use Go structs

- **WHEN** API types for SwiftGuest, SwiftGuestClass, SwiftImage, SwiftSeedProfile are defined
- **THEN** they reside in Go packages under `pkg/apis/` in github.com/projectbeskar/kubeswift

### Requirement: Node runtime implemented in Rust

The KubeSwift node runtime and low-level VM helpers MUST be implemented in Rust. The node daemon `swiftletd` MUST be a Rust binary that launches and manages Cloud Hypervisor on the node.

#### Scenario: swiftletd builds as Rust binary

- **WHEN** the node runtime is built
- **THEN** the swiftletd binary is produced from Rust source under `swiftletd/` or `cmd/swiftletd/` in github.com/projectbeskar/kubeswift

#### Scenario: No libvirt in node path

- **WHEN** swiftletd launches a VM
- **THEN** it uses the Cloud Hypervisor API directly via local Unix sockets; libvirt MUST NOT be used

### Requirement: Primary API groups

KubeSwift MUST define and use the following API groups with initial version v1alpha1:

- `swift.kubeswift.io` (core guest resources)
- `image.kubeswift.io` (image resources)
- `seed.kubeswift.io` (seed/initialization resources)

#### Scenario: API groups present in CRD manifests

- **WHEN** CRDs are installed
- **THEN** each CRD specifies one of swift.kubeswift.io, image.kubeswift.io, or seed.kubeswift.io as its group

#### Scenario: No KubeVirt API groups

- **WHEN** KubeSwift APIs are defined
- **THEN** KubeVirt naming (e.g., VirtualMachine, kubevirt.io) MUST NOT be used

### Requirement: Primary initial resources

KubeSwift MUST define the following primary resources as CRDs in the initial scope:

- SwiftGuest (swift.kubeswift.io)
- SwiftGuestClass (swift.kubeswift.io)
- SwiftImage (image.kubeswift.io)
- SwiftSeedProfile (seed.kubeswift.io)

#### Scenario: CRDs exist for primary resources

- **WHEN** the API scaffolding is complete
- **THEN** CRD manifests exist for SwiftGuest, SwiftGuestClass, SwiftImage, and SwiftSeedProfile

### Requirement: One guest per pod envelope

Each SwiftGuest MUST map to exactly one Kubernetes Pod (the pod envelope). The pod MUST be used for scheduling, resource accounting, networking attachment, and mounted storage. The VM runtime MUST NOT use a different scheduling unit.

#### Scenario: SwiftGuest creates single pod

- **WHEN** a SwiftGuest is reconciled
- **THEN** the controller creates or updates exactly one Pod for that guest

#### Scenario: Pod carries runtime intent

- **WHEN** the pod is scheduled to a node
- **THEN** the runtime intent (resolved spec) is available to swiftletd for that pod

### Requirement: swiftletd manages Cloud Hypervisor on node

The node daemon swiftletd MUST run on each node (or per-pod) and MUST launch and manage Cloud Hypervisor for guests scheduled there. swiftletd MUST communicate with Cloud Hypervisor via its API (e.g., local Unix sockets).

#### Scenario: swiftletd drives Cloud Hypervisor

- **WHEN** a guest is scheduled to a node
- **THEN** swiftletd receives the runtime intent and invokes Cloud Hypervisor to create and manage the VM

#### Scenario: No multi-hypervisor abstraction

- **WHEN** KubeSwift runs a VM
- **THEN** only Cloud Hypervisor is supported; there MUST NOT be a generic multi-hypervisor abstraction layer

### Requirement: Cloud-init via datasource media only

KubeSwift MUST support cloud-init compatibility by generating and delivering datasource media (e.g., NoCloud, ConfigDrive). KubeSwift MUST NOT reimplement cloud-init; the guest’s cloud-init runs against the provided media.

#### Scenario: Seed media generated from SwiftSeedProfile

- **WHEN** a SwiftGuest references a SwiftSeedProfile
- **THEN** KubeSwift generates datasource media (e.g., NoCloud ISO or directory) and makes it available to the guest

#### Scenario: No cloud-init reimplementation

- **WHEN** guest initialization is performed
- **THEN** KubeSwift provides media only; it does not parse or execute cloud-init user-data or vendor-data

### Requirement: Image and initialization as separate subsystems

Image import/preparation (SwiftImage) and guest initialization (SwiftSeedProfile, seed media) MUST be separate subsystems. SwiftImage MUST be immutable once ready. Seed media MUST be generated per-guest from SwiftSeedProfile.

#### Scenario: SwiftImage immutable when ready

- **WHEN** a SwiftImage reaches Ready status
- **THEN** its content MUST NOT be mutated in place

#### Scenario: Seed generation independent of image

- **WHEN** a SwiftGuest is created
- **THEN** seed media is generated from SwiftSeedProfile independently of the SwiftImage content

### Requirement: Single monorepo at github.com/projectbeskar/kubeswift

All KubeSwift implementation MUST reside in a single monorepo at github.com/projectbeskar/kubeswift. Repository paths, package names, binaries, and docs MUST be consistent with this repository.

#### Scenario: All code in one repository

- **WHEN** control plane or node runtime code is added
- **THEN** it is committed under github.com/projectbeskar/kubeswift

#### Scenario: Paths reference monorepo layout

- **WHEN** design or tasks reference file paths
- **THEN** paths are repository-relative within github.com/projectbeskar/kubeswift (e.g., pkg/apis/swift/, swiftletd/src/)

