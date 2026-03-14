## Why

The add-launcher-pod-and-runtime-intent change introduced runtime intent types, serialization, mount-path alignment, and seed rendering—but the SwiftGuest reconcile loop stops before creating the pod envelope and intent ConfigMap. SwiftGuest cannot progress from spec to running guest until the controller creates the pod, mounts prepared image and seed artifacts, hands off runtime intent to the node, and maps pod scheduling state into SwiftGuest status. This change completes that reconcile path.

## What Changes

- Implement the SwiftGuest controller reconcile loop end-to-end
- Create the guest pod envelope (one pod per SwiftGuest) with image volume, seed volume (when present), and intent ConfigMap volume
- Create and manage the runtime intent ConfigMap (serialized JSON at well-known key)
- Map pod scheduling and phase (Pending, Running, Failed) into SwiftGuest status and conditions
- Set Resolved condition on resolution success/failure
- Add pod watch (Owns) so status updates on pod changes
- Use existing internal/runtimeintent package; align with add-launcher-pod-and-runtime-intent design

**Intentionally excluded:**

- Live migration, snapshots
- Windows support
- Generic hypervisor abstractions
- swiftletd implementation (consumes intent; out of scope)

## Capabilities

### New Capabilities

- `swiftguest-controller-reconcile`: SwiftGuest controller creates pod envelope, intent ConfigMap, maps pod state to status; resolver, SwiftImage readiness, and seed rendering are upstream dependencies.

### Modified Capabilities

- *(none)*

## Impact

- **Repository**: github.com/projectbeskar/kubeswift
- **Paths**: internal/controller/swiftguest/, internal/runtimeintent/ (use existing; add hash if needed)
- **Prerequisites**: add-swiftguest-resolver, implement-seed-rendering-for-nocloud, implement-swiftimage-controller, add-launcher-pod-and-runtime-intent (runtime intent types, mount paths)
- **Dependencies**: ResolvedGuest, seed ConfigMap, SwiftImage preparedArtifact.pvcRef
- **Risks**: Pod spec must align with swiftletd expectations; runtime intent format is contract
- **Rollback**: Revert controller logic; delete pods and intent ConfigMaps; SwiftGuest status reverts
