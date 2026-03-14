# boot-first-guest Specification

## Purpose
TBD - created by archiving change boot-first-kubeswift-guest. Update Purpose after archive.
## Requirements
### Requirement: Sample manifests apply without validation errors

Sample manifests for SwiftImage, SwiftSeedProfile, and SwiftGuest MUST be provided in config/samples/ in github.com/projectbeskar/kubeswift. An operator MUST be able to apply these manifests with kubectl apply without validation errors.

#### Scenario: Apply SwiftImage sample

- **WHEN** an operator runs `kubectl apply -f config/samples/swiftimage-http.yaml`
- **THEN** the SwiftImage is created and the command exits with success

#### Scenario: Apply SwiftSeedProfile and SwiftGuest samples

- **WHEN** an operator runs `kubectl apply -f config/samples/swiftseedprofile-minimal.yaml` and `kubectl apply -f config/samples/swiftguest-sample.yaml`
- **THEN** both resources are created and the commands exit with success

#### Scenario: SwiftGuest references resolve

- **WHEN** SwiftGuest is created with imageRef and seedProfileRef pointing to existing SwiftImage and SwiftSeedProfile
- **THEN** the resolver produces ResolvedGuest without resolution errors

### Requirement: SwiftImage reaches Ready with prepared artifact

The SwiftImage controller MUST drive the image lifecycle to status.phase Ready. When Ready, status.preparedArtifact MUST be set and MUST reference a PVC that the SwiftGuest controller can use for pod volume creation.

#### Scenario: Image progresses to Ready

- **WHEN** SwiftImage is created with a valid http source and format
- **THEN** status.phase transitions through Pending, Importing, Validating, Preparing to Ready

#### Scenario: Prepared artifact set when Ready

- **WHEN** status.phase is Ready
- **THEN** status.preparedArtifact.pvcRef is set and references a PVC in the same namespace

#### Scenario: SwiftGuest controller can use prepared artifact

- **WHEN** SwiftGuest references a SwiftImage with status.phase Ready
- **THEN** the SwiftGuest controller can create a pod volume from status.preparedArtifact.pvcRef

### Requirement: SwiftGuest pod is created and scheduled

The SwiftGuest controller MUST create exactly one Pod per SwiftGuest when resolution succeeds and SwiftImage is Ready. The pod MUST be schedulable and MUST have volume mounts for the root disk, seed ConfigMap (when SwiftSeedProfile referenced), and runtime intent.

#### Scenario: Pod created when resolved and image Ready

- **WHEN** SwiftGuest is resolved, SwiftImage status.phase is Ready, and SwiftGuest references SwiftSeedProfile
- **THEN** the controller creates a Pod with volumes for image PVC, seed ConfigMap, and runtime intent ConfigMap

#### Scenario: Pod scheduled when nodes available

- **WHEN** the pod is created and cluster has schedulable nodes
- **THEN** the pod is scheduled (spec.nodeName set) and reaches phase Running or ContainerCreating

#### Scenario: Mount paths align with node runtime expectations

- **WHEN** the pod is created
- **THEN** the volume mount paths for image, seed, and runtime intent match the paths the node runtime expects (alignment verified by successful boot)

### Requirement: Seed ConfigMap is created and mounted

When SwiftGuest references SwiftSeedProfile, the seed renderer MUST create a ConfigMap with keys user-data, meta-data, and network-config. The SwiftGuest controller MUST mount this ConfigMap into the guest pod. The node runtime MUST be able to generate NoCloud media from the mounted content.

#### Scenario: Seed ConfigMap created when profile referenced

- **WHEN** SwiftGuest has spec.seedProfileRef and resolution succeeds
- **THEN** a ConfigMap exists with keys user-data, meta-data, network-config (or equivalent NoCloud keys)

#### Scenario: Seed ConfigMap mounted in pod

- **WHEN** the guest pod is created and SwiftGuest references SwiftSeedProfile
- **THEN** the pod has a volume from the seed ConfigMap mounted at a path accessible to the node runtime

