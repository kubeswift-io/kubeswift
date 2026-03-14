## 1. Webhook infrastructure

**Prerequisite:** add-core-kubeswift-api-types (API types, CRDs) must be complete.

- [x] 1.1 Add internal/webhook/suite.go with webhook server setup and handler registration
- [x] 1.2 Add scheme registration for swift, image, seed API types in webhook suite
- [x] 1.3 Add internal/webhook/swiftguest/, internal/webhook/swiftimage/, internal/webhook/swiftseedprofile/ directories
- [x] 1.4 Wire webhook handlers into cmd/webhook-server/ or cmd/controller-manager/ main

## 2. SwiftGuest validation

- [x] 2.1 Add internal/webhook/swiftguest/validator.go with ValidateCreate and ValidateUpdate
- [x] 2.2 Implement exactly-one-boot-source validation (reject multiple, require imageRef)
- [x] 2.3 Implement runPolicy enum validation (Running, Stopped only)
- [x] 2.4 Implement required refs validation (imageRef, guestClassRef)
- [ ] 2.5 Implement memory hotplug maxGuest >= guest memory (when both in request; otherwise document reconcile-time)

## 3. SwiftGuest defaulting

- [x] 3.1 Add internal/webhook/swiftguest/defaulter.go with Default and DefaultCreate
- [x] 3.2 Implement runPolicy default to Running when omitted
- [ ] 3.3 Implement architecture default to x86_64 when field exists and omitted
- [ ] 3.4 Implement firmware, bus, interface model, shutdown method defaults when fields exist

## 4. SwiftImage validation

- [x] 4.1 Add internal/webhook/swiftimage/validator.go with ValidateCreate and ValidateUpdate
- [x] 4.2 Implement image source exactly-one-type validation (URL xor PVC)
- [x] 4.3 Implement format explicit validation (reject empty or missing format)
- [x] 4.4 Add SwiftImage immutable-when-Ready check (reject spec mutation if status.Ready)

## 5. SwiftImage defaulting

- [x] 5.1 Add internal/webhook/swiftimage/defaulter.go
- [x] 5.2 Implement format default to raw when omitted (if API allows)

## 6. SwiftSeedProfile validation

- [x] 6.1 Add internal/webhook/swiftseedprofile/validator.go with ValidateCreate and ValidateUpdate
- [x] 6.2 Implement NoCloud unsupported-combinations validation
- [x] 6.3 Implement userData required when datasource is NoCloud
- [x] 6.4 Implement datasource NoCloud-only for MVP (reject ConfigDrive, Ignition, Unattend)

## 7. SwiftSeedProfile defaulting

- [x] 7.1 Add internal/webhook/swiftseedprofile/defaulter.go
- [x] 7.2 Implement datasource default to NoCloud when omitted

## 8. Webhook configuration

- [ ] 8.1 Add config/webhook/ with ValidatingWebhookConfiguration for SwiftGuest, SwiftImage, SwiftSeedProfile
- [ ] 8.2 Add config/webhook/ with MutatingWebhookConfiguration for same resources
- [ ] 8.3 Add certificate generation or volume mount for webhook TLS (or document manual setup)
- [ ] 8.4 Add config/webhook/kustomization.yaml or patch for webhook configs

## 9. Tests and documentation

- [ ] 9.1 Add unit tests for SwiftGuest validator (each validation rule)
- [ ] 9.2 Add unit tests for SwiftImage and SwiftSeedProfile validators
- [ ] 9.3 Add unit tests for defaulters
- [ ] 9.4 Add docs/webhook-validation.md documenting admission vs reconcile enforcement
