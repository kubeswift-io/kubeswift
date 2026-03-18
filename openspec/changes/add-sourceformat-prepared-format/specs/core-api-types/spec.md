## ADDED Requirements

### Requirement: SwiftImageStatus format observability

SwiftImageStatus MUST include sourceFormat and preparedFormat (DiskFormat) for format observability. sourceFormat records the original input format (e.g. qcow2); preparedFormat records the runtime-ready format (always raw for Cloud Hypervisor).

#### Scenario: SwiftImageStatus has format fields

- **WHEN** the SwiftImage API type is inspected
- **THEN** SwiftImageStatus includes sourceFormat and preparedFormat as optional (omitempty) DiskFormat fields
