# swiftimage-controller Specification

## Purpose

The SwiftImage controller reconciles SwiftImage resources through lifecycle phases from source (http, upload, pvcClone) to Ready. When Ready, the image is immutable and exposes a prepared artifact reference for consumers (e.g., SwiftGuest resolver).

## Requirements

### Requirement: SwiftImage lifecycle phases

The SwiftImage controller MUST reconcile SwiftImage through phases: Pending, Importing, Validating, Preparing, Ready, Failed. Status.phase MUST reflect the current phase.

#### Scenario: New SwiftImage starts in Pending

- **WHEN** a SwiftImage is created
- **THEN** status.phase is Pending (or unset, treated as Pending)

#### Scenario: Phase transitions to Importing

- **WHEN** the controller begins fetching or copying from source (http, pvcClone)
- **THEN** status.phase is Importing

#### Scenario: Phase transitions to Validating

- **WHEN** import completes and validation begins
- **THEN** status.phase is Validating

#### Scenario: Phase transitions to Preparing

- **WHEN** validation passes and preparation (format conversion if needed) begins
- **THEN** status.phase is Preparing

#### Scenario: Phase transitions to Ready

- **WHEN** preparation completes and the artifact is runtime-ready
- **THEN** status.phase is Ready and preparedArtifact is set

#### Scenario: Phase transitions to Failed

- **WHEN** import, validation, or preparation fails
- **THEN** status.phase is Failed and a condition carries the failure reason

### Requirement: Source types http, upload placeholder, pvcClone

The controller MUST support source types: http, upload (placeholder), pvcClone. Exactly one source type MUST be specified per SwiftImage.

#### Scenario: HTTP source

- **WHEN** SwiftImage specifies source.http with a URL
- **THEN** the controller fetches from the URL and progresses through lifecycle

#### Scenario: Upload placeholder

- **WHEN** SwiftImage specifies source.upload (placeholder)
- **THEN** the controller accepts the spec and sets a condition indicating upload is not yet implemented (or waits for future upload completion)

#### Scenario: PVC clone source

- **WHEN** SwiftImage specifies source.pvcClone with a source PVC reference
- **THEN** the controller clones from the source PVC and progresses through lifecycle

#### Scenario: Exactly one source type

- **WHEN** SwiftImage specifies multiple or zero source types
- **THEN** the request is rejected (webhook) or the controller sets Failed with reason

### Requirement: Explicit image format

The controller MUST use spec.format only. It MUST NOT guess or infer format from URL, file extension, or content. Format MUST be explicitly declared (raw or qcow2).

#### Scenario: Format from spec

- **WHEN** the controller prepares an image
- **THEN** it uses spec.format only; it does not inspect file content

#### Scenario: No format guessing

- **WHEN** spec.format is empty or invalid
- **THEN** the controller does not proceed; it sets Failed or rejects (webhook)

### Requirement: Prepared storage reference in status

When SwiftImage reaches Ready, status MUST include a prepared artifact reference (pvcRef, format, size) that consumers can use to mount the runtime-ready image.

#### Scenario: PreparedArtifact set when Ready

- **WHEN** status.phase is Ready
- **THEN** status.preparedArtifact is set with pvcRef (or equivalent), format, and size

#### Scenario: PreparedArtifact usable by resolver

- **WHEN** SwiftGuest resolver looks up a Ready SwiftImage
- **THEN** it can use preparedArtifact to obtain the PVC reference for mounting

### Requirement: SwiftImage immutable once Ready

When SwiftImage status.phase is Ready, spec mutations MUST be rejected. The spec MUST NOT be modified after the image is Ready.

#### Scenario: Spec mutation rejected when Ready

- **WHEN** a user attempts to update SwiftImage spec when status.phase is Ready
- **THEN** the update is rejected (webhook or controller) with an error indicating immutability

#### Scenario: Status updates allowed when Ready

- **WHEN** only status is updated (e.g., conditions) while phase remains Ready
- **THEN** the update is allowed

### Requirement: Runtime-preferred format raw

The runtime-preferred format MUST be raw. When spec.format is qcow2, the Preparing phase converts to raw (or stubs conversion). When spec.format is raw, no conversion is needed.

#### Scenario: Raw format no conversion

- **WHEN** spec.format is raw and source is already raw
- **THEN** Preparing phase does not perform conversion; artifact is used as-is

#### Scenario: Qcow2 to raw conversion path

- **WHEN** spec.format is qcow2
- **THEN** the design provides a clean path for conversion to raw in Preparing; implementation may stub conversion for MVP

### Requirement: Format conversion pluggable

Format conversion MUST be implemented as a pluggable step. The converter interface MUST be injectable. A stub implementation is allowed for MVP; the design MUST leave a clean path for real conversion.

#### Scenario: Converter interface exists

- **WHEN** the controller performs preparation
- **THEN** it calls an ImageConverter interface (or equivalent) rather than inline conversion logic

#### Scenario: Stub returns error for unsupported conversion

- **WHEN** conversion is required (e.g., qcow2 to raw) and only stub is implemented
- **THEN** the stub returns an error; the controller sets Failed with a clear reason

### Requirement: Status exposes conditions

SwiftImage status MUST expose conditions for each phase and failure reasons. Conditions MUST be clear and actionable.

#### Scenario: Ready condition when phase Ready

- **WHEN** status.phase is Ready
- **THEN** a Ready condition is True

#### Scenario: Failed condition with reason

- **WHEN** status.phase is Failed
- **THEN** a condition carries the failure reason (e.g., "ImportFailed", "ValidationFailed")

### Requirement: Controller package placement

The SwiftImage controller MUST reside in internal/controller/swiftimage/ in github.com/projectbeskar/kubeswift. Paths MUST be repository-relative.

#### Scenario: Controller in internal/controller/swiftimage

- **WHEN** the repository is inspected
- **THEN** the SwiftImage controller code resides under internal/controller/swiftimage/

#### Scenario: Paths repository-relative

- **WHEN** imports or file paths are referenced
- **THEN** they are relative to github.com/projectbeskar/kubeswift
