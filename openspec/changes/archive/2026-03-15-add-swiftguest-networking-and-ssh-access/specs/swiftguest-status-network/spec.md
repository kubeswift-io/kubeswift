# swiftguest-status-network Specification

## ADDED Requirements

### Requirement: SwiftGuest status exposes network information

SwiftGuest status MUST include a network block (or equivalent) that exposes the guest's primary IP address and interface when available. This enables operators to discover how to connect to the guest (e.g., for SSH).

#### Scenario: status.network populated when guest has IP

- **WHEN** the guest VM has obtained an IP address (e.g., via DHCP)
- **THEN** SwiftGuest status includes `status.network.primaryIP` (or equivalent field) with that IP

#### Scenario: status.network.ready indicates IP discovery

- **WHEN** the guest IP has been discovered and reflected in status
- **THEN** status.network includes a ready flag or condition so operators know when to attempt SSH

#### Scenario: status.network empty when no network or IP unknown

- **WHEN** networking is disabled or the guest has not yet obtained an IP
- **THEN** status.network is empty or indicates not ready

### Requirement: Guest IP discovered from runtime

The guest's IP MUST be discovered from the node runtime (e.g., DHCP lease file, or other mechanism) and reported to the control plane for inclusion in SwiftGuest status.

#### Scenario: DHCP lease used for IP discovery

- **WHEN** a DHCP server in the pod assigns an IP to the VM
- **THEN** the system reads the DHCP lease (e.g., from dnsmasq lease file) and extracts the VM's IP

#### Scenario: Status updated when IP discovered

- **WHEN** the VM's IP is discovered
- **THEN** SwiftGuest status is updated (by the controller or node runtime) with the primary IP
