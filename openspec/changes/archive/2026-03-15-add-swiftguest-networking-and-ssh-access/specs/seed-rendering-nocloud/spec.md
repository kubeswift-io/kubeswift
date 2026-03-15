# seed-rendering-nocloud Specification (Delta)

## MODIFIED Requirements

### Requirement: Resolve user-data, meta-data, network-data from SwiftGuest and SwiftSeedProfile

The control plane MUST resolve user-data, meta-data, and network-data from SwiftGuest and optional SwiftSeedProfile. When SwiftSeedProfile is referenced, its userData, metaData, and networkData (or refs) are used. When not referenced, no seed is produced. UserData MAY contain cloud-init cloud-config with `ssh_authorized_keys` for SSH key injection; KubeSwift passes it through verbatim and does not interpret or validate cloud-config structure.

#### Scenario: UserData from SwiftSeedProfile

- **WHEN** SwiftGuest references SwiftSeedProfile with userData "#!/bin/bash\necho hello"
- **THEN** the resolved user-data is "#!/bin/bash\necho hello"

#### Scenario: MetaData from SwiftSeedProfile

- **WHEN** SwiftSeedProfile specifies metaData "instance-id: guest-1"
- **THEN** the resolved meta-data is "instance-id: guest-1"

#### Scenario: No seed when no profile

- **WHEN** SwiftGuest does not reference SwiftSeedProfile
- **THEN** no seed ConfigMap is created; no user-data, meta-data, or network-data are resolved

#### Scenario: ssh_authorized_keys in userData passed through

- **WHEN** SwiftSeedProfile userData contains cloud-config with a user and `ssh_authorized_keys` list
- **THEN** the resolved user-data includes that content verbatim; cloud-init in the guest will process it and write keys to ~/.ssh/authorized_keys
