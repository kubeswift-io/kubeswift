## Why

SwiftImage CRDs exist but have no controller to drive their lifecycle. Images must progress from source (http, upload, pvcClone) through import, validation, preparation, and finally Ready—at which point they become immutable and expose a runtime-ready artifact reference. Without a controller, SwiftImage resources never leave Pending and SwiftGuest reconciliation cannot resolve image references. This change implements the first SwiftImage controller to reconcile the full lifecycle, enforce explicit format, prepare runtime-ready artifacts, and make SwiftImage immutable once Ready—aligned with the Cloud Hypervisor–native design (raw preferred, no format guessing).

## What Changes

- Implement SwiftImage controller in internal/controller/swiftimage/
- Reconcile lifecycle through Pending, Importing, Validating, Preparing, Ready, Failed
- Support source types: http, upload (placeholder), pvcClone
- Enforce explicit image format (no guessing)
- Add prepared storage reference to SwiftImage status
- Enforce immutability once Ready (reject spec mutations via webhook or controller)
- Add conditions for each phase and failure reasons
- Design format conversion as a pluggable step (stub allowed for MVP)

**Intentionally excluded:**

- Full image upload UX
- Node-local image caching implementation

## Capabilities

### New Capabilities

- `swiftimage-controller`: SwiftImage controller implementation; lifecycle phases (Pending, Importing, Validating, Preparing, Ready, Failed); source types (http, upload placeholder, pvcClone); explicit format enforcement; prepared storage reference in status; immutability once Ready; format conversion as pluggable step (stub allowed).

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: internal/controller/swiftimage/, api/image/v1alpha1/ (status fields if extended)
- **API group**: image.kubeswift.io
- **Prerequisites**: add-core-kubeswift-api-types (API types, CRDs)
- **Dependencies**: controller-runtime, client-go
- **Risks**: Image import can be long-running; design must support async/job-based import
- **Rollback**: Disable controller; existing Ready SwiftImages remain usable
