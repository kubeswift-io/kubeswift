## 1. Prerequisites documentation

- [x] 1.1 Add docs/smoke-verification.md with exact local cluster prerequisites (CRDs, controllers, swiftletd image, RBAC, node requirements)
- [x] 1.2 Document RBAC apply command: `kubectl apply -k config/rbac/ -n <namespace>`
- [x] 1.3 Document verification commands for each prerequisite (e.g., kubectl get crd, kubectl get pods -n kubeswift-system)
- [x] 1.4 Update docs/first-boot.md to reference smoke-verification doc for prerequisites

## 2. SwiftImage verification

- [x] 2.1 Extend test/smoke/boot-test.sh to apply RBAC before SwiftGuest (config/rbac/ in target namespace)
- [x] 2.2 Add explicit SwiftImage Ready verification with configurable timeout (--timeout-image)
- [x] 2.3 On SwiftImage timeout: output `kubectl describe swiftimage` before exit
- [x] 2.4 Document SwiftImage failure checks and common causes in docs/smoke-verification.md

## 3. SwiftGuest scheduling verification

- [x] 3.1 Add verification that SwiftGuest pod is created and scheduled (not Pending indefinitely)
- [x] 3.2 On scheduling failure: output `kubectl describe pod` and pod events before exit
- [x] 3.3 Document scheduling failure checks in docs/smoke-verification.md

## 4. Seed rendering and mount path verification

- [x] 4.1 Add verification that seed ConfigMap exists when SwiftGuest has seedProfileRef
- [x] 4.2 Add verification that pod spec includes seed volume mount at /var/lib/kubeswift/seed
- [x] 4.3 Document seed verification and runtime intent alignment in docs/smoke-verification.md

## 5. swiftletd and Cloud Hypervisor verification

- [x] 5.1 Add verification that launcher container (swiftletd) is running and has not exited with error
- [x] 5.2 On swiftletd failure: output `kubectl logs <pod> -c launcher` before exit
- [x] 5.3 Document swiftletd failure checks in docs/smoke-verification.md

## 6. SwiftGuest Running and status conditions

- [x] 6.1 Add explicit verification of status.phase=Running and GuestRunning condition True
- [x] 6.2 Add assertion or log of Resolved, PodScheduled, GuestRunning conditions on success
- [x] 6.3 On Running timeout: output `kubectl describe swiftguest` and launcher pod logs before exit
- [x] 6.4 Document status condition checks in docs/smoke-verification.md

## 7. Failure checks documentation

- [x] 7.1 Document exact `kubectl describe` and `kubectl logs` commands for each failure mode
- [x] 7.2 Document common failure causes (image URL unreachable, RBAC missing, node without CH) and remediation
- [x] 7.3 Ensure config/samples/README.md references smoke-verification doc
