# Phase 2 r6: dev on `4f08e87`

Run 2026-09-28, 06:33–07:17 UTC, after Phase 1 r6 passed on dev (`phase1-r6.md`).

- **Setup.** Node names are generalised: `cp-1`, `worker-1`, `worker-2`. Pod
  IPs are redacted.
- **Launchers.** Every guest was created after the upgrade, and every
  launcher checked ran `swiftletd:sha-4f08e87`.
- **Space.** cp-1 had 63.7% free at the start and 57.4% at the end.

**No guest was lost.** Every passed scenario's namespace was deleted once its
evidence was saved, and each finished deleting:
- `val-r6-rt` (17 s), `val-r6-img` (12 s), `val-r6-ref` (11 s);
- `val-r6-pool` (12 s), `val-r6-smoke` (7 s).

`val-r6-dnr` (R6-D) is **kept**, as the plan says for a scenario that did not
fully pass.

## Results

| # | Verdict |
|---|---|
| R6-A | **PASS**: the guest applied before its image is never `Failed`: `Pending` with "SwiftImage not found" (controller) then "SwiftImage not Ready" until the image is `Ready`; no `ResolutionFailed` event; the round trip passes (restore 7 s, address kept) |
| R6-B | **PASS**: (1) `Pending` while importing; (2) `Failed` with "SwiftImage failed: Job has reached the specified backoff limit" and exactly one `ResolutionFailed` event, no launcher; (3) `Pending` 2 s after the delete, boots once the new image is `Ready` |
| R6-C | **PASS**: `Pending` with "SwiftGuestClass not found: …" through a missing class and seed; never `Failed`, no launcher; `Scheduling` 1 s after the class appears |
| R6-D | **PARTIAL.** `DstNeverReady` at 60 s, a `DestinationPodNeverReady` event, the guest untouched on its source, no destination pod left. **But the message does not name `FailedAttachVolume`**: Kubernetes emitted that event 94 s after the pod was created, 34 s after the budget ran out. Also, the target-side attach completed after the failure and lingered for about 4 min |
| R6-E | **Not observed**: no sandbox or warm slot failed during the run (0 `SlotEnded`, 0 `GuestFailed`) |
| R6-F | **PASS**: both replicas `Pending` while the image imported, never `Failed`; exactly 2 SwiftGuest objects ever; both boot |
| V1 | **PASS**: 5/5, 0 WARN lines; cleanup removed only the run's objects |

## R6-A: a guest applied before its image, plus V11-R2 (#685). PASS

`local-roundtrip-test.sh --namespace val-r6-rt --no-cleanup` (06:33:36–06:37:06),
with a 1 s watcher on the guest's phase, `Resolved` and Warning events:

```text
06:33:41 guest snapshot-local-source created (no status yet)
06:33:41.426 controller: "waiting to resolve" reason="SwiftImage not found: SwiftImage.image.kubeswift.io \"ubuntu-noble\" not found"   (x3, the first reconciles)
06:33:43 Pending  Resolved=False "SwiftImage not Ready"   image Importing   (controller: "waiting to resolve" x16 from here on)
06:35:02 image Validating
06:35:07 image Ready → guest Scheduling, Resolved=True
06:36:19 Running;  06:36:42 primaryIP 192.168.99.14
Warning events on the guest: none, throughout
```

- **The first reconciles saw "not found"** (06:33:41.4). The image existed 0.1
  s later, so the 1 s watcher's first reading was already "not Ready". The
  phase was never `Failed`. In round 5 it was `Failed` for 1 m 45 s here.
- **The round trip:**

```text
Captured on node <worker-1> (pause window 1114ms)
OK: launcher pod is in restore-receive mode, no stager (in-place fast path)   launcher swiftletd:sha-4f08e87
OK: sentinel survived: kubeswift-roundtrip-1790577411-23974
=== Tier B round-trip e2e PASS ===
primaryIP before / snapshot guestSpec.primaryIP / after: 192.168.99.14 / 192.168.99.14 / 192.168.99.14
SwiftRestore created 06:36:57 → Ready=True/RestoreReady 06:37:04 = 7 s
```

