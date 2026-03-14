## Prerequisites and blockers

**Prerequisites (must be implemented first):** add-core-kubeswift-api-types, add-swiftguest-resolver, implement-swiftimage-controller, implement-seed-rendering-for-nocloud, add-launcher-pod-and-runtime-intent, implement-swiftletd-mvp.

**Blocker:** Integration fixes (sections 1–2) must complete before samples and smoke test can verify end-to-end boot.

---

## 1. Controller: mount path alignment

**Package:** internal/controller/swiftguest/

- [x] 1.1 Add mount path constants in internal/controller/swiftguest/ (e.g., DisksRootPath, SeedPath, IntentPath) or internal/runtimeintent/
- [x] 1.2 Set image volume mount path to /var/lib/kubeswift/disks/root in pod spec builder
- [x] 1.3 Set seed ConfigMap mount path to /var/lib/kubeswift/seed in pod spec builder
- [x] 1.4 Set runtime intent ConfigMap mount path to /var/lib/kubeswift/intent with file runtime-intent.json

## 2. Controller: runtime intent format alignment

**Package:** internal/runtimeintent/

- [x] 2.1 Ensure RuntimeIntent includes rootDisk.path, rootDisk.format, seedPath, cpu, memory, lifecycle, guestId
- [x] 2.2 Set rootDisk.path to /var/lib/kubeswift/disks/root when building from ResolvedGuest
- [x] 2.3 Set seedPath to /var/lib/kubeswift/seed when ResolvedGuest has Seed; empty string when no seed
- [x] 2.4 Ensure JSON serialization uses sorted keys (deterministic) for hashability
- [x] 2.5 Add unit test: serialized intent parses correctly with expected field names and types

## 3. Controller: SwiftGuest status and SwiftImage preparedArtifact

**Packages:** internal/controller/swiftguest/, api/swift/v1alpha1/, internal/controller/swiftimage/

- [x] 3.1 Ensure SwiftGuest status supports GuestRunning condition (add to api/swift/v1alpha1/ if missing)
- [ ] 3.2 Ensure status mapping preserves GuestRunning when swiftletd patches it (controller must not overwrite)
- [ ] 3.3 Verify SwiftGuest controller creates PVC volume from SwiftImage status.preparedArtifact.pvcRef
- [ ] 3.4 Verify SwiftImage controller sets status.preparedArtifact.pvcRef when phase is Ready

## 4. Runtime: swiftletd path and intent parsing

**Package:** rust/swiftletd/

- [x] 4.1 Use intent path /var/lib/kubeswift/intent/runtime-intent.json (configurable or constant)
- [x] 4.2 Use seed path /var/lib/kubeswift/seed when seedPath in intent is non-empty
- [x] 4.3 Parse runtime intent JSON: rootDisk.path, rootDisk.format, seedPath, cpu, memory, lifecycle, guestId
- [x] 4.4 Handle missing or malformed intent: exit with clear error, do not launch Cloud Hypervisor
- [x] 4.5 Use rootDisk.path from intent as disk path for Cloud Hypervisor (no hardcoded path)

## 5. Samples

**Path:** config/samples/

- [x] 5.1 Add config/samples/swiftguestclass-default.yaml with cpu and memory defaults (if not exists from prior changes)
- [x] 5.2 Add config/samples/swiftimage-http.yaml: SwiftImage with source.http URL and format raw
- [x] 5.3 Add config/samples/swiftseedprofile-minimal.yaml: SwiftSeedProfile with minimal user-data (hostname, etc.)
- [x] 5.4 Add config/samples/swiftguest-sample.yaml: SwiftGuest with imageRef, seedProfileRef, guestClassRef
- [x] 5.5 Add config/samples/README.md: apply order (SwiftGuestClass, SwiftImage, SwiftSeedProfile, SwiftGuest), brief usage

## 6. Documentation

**Path:** docs/

- [x] 6.1 Add docs/first-boot.md with prerequisites (cluster, KubeSwift installed, Cloud Hypervisor on nodes)
- [x] 6.2 Document sample image URL and format (e.g., Ubuntu cloud image, raw)
- [x] 6.3 Document apply steps and expected timeline (image Ready, guest Running)
- [x] 6.4 Document verification: kubectl get swiftimage/swiftguest, kubectl describe, kubectl logs
- [x] 6.5 Add troubleshooting: image import failure, pod scheduling, swiftletd errors

## 7. Smoke test

**Path:** test/smoke/

- [x] 7.1 Add test/smoke/ directory
- [x] 7.2 Add test/smoke/boot-test.sh: apply config/samples/*.yaml in order
- [x] 7.3 Wait for SwiftImage status.phase Ready (timeout configurable, e.g., 15 min)
- [x] 7.4 Wait for SwiftGuest status.phase Running or GuestRunning=True (timeout e.g., 5 min)
- [x] 7.5 Assert conditions: ImageReady, PodScheduled, GuestRunning (or equivalent)
- [x] 7.6 Add cleanup: delete SwiftGuest, SwiftImage, SwiftSeedProfile, SwiftGuestClass
- [x] 7.7 Add Makefile target smoke-test that invokes boot-test.sh (or document run command)
- [x] 7.8 Document smoke test prerequisites and known flakiness in test/smoke/README.md or Makefile

## 8. Verification

- [ ] 8.1 Run manual verification: apply samples, observe SwiftImage Ready, SwiftGuest Running
- [ ] 8.2 Run make smoke-test (or test/smoke/boot-test.sh) and confirm pass
- [ ] 8.3 Verify kubectl describe swiftguest shows status.conditions
- [ ] 8.4 Verify kubectl logs for guest pod shows swiftletd or Cloud Hypervisor output
