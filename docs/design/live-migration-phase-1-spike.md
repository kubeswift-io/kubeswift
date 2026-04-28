# Live Migration Phase 1 — Spike Results

> **Status:** Spike complete — Stage 1 of Phase 1 implementation
> **Date:** 2026-04-28
> **Cluster:** k0s 1.34.3, 3 nodes (frida control-plane, miles + boba workers), Longhorn 22d, default StorageClass `longhorn` (RWO, full-copy)
> **Audience:** Stage 2 implementer + staff-architect review

This spike answers the two empirical questions for Phase 1 (offline migration via direct PVC reuse) and captures the PVC ownerRef behavior that informs the D3 architectural decision.

---

## Q1 — Cross-node PVC reuse on Longhorn (RWO)

### Experiment

1. SwiftGuest `spike-guest` created with `cloneStrategy: copy`, default SwiftGuestClass (4Gi RAM, 40Gi root disk), pinned to `boba` via `kubectl cordon miles`. Reached `Running` with `primaryIP=10.244.125.15`.
2. Wrote sentinel inside the guest: `SPIKE-SENTINEL-PRE-MIGRATION-1777382314` to `/root/spike-sentinel.txt`. Captured machine-id `9115d9d74efe49c4baeb878f094d3de2`.
3. Stopped guest by deleting the launcher pod (see Q1b — `runPolicy: Stopped` is reactive-only).
4. Cordoned `boba`, uncordoned `miles`, patched `runPolicy: Running` on the unchanged SwiftGuest CR.
5. Waited for new pod on `miles`, IP discovery, and verified sentinel.

### Result: works cleanly. End-to-end timing on Longhorn

| Phase | Elapsed | Notes |
|---|---|---|
| pod delete issued (T0) | 0s | `kubectl delete pod ... --grace-period=30` |
| pod gone | 32s | swiftletd SIGTERM handling consumed grace period |
| Longhorn `attached → detaching` | 42s | Longhorn-side detach starts only after pod is gone |
| Longhorn `detached` (PVC released) | 45s | Cross-node detach window |
| `runPolicy: Running` patch issued (T1) | — | (after stop completed) |
| pod scheduled on miles | T1+1s | Standard k8s scheduler dispatch |
| Longhorn `detached → attaching@miles` | T1+4s | |
| Longhorn `attached@miles` | T1+7s | Cross-node attach window: ~3-7s |
| pod `Running` on miles | T1+24s | swiftletd boot + CH spawn + cloud-init resume |
| status `primaryIP=10.244.125.17` | T1+24s | Fresh DHCP lease from miles' bridge |

**Total stop+start downtime (best case): ~70s on Longhorn full-copy.** Detach is the long pole on the stop side (45s); cross-node attach is fast (~7s). Restart-side dominated by VM boot (~17s after attach) — this is the cloud-init resume cost, not migration overhead. On true CoW drivers (Rook Ceph RBD, EBS) the Longhorn `attached → detaching` 9s window and `attaching → attached` 7s window should both be near-instantaneous, leaving boot as the only meaningful delay.

### Sentinel verification (post-migration)

```
=== SENTINEL ===
SPIKE-SENTINEL-PRE-MIGRATION-1777382314    ← unchanged
=== HOSTNAME ===
spike-source                                ← unchanged (cloud-init didn't re-run; expected)
=== MACHINE-ID ===
9115d9d74efe49c4baeb878f094d3de2            ← unchanged
```

Disk content survived the cross-node attach intact. Identity inheritance is the same as a normal reboot — cloud-init does not re-run, so hostname/machine-id are stable. This is correct behavior: offline migration preserves the disk's view of its own identity.

### Q1a — IP preservation

The guest's IP changed from `10.244.125.15` (boba) to `10.244.125.17` (miles). Both are on the same `10.244.125.0/24` subnet but the leases are independent because each node runs its own dnsmasq. **This is expected and matches the live migration design's Constraint 6** — KubeSwift's default node-local bridge does NOT preserve IPs across nodes. Phase 1 docs must call this out: *with default networking, migrated guests get a fresh IP*. Operators needing IP preservation must use multus + macvlan or OVN-K layer-2 (and the SwiftMigration validation webhook should reject migrations of guests on node-local networks when `targetNode != sourceNode`, mirroring the design doc's Constraint 6 logic).

