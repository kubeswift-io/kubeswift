# seed-rendering-nocloud Specification

## Purpose

KubeSwift delivers cloud-init-compatible NoCloud datasource media without reimplementing cloud-init. The control plane resolves user-data, meta-data, and network-config from SwiftSeedProfile (inline or Secret/ConfigMap refs), creates a text-based ConfigMap, and the node runtime (swiftletd/swift-seed) builds local NoCloud media from it.

## Requirements

### Requirement: Resolve user-data, meta-data, network-data from SwiftGuest and SwiftSeedProfile

The control plane MUST resolve user-data, meta-data, and network-data from SwiftGuest and optional SwiftSeedProfile. When SwiftSeedProfile is referenced, its userData, metaData, and networkData (or refs) are used. When not referenced, no seed is produced.

#### Scenario: UserData from SwiftSeedProfile

- **WHEN** SwiftGuest references SwiftSeedProfile with userData "#!/bin/bash\necho hello"
- **THEN** the resolved user-data is "#!/bin/bash\necho hello"

#### Scenario: MetaData from SwiftSeedProfile

- **WHEN** SwiftSeedProfile specifies metaData "instance-id: guest-1"
- **THEN** the resolved meta-data is "instance-id: guest-1"

#### Scenario: No seed when no profile

- **WHEN** SwiftGuest does not reference SwiftSeedProfile
- **THEN** no seed ConfigMap is created; no user-data, meta-data, or network-data are resolved

### Requirement: Support Secret and ConfigMap references

The control plane MUST support Secret and ConfigMap references for userData, metaData, and networkData where designed. When a ref is specified, the controller fetches the Secret or ConfigMap and extracts the value by key.

#### Scenario: UserData from Secret

- **WHEN** SwiftSeedProfile specifies userData as secretKeyRef (name, key)
- **THEN** the controller fetches the Secret and uses the value at the specified key as user-data

#### Scenario: UserData from ConfigMap

- **WHEN** SwiftSeedProfile specifies userData as configMapKeyRef (name, key)
- **THEN** the controller fetches the ConfigMap and uses the value at the specified key as user-data

#### Scenario: Ref resolution failure

- **WHEN** the referenced Secret or ConfigMap does not exist or the key is missing
- **THEN** resolution fails; the controller sets a condition or does not create the seed ConfigMap

### Requirement: Create Kubernetes artifact for node runtime

The control plane MUST create a Kubernetes artifact (ConfigMap) that the node runtime can consume to build local seed media. The ConfigMap MUST contain text-based keys (user-data, meta-data, network-config).

#### Scenario: ConfigMap created when seed present

- **WHEN** ResolvedGuest has Seed with userData (and optionally metaData, networkData)
- **THEN** the controller creates a ConfigMap with keys user-data, meta-data, network-config (for non-empty values)

#### Scenario: ConfigMap mounted into pod

- **WHEN** the pod envelope is created for SwiftGuest
- **THEN** the seed ConfigMap is mounted as a volume at a well-known path (e.g., /var/lib/kubeswift/seed/<guest>/)

#### Scenario: Node runtime can read ConfigMap

- **WHEN** swiftletd or rust/swift-seed runs on the node
- **THEN** it can read the ConfigMap contents from the mounted path

### Requirement: Artifact is text-based

The generated artifact MUST be text-based. ISO blobs MUST NOT be stored in the API server (ConfigMap, Secret, or CRD).

#### Scenario: ConfigMap contains text only

- **WHEN** the seed ConfigMap is created
- **THEN** its data values are strings (text); no binary ISO content

#### Scenario: No ISO in etcd

- **WHEN** seed rendering completes
- **THEN** no ISO image is stored in any Kubernetes resource; ISO generation happens on the node

### Requirement: KubeSwift provides datasource delivery, not cloud-init

KubeSwift MUST provide cloud-init-compatible datasource delivery. KubeSwift MUST NOT reimplement cloud-init (no parsing, validation, or execution of user-data).

#### Scenario: KubeSwift delivers media only

- **WHEN** KubeSwift creates seed media
- **THEN** it delivers the bytes (user-data, meta-data, network-config) in the format cloud-init expects; it does not interpret or execute the content

#### Scenario: Guest cloud-init processes media

- **WHEN** the VM boots
- **THEN** the guest's cloud-init discovers the NoCloud media and processes it; KubeSwift does not run cloud-init

### Requirement: Local seed media generation in swiftletd or Rust helper

Local seed media generation (building NoCloud directory or ISO from ConfigMap) MUST be performed by swiftletd or a Rust helper (rust/swift-seed). The control-plane controller MUST NOT build the NoCloud directory or ISO.

#### Scenario: Control plane does not build ISO

- **WHEN** the SwiftGuest controller creates the seed ConfigMap
- **THEN** it does NOT create an ISO or NoCloud directory; it only creates the ConfigMap with text

#### Scenario: Node runtime builds seed media

- **WHEN** swiftletd prepares to launch a VM
- **THEN** it (or rust/swift-seed) reads the ConfigMap mount and builds the NoCloud directory or ISO locally

### Requirement: Templating simple and deterministic

If templating is supported, it MUST be simple and deterministic. No arbitrary code execution. For MVP, pass-through (no templating) is acceptable.

#### Scenario: Pass-through when no templating

- **WHEN** SwiftSeedProfile provides userData as inline string
- **THEN** the content is used verbatim; no variable substitution

#### Scenario: Deterministic when templating present

- **WHEN** templating is implemented (future)
- **THEN** only a fixed set of variables (e.g., instance-id, hostname) is supported; output is deterministic for same input

### Requirement: Package and file paths fit repository

Seed rendering logic MUST reside in paths that fit github.com/projectbeskar/kubeswift. Control-plane code in internal/seed/ or internal/controller/; node runtime in rust/swift-seed/.

#### Scenario: Control plane seed code in internal/

- **WHEN** the repository is inspected
- **THEN** seed resolution and ConfigMap creation logic resides under internal/seed/ or internal/controller/swiftguest/ in github.com/projectbeskar/kubeswift

#### Scenario: Node runtime seed code in rust/

- **WHEN** the repository is inspected
- **THEN** NoCloud media building logic resides under rust/swift-seed/ in github.com/projectbeskar/kubeswift