## R6-B: a failed image says so, and a recreated image is picked up (#685). PASS

In `val-r6-img`, one `kubectl apply` of `img404` (a 404 URL) followed by the
guest `r6b`, at 06:38:18. A 1 s watcher ran throughout.

```text
(1) 06:38:20 r6b Pending  Resolved=False "SwiftImage not Ready"   img404 Importing   (the import container retries its 404)
(2) 06:44:32 img404 Failed  Failed=True/ImportFailed "Job has reached the specified backoff limit"
    06:44:34 r6b Failed  Resolved=False/ResolutionFailed "SwiftImage failed: Job has reached the specified backoff limit"
             event: Warning ResolutionFailed x1 (06:44:32) "SwiftImage failed: Job has reached the specified backoff limit"
             launcher pods: 0
(3) 06:44:49.144 kubectl delete swiftimage img404
    06:44:49.286 working img404 applied (same name)
    06:44:51 r6b Pending  Resolved=False "SwiftImage not Ready"   (2 s after the delete)
    06:46:10 img404 Ready → r6b Scheduling
    06:47:01 r6b Running;  06:47:22 primaryIP 192.168.99.14
```

- **(2)** The guest's message is the image's `Failed` condition message,
  word for word. There is **exactly one** `ResolutionFailed` event (`x1`),
  and it did not repeat while the guest stayed `Failed`.
- **(3)** The replacement image existed 0.14 s after the delete. The
  controller's first reconcile after the delete (06:44:49.53) already found
  it, so the "not found" step was not visible; it went straight to "not
  Ready". R6-A shows the "not found" wait on its own.
- **Totals from the controller log for `r6b`:** 68 × "waiting to resolve"
  ("SwiftImage not Ready") and 2 × "resolution failed" ("SwiftImage failed: …").

## R6-C: other missing references wait (#685). PASS

In `val-r6-ref`, with an already `Ready` image, a guest `r6c` referencing the
SwiftGuestClass `val-r6-class` and the SwiftSeedProfile `r6-seed`. Neither
existed.

```text
06:48:12.000 apply r6c
06:48:14 Pending  Resolved=False "SwiftGuestClass not found: SwiftGuestClass.swift.kubeswift.io \"val-r6-class\" not found"   launchers: none
06:48:42.206 apply r6-seed          (message unchanged: the class is checked first)
06:49:12.421 apply val-r6-class
06:49:13 Scheduling, Resolved=True   (1 s after the class)
06:49:58 launcher pod created (after the root-disk clone);  06:50:05 Running;  06:50:26 primaryIP 192.168.99.10
```

- **Controller log:** 9 × "waiting to resolve" with "SwiftGuestClass not
  found", from 06:48:12 to 06:49:02. It never reported the seed, because the
  class is checked first.
- **Events.** No `ResolutionFailed` event on the guest. The one Warning in
  the namespace for `r6c` was `CalculateExpectedPodCountFailed` on its
  **PodDisruptionBudget** (the known round-4 finding 6), not on the guest.
- **Cleanup.** The SwiftGuestClass `val-r6-class` was deleted afterwards.

## R6-D: `DstNeverReady` names its cause, live (#682). PARTIAL

**Approved by William** (07:0x). A guest `val-r6-dnr/r6d`, class
`small-migratable` (`longhorn-migratable`), on cp-1.
- **Data written:** 6 GiB of random data in `/var/tmp/fill` (volume actual
  size 9.3 GiB).
- **The trigger:** deleting its worker-2 Replica object made Longhorn
  rebuild it.
- **The migration:** to worker-1, started the moment the engine showed a
  `WO` replica.

