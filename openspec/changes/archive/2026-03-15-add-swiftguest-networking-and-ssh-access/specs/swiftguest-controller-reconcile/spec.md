# swiftguest-controller-reconcile Specification (Delta)

## MODIFIED Requirements

### Requirement: SwiftGuest status reflects pod state

The controller MUST map pod phase and scheduling state into SwiftGuest status. Status MUST include phase (Pending, Scheduling, Running, Stopped, Failed), nodeName when scheduled, podRef, and conditions (Resolved, PodScheduled). When guest network information is available (e.g., from pod annotations or node runtime report), status MUST include status.network with primaryIP and ready so operators can discover how to connect (e.g., for SSH).

#### Scenario: Pod Pending maps to Scheduling

- **WHEN** the pod is in phase Pending and not yet scheduled
- **THEN** SwiftGuest status.phase is Scheduling and PodScheduled condition is False

#### Scenario: Pod Running maps to Running

- **WHEN** the pod is in phase Running
- **THEN** SwiftGuest status.phase is Running, PodScheduled is True, nodeName and podRef are set

#### Scenario: Pod Failed maps to Failed

- **WHEN** the pod is in phase Failed
- **THEN** SwiftGuest status.phase is Failed and PodScheduled condition includes reason

#### Scenario: Pod Unschedulable has reason

- **WHEN** the pod has Unschedulable condition
- **THEN** SwiftGuest PodScheduled condition is False with reason and message from the pod

#### Scenario: Status includes network when guest IP discovered

- **WHEN** the guest VM has obtained an IP and that IP is reported (e.g., via pod annotation or status patch from node runtime)
- **THEN** the controller updates SwiftGuest status.network with primaryIP and ready so operators can discover the guest IP for SSH