### Q1b — `runPolicy: Stopped` does not actively kill running pods

Patching `spec.runPolicy: Stopped` while the pod is Running has no effect for **164s+** (test cancelled at that point). The SwiftGuest controller's `lifecycle == "stop"` branch is reactive: it transitions phase to `Stopped` only when the pod is already gone or terminated. To stop a running guest, the controller (or operator) must explicitly delete the pod.

`swiftctl stop` does both — patches `runPolicy: Stopped` AND `kubectl delete pod`. The Phase 1 SwiftMigration controller's `Preparing` phase must do the same. **This is a real implementation finding**: the design doc's "stop the source guest, wait for pod release" must call `Delete(pod)` explicitly, not just patch the SwiftGuest spec.

### Q1c — SwiftGuest has no `nodeName`/`nodeSelector` field

`buildDiskBootPod` does not set `pod.spec.nodeSelector`. Kernel-boot uses a hard-coded `kubeswift.io/kernel-node=true`; GPU pods use `kubernetes.io/hostname=<allocated-node>`. **Plain disk-boot pods are unpinned** — the scheduler picks. The spike used cordon/uncordon to control placement, which is fine for spike validation but not how production migration will work.

**Phase 1 must add a node-pinning field to SwiftGuest** (the cleanest options: `spec.nodeName string` or `spec.nodeSelector map[string]string` honored by the pod builder when set). The SwiftMigration controller's `StopAndCopy` phase patches this on the source SwiftGuest before recreating the pod. Recommend `spec.nodeName` for simplicity (mirrors `pod.spec.nodeName`); `spec.nodeSelector` can be added later if needed.

---

## Q2 — Schedulability pre-flight check

Tested all three approaches against the cluster.

| Approach | Detects overcommit | Detects bad node-selector | Side effects | Notes |
|---|---|---|---|---|
| `kubectl create --dry-run=server` | **No** | **No** | None | Server dry-run only validates admission/CRD validation; **does not run scheduler**. `pod/x created (server dry run)` even with `nodeName: frida + cpu: 100`. **Useless for schedulability.** |
| Real pod with `nodeName` | Yes (Phase=Failed, event `OutOfcpu`, <5s) | N/A | Pod must be cleaned up | Direct binding bypasses scheduler; kubelet rejects at admit time. Fast and definitive but creates a real Failed Pod that the controller must own. |
| Real pod with `nodeSelector` | Yes (Phase=Pending, event `FailedScheduling`, ~4s) | Yes (same event) | Pod must be cleaned up; stays Pending forever | `0/3 nodes are available: 1 node(s) were unschedulable, 2 node(s) didn't match Pod's node affinity/selector` |
| **Manual capacity check** (read `node.status.allocatable` + sum running pod requests on that node, plus `node.spec.unschedulable` + Ready condition + label-match check) | **Yes** | **Yes** | **None** | ~30 lines of Go; one `Get(Node)` + one `List(Pod, fieldSelector=spec.nodeName)` call |

### Recommendation

**Use the manual capacity check** in the Validating phase. Specifically:

