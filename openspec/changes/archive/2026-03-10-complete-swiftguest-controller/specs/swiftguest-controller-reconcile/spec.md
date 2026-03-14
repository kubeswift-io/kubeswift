## ADDED Requirements

### Requirement: SwiftGuest controller creates pod envelope

The SwiftGuest controller MUST create a pod envelope for each SwiftGuest when resolution succeeds. The pod MUST mount the prepared image (PVC from SwiftImage preparedArtifact), seed ConfigMap (when ResolvedGuest has Seed), and runtime intent ConfigMap.

#### Scenario: Pod created when resolution succeeds

- **WHEN** Resolver.Resolve returns ResolvedGuest with PreparedImage.Ready true
- **THEN** the controller creates a Pod with image volume, seed volume (if HasSeed), and intent ConfigMap volume

#### Scenario: Pod has image volume from preparedArtifact

- **WHEN** SwiftImage has status.preparedArtifact.pvcRef
- **THEN** the pod includes a volume from that PVC mounted at the well-known disk path (e.g., /var/lib/kubeswift/disks/root)

#### Scenario: Pod has seed volume when ResolvedGuest has Seed

- **WHEN** ResolvedGuest.HasSeed returns true
- **THEN** the pod includes a volume from the seed ConfigMap mounted at the well-known seed path

#### Scenario: Pod has intent ConfigMap volume

- **WHEN** the controller creates the pod
- **THEN** the pod includes a volume from the runtime intent ConfigMap mounted at the well-known intent path

### Requirement: Runtime intent ConfigMap creation

The controller MUST create a ConfigMap containing the serialized runtime intent (JSON at key runtime-intent.json) before or when creating the pod. The ConfigMap MUST be owned by the SwiftGuest.

#### Scenario: Intent ConfigMap created

- **WHEN** resolution succeeds and ResolvedGuest is available
- **THEN** the controller creates or updates a ConfigMap with key runtime-intent.json and serialized RuntimeIntent content

#### Scenario: Intent uses canonical mount paths

- **WHEN** RuntimeIntent is built from ResolvedGuest
- **THEN** RootDisk.Path, SeedPath use the same paths as pod volume mounts (internal/runtimeintent constants)

### Requirement: SwiftGuest status reflects pod state

The controller MUST map pod phase and scheduling state into SwiftGuest status. Status MUST include phase (Pending, Scheduling, Running, Stopped, Failed), nodeName when scheduled, podRef, and conditions (Resolved, PodScheduled).

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

### Requirement: Resolution failure sets status

When Resolver.Resolve returns ResolutionError, the controller MUST set Resolved condition to False with the error reason, set phase to Failed, and MUST NOT create pod or intent ConfigMap.

#### Scenario: Resolution failure does not create pod

- **WHEN** Resolver.Resolve returns ResolutionError (e.g., SwiftImage not Ready)
- **THEN** the controller sets Resolved=False, phase=Failed, and does not create pod or intent ConfigMap

#### Scenario: Resolution failure reason preserved

- **WHEN** ResolutionError has Reason "SwiftImage not Ready"
- **THEN** SwiftGuest Resolved condition message includes that reason

### Requirement: Controller watches owned Pods

The SwiftGuest controller MUST register a watch for owned Pods so that pod status changes trigger reconciliation and status updates.

#### Scenario: Pod status change triggers reconcile

- **WHEN** an owned Pod's phase or conditions change
- **THEN** the controller reconciles the SwiftGuest and updates status from the pod

### Requirement: Package layout

Webhook and controller logic for SwiftGuest MUST reside under internal/controller/swiftguest/ in github.com/projectbeskar/kubeswift. Runtime intent types and serialization MUST reside under internal/runtimeintent/.

#### Scenario: Controller in internal/controller/swiftguest

- **WHEN** the repository is inspected
- **THEN** reconcile logic, pod builder, and status mapping reside under internal/controller/swiftguest/

#### Scenario: Runtime intent in internal/runtimeintent

- **WHEN** the repository is inspected
- **THEN** RuntimeIntent types, Build, and Serialize reside under internal/runtimeintent/