#### Scenario: NoCloud media generated from mounted seed

- **WHEN** the pod runs and the node runtime reads the mounted seed
- **THEN** NoCloud media is generated and the guest boots with cloud-init applied (observable via guest behavior or logs)

### Requirement: Cloud Hypervisor launches and VM runs

The node runtime MUST launch Cloud Hypervisor from the runtime intent. Cloud Hypervisor MUST start without immediate error. The VM MUST run (process does not exit immediately with non-zero status).

#### Scenario: Cloud Hypervisor process starts

- **WHEN** the guest pod container runs
- **THEN** the Cloud Hypervisor process is spawned and does not exit within a short window (e.g., 10 seconds) with an error

#### Scenario: VM configuration from runtime intent

- **WHEN** Cloud Hypervisor is launched
- **THEN** the VM uses root disk path, cpu, memory, and network config from the runtime intent

### Requirement: SwiftGuest status reflects Running

When the VM is running, SwiftGuest status MUST indicate the guest is running. The GuestRunning condition MUST be True when the node runtime has successfully launched the VM.

#### Scenario: Status indicates Running

- **WHEN** the VM is launched and running
- **THEN** SwiftGuest status.phase is Running (or equivalent) or a condition indicates running state

#### Scenario: GuestRunning condition True when VM running

- **WHEN** the node runtime reports VM running to the control plane
- **THEN** SwiftGuest status.conditions includes GuestRunning=True (or equivalent condition name)

### Requirement: Status conditions and logs are inspectable

An operator MUST be able to inspect SwiftGuest status conditions via kubectl. An operator MUST be able to view logs from the guest pod container.

#### Scenario: Status conditions visible via kubectl describe

- **WHEN** an operator runs `kubectl describe swiftguest <name>` (or equivalent for the SwiftGuest CRD)
- **THEN** status.conditions are visible (e.g., Resolved, PodScheduled, GuestRunning, ImageReady)

#### Scenario: Pod logs accessible

- **WHEN** an operator runs `kubectl logs <pod> -c <container>` for the guest pod
- **THEN** logs from the node runtime (or Cloud Hypervisor) are visible

### Requirement: Verification is documented and automatable

Verification steps MUST be documented in docs/first-boot.md in github.com/projectbeskar/kubeswift. A smoke test script MUST exist in test/smoke/ that automates verification.

#### Scenario: Manual verification documented

- **WHEN** an operator follows docs/first-boot.md
- **THEN** they can create SwiftImage, SwiftSeedProfile, SwiftGuest; wait for status.phase Ready (SwiftImage) and Running (SwiftGuest); and verify status conditions

#### Scenario: Smoke test applies samples and asserts success

- **WHEN** an operator runs the smoke test (e.g., `make smoke-test` or `test/smoke/boot-test.sh`)
- **THEN** the test applies sample manifests, waits for SwiftImage Ready and SwiftGuest Running (with configurable timeout), asserts status conditions, and reports pass or fail

#### Scenario: Smoke test cleans up

- **WHEN** the smoke test completes (pass or fail)
- **THEN** it deletes the created SwiftGuest, SwiftImage, and SwiftSeedProfile (or documents manual cleanup)

### Requirement: Artifacts reside in repository

All sample manifests, documentation, and test scripts MUST reside under github.com/projectbeskar/kubeswift. Paths MUST be consistent with the monorepo layout.

#### Scenario: Samples in config/samples

- **WHEN** the repository is inspected
- **THEN** config/samples/ contains swiftimage-http.yaml, swiftseedprofile-minimal.yaml, swiftguest-sample.yaml (or equivalent names)

#### Scenario: First-boot docs in docs

- **WHEN** the repository is inspected
- **THEN** docs/first-boot.md exists with first-boot workflow content

#### Scenario: Smoke test in test/smoke

- **WHEN** the repository is inspected
- **THEN** test/smoke/ contains a runnable smoke test script or Makefile target