1. `Get(Node{name: target})` — verify exists, `Ready=True`, `spec.unschedulable=false`.
2. `List(Pod, fieldSelector=spec.nodeName=target, status.phase!=Failed,Succeeded)` — sum `spec.containers[*].resources.requests.{cpu, memory}`.
3. Compute headroom = `allocatable - used`.
4. Compare against the SwiftGuest's `guestClassRef`-resolved `requests.cpu/memory + LauncherMemoryOverheadMiB`.
5. Verify any required node labels (`kubeswift.io/kernel-node` for kernel boot, `kubeswift.io/gpu-node` if `gpuProfileRef` set — though Phase 1's webhook will refuse GPU migrations to nodes without the GPU profile mapping).

This avoids leaving stray pods on the cluster and gives the validation webhook the information it needs to **reject** invalid migrations at submission time, not just at controller reconcile time. The check is cheap enough to run in the webhook itself.

The "real pod, observe" approach is tempting but creates a debris-cleanup problem and adds 4-5s latency to every validation. Manual capacity check is sub-second and zero-side-effect.

---

## D3 — PVC ownerRef observations (input for staff-architect)

The architect call from the spike-readiness summary listed three sub-options for handling PVC ownership during the StopAndCopy transition. The spike eliminates Option B (delete+recreate the SwiftGuest with same name) and validates Option A (in-place patch).

### Approach A — In-place SwiftGuest patch (validated)

The spike's main experiment. The SwiftGuest CR is unchanged (`uid: 7a7bce70-...` throughout). Only `spec.runPolicy` is toggled `Running → Stopped → Running` and (in production) `spec.nodeName` is patched to the target node. Pod is recreated naturally by the controller on the new node, picks up the existing PVC.

**PVC ownerRef across all phases:**
```
[T0 baseline]   ownerRef → spike-guest, uid=7a7bce70-...
[stop + detach] ownerRef → spike-guest, uid=7a7bce70-...   (unchanged)
[restart on miles] ownerRef → spike-guest, uid=7a7bce70-... (unchanged)
```

**No D3 problem in Approach A.** The PVC is owned by the same SwiftGuest object throughout. No orphan-check race. No PR-#21-style label dance needed. `EnsureRootDiskClone` sees `metav1.IsControlledBy(pvc, guest) == true` because the UID matches.

### Approach B — SwiftGuest delete + recreate (eliminated by spike)

`kubectl delete swiftguest spike-guest` with default cascade. **Result:** PVC was GC'd at T+33s along with the pod. There is no window where the PVC survives without the SwiftGuest unless explicit action is taken (`--cascade=orphan` or pre-stripping the ownerRef). For Phase 1 direct PVC reuse, Approach B is more complex than Approach A with no benefit — the SwiftGuest CR identity is preserved across migration anyway.

### Approach C — migration-seeded label (PR-#21 mirror)

Not needed if we go with Approach A. The label pattern would only matter if Phase 1 did delete+recreate (Approach B) AND wanted to reattach the orphaned PVC to a new SwiftGuest. Since Approach A is empirically simpler and the SwiftGuest design intent is "the SwiftGuest CR is unchanged from the operator's perspective" (live-migration design line 56), Approach A is the right pick.

### Architect question

**Recommended for Stage 2 architect review:** confirm Approach A (in-place SwiftGuest patch — toggle `runPolicy` and patch `nodeName`). The spike validates it works end-to-end on Longhorn with no ownerRef hazards. The migration-seeded label pattern (Option 3 in the spike-readiness summary) becomes unnecessary.

---

## Summary of findings affecting Stage 2

1. **Q1 PASS** — direct PVC reuse works on Longhorn cross-node. ~70s end-to-end downtime on full-copy storage; expected to be much faster (boot-bound, ~25s) on true CoW drivers.
2. **Q2 — manual capacity check is the right schedulability gate.** Server dry-run is useless; real-pod-probe is debris-prone. Implement as a webhook-time helper.
3. **D3 — Approach A (in-place SwiftGuest patch) is the right pattern.** No ownerRef transition needed. Approach B and the migration-seeded label are unnecessary.
4. **NEW finding — Phase 1 must add `spec.nodeName` (or `nodeSelector`) to SwiftGuest.** The pod builder honors it when set. Without this field, the SwiftMigration controller has no way to pin the recreated pod to the destination node.
5. **NEW finding — Phase 1 `Preparing` phase must explicitly `Delete(pod)`, not just patch `runPolicy: Stopped`.** The current SwiftGuest controller's stop guard is reactive only. Mirror what `swiftctl stop` does.
6. **Networking constraint applies as designed** — node-local bridge does not preserve IPs across nodes. Phase 1 validation webhook should reject cross-node migrations when the SwiftGuest is on the default network and source≠target. (Same-node migrations are pointless for offline; nothing to reject.) This will be re-relaxed in Phase 3 when multi-node networks are wired up.

---

## Cleanup

Spike namespace `migration-spike` deleted. Both worker nodes uncordoned. No residual resources.
