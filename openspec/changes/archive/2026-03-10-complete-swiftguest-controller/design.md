## Context

The SwiftGuest controller already has: Resolver (produces ResolvedGuest), seed rendering (creates ConfigMap), runtime intent types (internal/runtimeintent), mount path constants (internal/controller/swiftguest/constants.go), and pod volume helpers (AddVolumeMounts, AddSeedVolume). The reconcile loop invokes resolver and seed rendering but stops before creating the pod and intent ConfigMap. add-launcher-pod-and-runtime-intent defined the design; this change implements the remaining reconcile path.

**Constraints:** One pod per SwiftGuest; resolver, SwiftImage readiness, and seed rendering are upstream dependencies; repository paths in github.com/projectbeskar/kubeswift.

## Goals / Non-Goals

**Goals:**

- Implement the full SwiftGuest reconcile loop
- Create pod envelope with image volume (from preparedArtifact.pvcRef), seed volume (when present), intent ConfigMap volume
- Create and manage runtime intent ConfigMap (serialized JSON at key runtime-intent.json)
- Map pod phase (Pending, Running, Failed) into SwiftGuest status and conditions
- Set Resolved condition on resolution success/failure
- Add pod watch (Owns) so status updates on pod changes

**Non-Goals:**

- swiftletd implementation
- Live migration, snapshots, Windows support
- Generic hypervisor abstractions

## Decisions

### 1. Reuse existing runtimeintent package

internal/runtimeintent already has types.go, build.go, serialize.go, constants.go. Build(rg) produces RuntimeIntent from ResolvedGuest. Serialize produces deterministic JSON. No hash.go needed for MVP; add if intent-change detection is required later.

**Rationale:** add-launcher-pod-and-runtime-intent established the package; avoid duplication.

### 2. Pod spec builder in internal/controller/swiftguest/pod.go

Extend pod.go with BuildPod(guest, resolved, imagePVCName, seedConfigMapName, intentConfigMapName) *corev1.Pod. Use existing AddVolumeMounts, AddSeedVolume. Add volume for image PVC (from ResolvedGuest.PreparedImage; PVC name from SwiftImage status.preparedArtifact.pvcRef). Add volume for intent ConfigMap. Set resource requests/limits from ResolvedGuest.Resources. Set container (swiftletd or launcher placeholder) with volume mounts. Set ownerReference, pod name, labels.

**Rationale:** Single place for pod construction; aligns with add-launcher-pod-and-runtime-intent design.

### 3. Intent ConfigMap name and lifecycle

ConfigMap name: `<guest-name>-runtime-intent`. Key: `runtime-intent.json` (matches IntentFile). Controller creates ConfigMap before pod; pod mounts it. ConfigMap owned by SwiftGuest. On update, controller serializes new intent and updates ConfigMap; pod gets updated mount (or pod restart if needed).

**Rationale:** Well-known naming; garbage collection via ownerReference.

### 4. Status mapping: pod phase to SwiftGuest

| Pod Phase | SwiftGuest Condition | Status |
|-----------|----------------------|--------|
| Pending (scheduling) | PodScheduled=False | phase: Scheduling | 
| Pending (unschedulable) | PodScheduled=False, reason | phase: Pending |
| Running | PodScheduled=True | phase: Running |
| Failed | PodScheduled=False, reason | phase: Failed |
| Succeeded | (unusual) | phase: Stopped? |

Conditions: Resolved (from resolver), PodScheduled (from pod). GuestRunning deferred to swiftletd status.

**Rationale:** Matches add-launcher-pod-and-runtime-intent design; status.phase reflects user-visible state.

### 5. Mount paths and volume alignment

Use existing constants: DisksRootPath, SeedPath, IntentPath, IntentFile. Image volume mounts at DisksRootPath. Seed volume at SeedPath. Intent ConfigMap at IntentPath (file at IntentPath/runtime-intent.json). Runtime intent Build uses same paths; swiftletd expects them.

**Rationale:** internal/runtimeintent and rust/swiftletd already share these paths; constants.go ensures consistency.

### 6. Resolution failure handling

On resolution error (ResolutionError): set Resolved=False, reason from ResolutionError.Reason, phase=Failed. Do not create pod or intent ConfigMap. Return without error so controller does not retry indefinitely (resolution failure is user-fixable).

**Rationale:** User sees condition; no pod creation when dependencies are missing.

### 7. File layout

```
internal/controller/swiftguest/
├── controller.go   # Reconcile: resolve, seed, intent ConfigMap, pod, status
├── pod.go          # BuildPod (extend existing); AddVolumeMounts, AddSeedVolume
├── status.go       # MapPodToStatus, SetResolvedCondition, SetPodScheduledCondition
└── constants.go    # (existing)
```

internal/runtimeintent/ unchanged.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Runtime intent format change breaks swiftletd | Version field in intent if needed; swiftletd validates |
| Pod never schedules | Condition with reason; user sees Scheduling or Pending |
| Intent ConfigMap too large | Keep intent minimal; avoid embedding large data |
| preparedArtifact.pvcRef nil | Resolver fails before pod creation; ResolvedGuest.PreparedImage.Ready must be true |

## Migration Plan

1. Add status.go with MapPodToStatus, SetResolvedCondition, SetPodScheduledCondition
2. Extend pod.go with BuildPod, image volume, intent ConfigMap volume
3. Add intent ConfigMap creation in controller
4. Add pod creation/update in controller
5. Add Owns(&corev1.Pod{}) to SetupWithManager
6. Add resolution failure status handling
7. Deploy; create SwiftGuest; verify pod created and status updated

**Rollback:** Revert controller; delete pods and intent ConfigMaps; SwiftGuest status reverts.

## Open Questions

- Whether swiftletd runs inside the pod container or as DaemonSet—affects container image (placeholder vs real). MVP: use placeholder or minimal launcher that exits; swiftletd integration is separate.
- Exact container image for pod: placeholder until swiftletd image is ready.
