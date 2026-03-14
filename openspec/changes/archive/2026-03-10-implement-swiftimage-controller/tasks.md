## 1. API status and source extensions

**Prerequisite:** add-core-kubeswift-api-types (API types, CRDs) must be complete.

- [x] 1.1 Add SwiftImagePhase type (Pending, Importing, Validating, Preparing, Ready, Failed) to api/image/v1alpha1/
- [x] 1.2 Add PreparedArtifactRef to SwiftImageStatus (pvcRef, format, size)
- [x] 1.3 Extend ImageSource with HTTP, Upload, PVCClone source types (or add if not present)
- [x] 1.4 Add HTTPSource, UploadSource, PVCCloneSource structs
- [x] 1.5 Regenerate CRDs with controller-gen
- [x] 1.6 Update SwiftImageStatus to include phase and preparedArtifact

## 2. Controller scaffold

- [x] 2.1 Add internal/controller/swiftimage/controller.go with Reconcile function
- [x] 2.2 Implement phase dispatch: route to Pending, Importing, Validating, Preparing, Ready, Failed handlers
- [x] 2.3 Add status update helpers in internal/controller/swiftimage/status.go
- [x] 2.4 Register SwiftImage controller with controller-manager in cmd/controller-manager/
- [x] 2.5 Add watch for SwiftImage and owned Jobs/Pods

## 3. Pending and Importing phases

- [x] 3.1 Add internal/controller/swiftimage/import.go
- [x] 3.2 Implement http source: create Job to fetch URL to PVC
- [ ] 3.3 Implement pvcClone source: create Job or use CSI clone to produce PVC
- [x] 3.4 Implement upload placeholder: set condition "upload not implemented", remain in Pending
- [x] 3.5 Add Job template for http import (curl/wget or custom image)
- [x] 3.6 Transition to Importing when Job created; watch Job completion
- [x] 3.7 On Job success, transition to Validating; on failure, transition to Failed

## 4. Validating and Preparing phases

- [x] 4.1 Add internal/controller/swiftimage/validate.go
- [x] 4.2 Implement validation step: verify image exists, size (use spec.format only; no guessing)
- [x] 4.3 Add internal/controller/swiftimage/prepare.go
- [x] 4.4 Add ImageConverter interface in internal/controller/swiftimage/converter.go
- [x] 4.5 Implement stub converter: pass-through when source format == target format; error for conversion
- [x] 4.6 Call converter in Preparing phase; target format raw when spec.format is qcow2
- [x] 4.7 When spec.format is raw, skip conversion; use artifact as-is
- [x] 4.8 On preparation success, set preparedArtifact and transition to Ready

## 5. Ready and Failed handling

- [x] 5.1 Set status.preparedArtifact (pvcRef, format, size) when transitioning to Ready
- [x] 5.2 Set Ready condition True when phase is Ready
- [x] 5.3 Set Failed condition with reason when transitioning to Failed
- [x] 5.4 Add immutability check: reject spec update when phase is Ready (in controller or document for webhook)

## 6. Immutability enforcement

- [x] 6.1 Add webhook validation (or controller check): reject spec mutation when status.phase is Ready
- [x] 6.2 Document in add-api-validation-and-defaulting or implement in SwiftImage webhook
- [x] 6.3 Allow status-only updates when Ready

## 7. Sample YAML and tests

- [x] 7.1 Add config/samples/swiftimage-http.yaml with http source and format raw
- [x] 7.2 Add config/samples/swiftimage-pvc-clone.yaml with pvcClone source
- [x] 7.3 Add config/samples/swiftimage-upload-placeholder.yaml
- [x] 7.4 Add unit tests for phase transitions
- [x] 7.5 Add unit tests for converter stub (pass-through, conversion error)
- [ ] 7.6 Add integration test or e2e placeholder for http import (optional)
