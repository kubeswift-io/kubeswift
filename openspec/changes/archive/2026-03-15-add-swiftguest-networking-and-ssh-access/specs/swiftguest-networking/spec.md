# swiftguest-networking Specification

## ADDED Requirements

### Requirement: Guest VM attached to pod network via TAP

The guest VM MUST receive network connectivity by attaching a virtio-net device backed by a TAP interface in the pod's network namespace. The TAP MUST be connected to the pod network (e.g., via a bridge) so the guest can obtain an IP on the pod subnet.

#### Scenario: TAP created in pod network namespace

- **WHEN** the SwiftGuest pod is scheduled and network setup runs
- **THEN** a TAP device is created in the pod's network namespace and added to a bridge that includes the pod's primary interface

#### Scenario: Cloud Hypervisor receives TAP for VM

- **WHEN** swiftletd launches Cloud Hypervisor with network enabled
- **THEN** it passes `--net tap=<tap_name>` so the VM has a virtio-net interface backed by the TAP

#### Scenario: Guest obtains IP on pod subnet

- **WHEN** the VM boots and cloud-init configures the network (DHCP or static)
- **THEN** the guest receives an IP address on the same subnet as the pod and is reachable from the cluster network

### Requirement: Bridge and TAP setup before VM launch

The pod MUST run network setup (bridge creation, TAP creation, DHCP server if used) before the launcher spawns Cloud Hypervisor. This MAY be done by an init container or by swiftletd before CH launch.

#### Scenario: Network setup completes before CH launch

- **WHEN** the SwiftGuest pod starts
- **THEN** bridge and TAP are configured and (if DHCP) the DHCP server is running before swiftletd spawns Cloud Hypervisor

#### Scenario: Pod remains reachable after bridge setup

- **WHEN** the pod's primary interface is moved to the bridge
- **THEN** the pod retains its IP and remains reachable on the cluster network

### Requirement: Network-config delivered via seed for first-boot

When SwiftSeedProfile provides networkData (or networkDataFrom), the seed MUST include network-config so cloud-init can configure the guest's network interface. When networkData is absent but networking is enabled, KubeSwift MAY inject a default network-config (e.g., DHCP) suitable for the pod network.

#### Scenario: network-config from SwiftSeedProfile

- **WHEN** SwiftSeedProfile specifies networkData with DHCP or static configuration
- **THEN** the seed ConfigMap includes network-config and cloud-init applies it on first boot

#### Scenario: Default DHCP when networkData absent

- **WHEN** networking is enabled and SwiftSeedProfile has no networkData
- **THEN** the system MAY inject a minimal network-config instructing cloud-init to use DHCP on the primary interface
