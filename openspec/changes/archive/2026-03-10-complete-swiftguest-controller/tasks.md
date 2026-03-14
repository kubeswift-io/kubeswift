## 1. ResolvedGuest PVC reference

**Prerequisite:** add-swiftguest-resolver, implement-swiftimage-controller (preparedArtifact).

- [x] 1.1 Add PVCName to internal/resolved/types.go PreparedImage (for pod volume creation)
- [x] 1.2 Update mergePreparedImage in internal/resolved/merge.go to set PVCName from preparedArtifact.pvcRef

## 2. Status mapping

- [x] 2.1 Add internal/controller/swiftguest/status.go with MapPodToStatus(pod) -> status updates (phase, nodeName, podRef, conditions)
- [x] 2.2 Implement SetResolvedCondition(status, ok bool, reason string)
- [x] 2.3 Implement SetPodScheduledCondition(status, pod *corev1.Pod)
- [x] 2.4 Handle Pod Pending: set phase=Scheduling or Pending, PodScheduled=False, include reason if Unschedulable
- [x] 2.5 Handle Pod Running: set PodScheduled=True, phase=Running, nodeName, podRef
- [x] 2.6 Handle Pod Failed: set phase=Failed, PodScheduled=False with reason

## 3. Pod spec builder

- [x] 3.1 Add BuildPod(guest, resolved, seedConfigMapName, intentConfigMapName) to internal/controller/swiftguest/pod.go with image volume from resolved.PreparedImage.PVCName
- [x] 3.2 Add intent ConfigMap volume with runtime-intent.json
- [x] 3.3 Set resource requests/limits from ResolvedGuest.Resources
- [x] 3.4 Set container (placeholder or swiftletd) with AddVolumeMounts
- [x] 3.5 Set ownerReference to SwiftGuest, pod name, labels

## 4. Intent ConfigMap

- [x] 4.1 Build RuntimeIntent from ResolvedGuest using runtimeintent.Build
- [x] 4.2 Serialize intent with runtimeintent.Serialize
- [x] 4.3 Create ConfigMap with key runtime-intent.json, name <guest-name>-runtime-intent
- [x] 4.4 Set ConfigMap ownerReference to SwiftGuest

## 5. Controller reconcile loop

- [x] 5.1 On resolution error: set Resolved=False, phase=Failed, return (no pod creation)
- [x] 5.2 Ensure intent ConfigMap exists (create or update)
- [x] 5.3 Build pod with BuildPod; create or update pod
- [x] 5.4 On reconcile, call MapPodToStatus when pod exists; update SwiftGuest status
- [x] 5.5 Add Owns(&corev1.Pod{}) to SetupWithManager

## 6. Condition types

- [x] 6.1 Define condition types: Resolved, PodScheduled (use metav1.Condition; types from api/swift/v1alpha1 or constants)
- [x] 6.2 Set Resolved=True when resolution succeeds (before pod creation)

## 7. Verification and docs

- [x] 7.1 Add unit test for MapPodToStatus (pod phases -> status)
- [x] 7.2 Add config/samples/swiftguest-with-pod.yaml or update existing sample
- [x] 7.3 Document reconcile flow in docs/ or README if needed