```text
07:10:57.316 kubectl -n longhorn-system delete replicas.longhorn.io <the worker-2 replica>
07:11:02 engine: <that replica>=ERR, then removed;  07:11:22.335 new replica 7ddf0cde=WO (rebuilding), 2 × RW
07:11:22.382 swiftctl -n val-r6-dnr migrate r6d --to <worker-1> --preferred-mode live --allow-ip-change --name r6d-mig
07:11:22 Validated, DestinationPodCreated r6d-mig-a08c8a on <worker-1>;  phase Preparing "waiting for destination pod ready"
07:12:22 Failed  DstNeverReady;  events: Warning DestinationPodNeverReady, Normal DestinationPodCleanedUp ("deleted destination pod … after pre-cutover Failed")
07:12:56 Warning FailedAttachVolume on the (already deleted) destination pod:
         "AttachVolume.Attach failed for volume \"pvc-cfe35b1a-…\" : rpc error: code = Internal desc = volume pvc-cfe35b1a-… failed to attach to node <worker-1> with attachmentID csi-…"
07:13:02 rebuild done: 3 × RW, volume healthy
```

**The full `failureMessage`:**

```text
destination pod "r6d-mig-a08c8a" never reached Ready within 1m0s budget: init container "network-init" waiting: PodInitializing
```

**What passes:**
- `DstNeverReady` fired after exactly 60 s.
- There is a `DestinationPodNeverReady` Warning event, with the same text.
- The guest is still `Running` on cp-1: the same launcher pod UID
  `26973c1b-…`, 0 restarts, continuous uptime (722 s at 07:15:45), the 6 GiB
  file intact, and a write succeeds.
- No destination pod is left.

**What does not pass: the cause is not named.**
- The message gives the pod's state after `within 1m0s budget:`, as #682
  intends. But the pod had **no Warning event yet**.
- Kubernetes' attach-detach controller emits `FailedAttachVolume` only when
  the attach call times out. That was at 07:12:56: 94 s after the pod was
  created, and 34 s after the 60 s budget ran out.
