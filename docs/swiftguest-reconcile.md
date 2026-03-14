# SwiftGuest Controller Reconcile Flow

The SwiftGuest controller reconciles SwiftGuest resources by resolving references, rendering seed data, creating the pod envelope, and mapping pod state to status.

## Flow

1. **Resolve** – Resolver fetches SwiftGuestClass, SwiftImage, SwiftSeedProfile and produces ResolvedGuest. On failure: set `Resolved=False`, `phase=Failed`, return.

2. **Seed rendering** – When ResolvedGuest has Seed, render userData/metaData/networkData and create ConfigMap `<guest-name>-seed`.

3. **Intent ConfigMap** – Build RuntimeIntent from ResolvedGuest, serialize to JSON, create ConfigMap `<guest-name>-runtime-intent` with key `runtime-intent.json`.

4. **Pod creation** – Build pod with image volume (from SwiftImage preparedArtifact.pvcRef), seed volume (if present), intent ConfigMap volume. Create or update pod.

5. **Status mapping** – Map pod phase to SwiftGuest status:
   - Pod Pending (scheduling) → `phase=Scheduling`; PodScheduled=False
   - Pod Pending (unschedulable) → `phase=Pending`; PodScheduled=False with reason
   - Pod Running → `phase=Running`; PodScheduled=True; nodeName; podRef
   - Pod Failed → `phase=Failed`; PodScheduled=False with reason
   - Pod Succeeded → `phase=Stopped`; PodScheduled=True

## Conditions

- **Resolved** – True when resolution succeeded; False with reason when resolution failed.
- **PodScheduled** – True when pod is Running or Succeeded; False when Pending or Failed.

## Admission vs Reconcile

Validation (required refs, runPolicy enum) is enforced by admission webhooks. Cross-resource checks (SwiftImage Ready, reference existence) are enforced at reconcile time by the controller.
