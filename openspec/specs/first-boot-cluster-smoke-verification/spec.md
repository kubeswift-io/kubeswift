# first-boot-cluster-smoke-verification Specification

## Purpose
TBD - created by archiving change add-first-boot-cluster-smoke-verification. Update Purpose after archive.
## Requirements
### Requirement: Document exact local cluster prerequisites

The smoke verification MUST document exact local cluster prerequisites for reproducible first-boot verification. Prerequisites MUST include:

- CRDs installed (swift.kubeswift.io, image.kubeswift.io, seed.kubeswift.io)
- KubeSwift controllers deployed and running
- swiftletd container image available (built or pulled)
- RBAC for swiftletd applied in target namespace (`kubectl apply -k config/rbac/ -n <namespace>`)
- At least one node capable of running guest pods (Cloud Hypervisor or swiftletd image with CH)
- Accessible image URL for SwiftImage source (e.g., Ubuntu cloud image)

#### Scenario: Prerequisites documented

- **WHEN** a developer reads the smoke verification docs
- **THEN** they find all prerequisites listed with exact commands to verify or satisfy each

#### Scenario: RBAC prerequisite explicit

- **WHEN** prerequisites are documented
- **THEN** RBAC for swiftletd (patch swiftguests/status) is explicitly listed with the apply command

### Requirement: Verify SwiftImage reaches Ready

The smoke verification MUST verify that SwiftImage reaches Ready phase. It MUST apply the SwiftImage manifest, wait for `status.phase=Ready`, and fail with diagnostic output if the timeout is exceeded.

#### Scenario: SwiftImage Ready

- **WHEN** the smoke test applies the SwiftImage manifest and waits
- **THEN** it succeeds when `status.phase` is `Ready`

#### Scenario: SwiftImage timeout

- **WHEN** SwiftImage does not reach Ready within the configured timeout
- **THEN** the smoke test fails and outputs `kubectl describe swiftimage` for diagnosis

### Requirement: Verify SwiftGuest scheduling

The smoke verification MUST verify that SwiftGuest scheduling succeeds. It MUST confirm that a pod is created for the SwiftGuest, the pod is scheduled to a node, and the pod is running (or progressing toward Running).

#### Scenario: Pod created and scheduled

- **WHEN** SwiftImage is Ready and SwiftGuest is applied
- **THEN** the smoke test verifies that a pod exists for the SwiftGuest and is scheduled (not Pending indefinitely)

#### Scenario: Scheduling failure

- **WHEN** the SwiftGuest pod remains Pending beyond a reasonable timeout
- **THEN** the smoke test fails and outputs `kubectl describe pod` and events for diagnosis

### Requirement: Verify seed rendering and mount path

The smoke verification MUST verify that seed rendering and mount path are correct when a SwiftGuest has a seed profile. It MUST confirm that the seed ConfigMap exists, the pod has the seed volume mounted at the expected path, and the runtime intent references the seed path.

#### Scenario: Seed ConfigMap and mount

- **WHEN** SwiftGuest has seedProfileRef and the pod is created
- **THEN** the smoke test verifies that the seed ConfigMap exists and the pod spec includes the seed volume mount at `/var/lib/kubeswift/seed`

#### Scenario: Runtime intent seed path

- **WHEN** swiftletd runs with seed
- **THEN** the runtime intent ConfigMap contains the correct seed path aligned with internal/controller/swiftguest constants

### Requirement: Verify swiftletd launches Cloud Hypervisor

The smoke verification MUST verify that swiftletd launches Cloud Hypervisor. It MUST confirm that the launcher pod runs swiftletd, swiftletd does not exit immediately with an error, and (where observable) Cloud Hypervisor is running.

#### Scenario: swiftletd runs

- **WHEN** the SwiftGuest pod is running
- **THEN** the launcher container (swiftletd) is running and has not exited with an error

#### Scenario: swiftletd failure

- **WHEN** swiftletd exits with an error before the VM is running
- **THEN** the smoke test fails and outputs `kubectl logs` for the launcher container

### Requirement: Verify SwiftGuest reaches Running with status conditions

The smoke verification MUST verify that SwiftGuest reaches Running phase with clear status conditions. It MUST confirm `status.phase=Running` and that the GuestRunning condition is True (or equivalent observable status).

#### Scenario: SwiftGuest Running

- **WHEN** the smoke test applies the SwiftGuest and waits
- **THEN** it succeeds when `status.phase` is `Running` and GuestRunning condition is True (or equivalent)

#### Scenario: Status conditions documented

- **WHEN** verification succeeds
- **THEN** the smoke test asserts or logs the expected conditions (Resolved, PodScheduled, GuestRunning)

#### Scenario: Running timeout

- **WHEN** SwiftGuest does not reach Running within the configured timeout
- **THEN** the smoke test fails and outputs `kubectl describe swiftguest` and `kubectl logs` for the launcher pod

### Requirement: Failure checks documented

The smoke verification MUST document exact failure checks for each verification stage. For each failure mode, the docs MUST include the command to run and expected output patterns for diagnosis.

#### Scenario: Failure check commands

- **WHEN** a verification stage fails
- **THEN** the documentation provides the exact `kubectl describe` and `kubectl logs` commands to run

#### Scenario: Common failure causes

- **WHEN** a developer encounters a failure
- **THEN** the documentation lists common causes (e.g., image URL unreachable, RBAC missing, node without CH) and remediation steps

