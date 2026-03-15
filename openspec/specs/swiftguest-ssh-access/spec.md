# swiftguest-ssh-access Specification

## Purpose
TBD - created by archiving change add-swiftguest-networking-and-ssh-access. Update Purpose after archive.
## Requirements
### Requirement: SSH keys delivered via cloud-init ssh_authorized_keys

Operators MUST be able to provide SSH public keys to the guest via the cloud-init `ssh_authorized_keys` user key. The keys MUST be written to the configured user's `~/.ssh/authorized_keys` by cloud-init on first boot.

#### Scenario: ssh_authorized_keys in userData

- **WHEN** SwiftSeedProfile userData includes a user with `ssh_authorized_keys` (cloud-init format)
- **THEN** cloud-init writes those keys to the user's authorized_keys file and the operator can SSH with the matching private key

#### Scenario: ssh_authorized_keys from Secret or ConfigMap

- **WHEN** SwiftSeedProfile uses userDataFrom to reference a Secret or ConfigMap containing cloud-config with `ssh_authorized_keys`
- **THEN** the resolved userData is used and SSH keys are delivered to the guest

### Requirement: Operator workflow for SSH access

Operators MUST be able to create a SwiftGuest with SSH enabled, discover the guest IP, and connect via SSH. The workflow MUST be documented.

#### Scenario: Create guest with SSH-enabled seed profile

- **WHEN** an operator creates a SwiftGuest referencing a SwiftSeedProfile that includes `ssh_authorized_keys` in userData
- **THEN** the guest boots with SSH keys configured and is accessible via SSH once the guest has an IP

#### Scenario: Discover guest IP from SwiftGuest status

- **WHEN** the guest has obtained an IP (e.g., via DHCP)
- **THEN** the operator can read `status.network.primaryIP` (or equivalent) from the SwiftGuest to connect

#### Scenario: SSH connection succeeds

- **WHEN** the operator runs `ssh <user>@<primaryIP>` with the private key matching an authorized key
- **THEN** the SSH connection succeeds (subject to network reachability and guest firewall)

### Requirement: Documentation for SSH setup

KubeSwift MUST document how operators provide SSH keys (SwiftSeedProfile userData format), how to discover the guest IP (SwiftGuest status), and how to connect via SSH.

#### Scenario: Docs describe SSH key injection

- **WHEN** an operator reads the documentation
- **THEN** they understand how to add `ssh_authorized_keys` to SwiftSeedProfile userData or userDataFrom

#### Scenario: Docs describe IP discovery

- **WHEN** an operator reads the documentation
- **THEN** they understand how to obtain the guest IP from SwiftGuest status (e.g., `kubectl get swiftguest <name> -o jsonpath='{.status.network.primaryIP}'`)