- So the message points at an init container ("network-init waiting:
  PodInitializing"). The real wait is the volume attach: kubelet starts no
  container, init or otherwise, before the pod's volumes are attached.
- The pod state visible within 60 s that would name the attach is the
  pod's `VolumeAttachment` (`attached=false`) for its PVC, or Longhorn's
  ticket "waiting for volume to migrate to node".

**Also seen: the target-side attach completes after the failure and lingers
for about 4 minutes.**

```text
07:13:04 Longhorn: migration engine on <worker-1> running; volume spec.migrationNodeID=<worker-1>; attachment tickets [<cp-1>, <worker-1>]
07:13:46–07:16:20 k8s VolumeAttachment for <worker-1>: attached=true (no pod on <worker-1> uses it); the same Longhorn state
07:16:42 the <worker-1> VolumeAttachment is gone, its engine stopped, migrationNodeID cleared; only <cp-1> remains
```

- **It clears by itself**, about 4 m 20 s after the destination pod was
  deleted.
- **The risk window.** For that time the volume is half-migrated to a node
  with no pod: two engines, and `migrationNodeID` set. A new migration, or a
  restart of the guest elsewhere, during that window may be refused or
  delayed. I did not test that.
- **The guest was unaffected.**

`val-r6-dnr` is kept for inspection: the guest `r6d` `Running` on cp-1, and
the SwiftMigration `r6d-mig` `Failed`.

## R6-E: launcher failure messages (#686), opportunistic. Not observed

- **Nothing failed.** No sandbox or warm slot failed during the run: 0
  `SlotEnded` and 0 `GuestFailed` events cluster-wide at 07:17.
- **The warm slot.** `field-testing/ft-gpu-pool-slot-j74r2` kept running
  (same uid `6aeee96c-…`, 0 restarts).

## R6-F: a pool created with its image does not churn (#681, #685). PASS

In `val-r6-pool`, one `kubectl apply` of a new SwiftImage `noble-pool` and a
SwiftGuestPool `r6f` (`replicas: 2`, class `small`) at 06:51:48. A 2 s watcher
recorded every SwiftGuest UID it saw.

```text
06:51:50 pool ready=0/2; noble-pool Importing; r6f-0[644b5b90] Pending "SwiftImage not Ready" | r6f-1[7fb623f1] Pending "SwiftImage not Ready"
06:53:04 noble-pool Validating
06:53:09 noble-pool Ready → both Scheduling
06:54:15 both launchers created;  06:54:20 r6f-1 Running (ready 1/2);  06:54:43 r6f-0 Running (ready 2/2)
```

- **No churn.** There were **exactly 2 distinct SwiftGuest objects** over
  the whole run (`r6f-0` `644b5b90-…`, `r6f-1` `7fb623f1-…`), and none was
  ever `Failed`. No replica was deleted and replaced, and the pool recorded
  no events.
- **Launchers:** both on `sha-4f08e87`.

## V1: smoke (regression). PASS

`NAMESPACE=val-r6-smoke make smoke-test` (06:55:50–07:09:17), then
`make smoke-test-cleanup`:

```text
disk-boot    PASS  hypervisor=cloud-hypervisor  primaryIP=192.168.99.13
kernel-boot  PASS  hypervisor=cloud-hypervisor  primaryIP=192.168.99.15
qemu-boot    PASS  hypervisor=qemu              primaryIP=192.168.99.18
gpu-alloc    PASS  Mock SwiftGPUNode <cp-1>; GPUAllocated=True
multi-nic    PASS  primaryIP=192.168.99.14
=== All scenarios PASSED ===   WARN lines: 0   launchers: swiftletd:sha-4f08e87 (all four)   Cannot open initramfs: 0
```

- **Cleanup.** It left nothing in `val-r6-smoke`.
- **Nothing else changed.** The cluster-scoped SwiftGuestClasses and
  SwiftGPUNodes, everything in `default`, and `innercp`'s uid and
  resourceVersion were identical before and after.

## Findings

1. **`DstNeverReady` still does not name a volume-attach wait** (R6-D). Within
   the 60 s budget, a destination pod waiting on its volume attach has no
   Warning event yet: `FailedAttachVolume` came at +94 s here. So the #682
   message falls back to "init container … waiting: PodInitializing", which
   points away from the cause. Checking the pod's `VolumeAttachment`
   (`attached=false`) would name it within the budget.
2. **A failed live migration leaves the target-side volume attachment for
   about 4 minutes** (R6-D). Deleting the destination pod does not cancel
   the in-flight attach. Longhorn completes it once the rebuild is done and
   starts a migration engine on the target. Kubernetes detaches it about 4
   min later. Nothing is lost, but for that window the volume is
   half-migrated to a node with no pod.
3. **The leftover `Failed` guests got no `ResolutionFailed` event on
   upgrade** (Phase 1, ntx). Their message was updated to "SwiftImage failed:
   …". The event fires on the transition, so guests already `Failed` before
   the upgrade got none. Fresh guests do get it (R6-B). This is minor and
   probably intended.

## State left on dev

- **Controller:** chart `0.0.0-dev.4f08e87` (rev 49), `--metrics-secure=true`.
- **Kept:** `val-r6-dnr` (R6-D):
  - guest `r6d` `Running` on cp-1, `small-migratable`, 6 GiB in `/var/tmp/fill`;
  - SwiftMigration `r6d-mig` `Failed`;
  - its volume healthy, attached only on cp-1.
- **Unchanged:**
  - `gpu-cells/innercp` (launcher uid `10bd28b8-…`, 0 restarts);
  - `field-testing/ft-gpu-pool-slot-j74r2` (uid `6aeee96c-…`, 0 restarts,
    holding the GPU).
- **Deleted during the run:** the SwiftGuestClass `val-r6-class`.
- **cp-1:** 57.4% free, `Schedulable=True`. **Nodes:** none cordoned.
