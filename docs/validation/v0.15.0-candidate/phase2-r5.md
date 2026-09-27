# Phase 2 r5: dev on `08d0165`

**When:**
- 2026-09-25 23:56 → 2026-09-26 00:30 UTC: V4a live, R5-A, R5-B, R5-C and V1.
- 2026-09-27 08:28–08:35 UTC: V1's cleanup and V11-R2.
- In between, my session was stopped and nothing ran. V1's smoke guests sat
  running in `val-r5-smoke` for those hours.

**Setup:**
- Node names are generalised: `cp-1`, `worker-1`, `worker-2`. Pod IPs are
  redacted.
- Every guest was created after the upgrade, and every launcher checked ran
  `swiftletd:sha-08d0165`.
- Before the run, cp-1 had 64.1% free (`phase0-r5.md`).

**No guest was lost.** Each scenario's namespace was deleted once its evidence
was saved, to keep cp-1 roomy: `val-r5-d7b` (12 s), `val-r5-dnr` (19 s),
`val-r5-sbx` (42 s), `val-r5-img` (13 s), `val-r5-smoke` (6 s) and `val-r5-rt`
(20 s). **Every namespace finished deleting.**

## Results

| # | Verdict |
|---|---|
| V4a live | **PASS**: guest on `--source`, "Longhorn volume … is healthy" before the migration, "All checks passed", `observedTransferDuration` 19.657 s |
| R5-A | **NOT TRIGGERED.** A volume kept `degraded` on purpose live-migrated normally, so `DstNeverReady` never fired. See R5-A for why, and for a corrected reading of round 4's failures |
| R5-B | **PASS**: (a) `Pending` for ~100 s while the image imported, then boots; (b) `Pending` for 6 m 14 s of import retries, then `Failed`. Minor message finding |
| R5-C | **PASS**: (a) no phase goes back; exit 0 → `Completed`, exit 3 → `Failed`, exit code 3; (b) `SlotLost` 0.3 s after the slot pod was force-deleted, naming the pod; the pool warmed a new slot |
| V9b | **SKIPPED**: `ft-gpu-pool`'s replacement slot holds the GPU. That slot itself shows #659 fixed (see V9b) |
| V1 | **PASS**: all 5 scenarios, 0 WARN lines; cleanup removed only the run's objects |
| V11-R2 | **PASS**, with a **new finding**: a guest applied before its SwiftImage exists shows `Failed` for the whole import (the #681 gap) |

## V4a live (#684). PASS

`NAMESPACE=val-r5-d7b migration-test.sh --mode live --source <worker-1>
--target <worker-2> --storage-class longhorn-migratable --no-cleanup`, from the
`08d0165` checkout (23:56:58–00:02:55):

```text
Launcher scheduled on <worker-1>; the other nodes are uncordoned
Guest uptime before: 39s
Waiting for Longhorn volume pvc-2d748410-… (PVC swiftguest-root-e2e-guest) to be healthy (max 15min)...
Longhorn volume pvc-2d748410-… is healthy
swiftmigration e2e-mig created (00:02:06)
Migration completed in 43s (resolved mode: live)
PASS: disk sentinel survived the migration
PASS: tmpfs sentinel survived and uptime kept counting (39s -> 189s): the running VM moved
PASS: webhook rejected migration of guest with migration.enabled=false
All checks passed.
```

- **The volume wait.** The volume was created at 23:58:43 and was healthy
  before 00:02:06, when the migration started. The script does not timestamp
  that line; from the guest's uptime reading, the wait was about 1 m 50 s.
- **Migration:** `observedTransferDuration=19.657s`, `observedDowntime=1.171s`,
  0 `SourceCompleteMissing`, and no node left cordoned.
- **V5-style phaseDetail watch (0.2 s):**

```text
00:02:23.569 transferring guest state   26 → 52 → 79
00:02:42.818 destination running; waiting for the source's report   DestinationRunning
00:02:43.164 cutover: deleting source pod   PodRefSwapped
00:02:43.500 cutover: completing
00:02:44.238 Resuming "waiting for guest health on destination"
00:02:44.559 Completed "destination guest healthy (IP 192.168.99.19)"
```

## R5-A: `DstNeverReady` names its cause (#682). NOT TRIGGERED

**Setup as specified (00:04–00:08):**
- the StorageClass `val-r5-degraded` (a copy of `longhorn-migratable` with
  `numberOfReplicas: "4"`);
- a SwiftGuestClass of the same name (RWX Block, 2 CPU, 2Gi, 10Gi root);
- guest `val-r5-dnr/r5a` on worker-1, launcher `sha-08d0165`.

The volume was `degraded` as intended:

```text
volume pvc-a3d7b1e5-…: state=attached robustness=degraded numberOfReplicas=4 migratable=true
  replicas: <worker-2> running, <cp-1> running, <worker-1> running, 1 × stopped (unscheduled)
  Scheduled=False/ReplicaSchedulingFailure: precheck new replica failed: disks are unavailable
```

**The migration completed.** `swiftctl -n val-r5-dnr migrate r5a --to <worker-2>
--preferred-mode live --allow-ip-change --name r5a-mig`:

```text
00:08:19 Validated, DestinationPodCreated
00:08:35 DestinationPodReady → StopAndCopy; transferring 26 → 52 → 79
00:08:56.411 "destination running; waiting for the source's report"
00:08:57 Completed   observedTransferDuration=19.715s  observedDowntime=1.762s
after: engine on <worker-2>, replicaModes [RW, RW, RW]; the volume still degraded (4th replica unscheduled)
```

**Why, and what it corrects in round 4.** Longhorn does not refuse to migrate
a *degraded* volume. What it waits on is a replica that is still
**rebuilding**. Round 4's two failures both fit that:

```text
round 4 attempt 1: replicas <worker-1>, <worker-2>, <cp-1> all "running"; engine replicaModes: 2 × RW   (third still rebuilding)
round 4 attempt 2: replicas <worker-2>, <worker-1>, <cp-1> all "running"; engine replicaModes: [RW, RW]
r5a (this run):    3 replicas running; engine replicaModes: [RW, RW, RW]; 4th never scheduled → migrates
```

- `phase2-r4.md` says "Longhorn will not live-migrate a degraded volume". It
  should say "a volume with a replica still rebuilding". The #684 wait for
  `healthy` covers both, so the script fix stands.
- **#682's message is therefore not exercised on the cluster.** The unit
  tests cover it.
- **A method that would trigger it**, for William to decide: on a test
  guest's volume, delete one Longhorn replica so that Longhorn rebuilds it,
  then live-migrate at once, while the engine shows a `WO` replica. That
  deletes a Longhorn object, so I did not do it without approval.
- As R5-A's step 4 says, the guest, the class and the StorageClass
  `val-r5-degraded` were deleted afterwards (00:09–00:10). Their YAML and
  events were saved.

## R5-B: a guest waits for its importing image (#681). PASS

One `kubectl apply` per pair in `val-r5-img`, both at 00:10:44, with a 2 s watcher.

**(a) A real Noble image and its guest, `r5b-a`:**

```text
00:10:46 noble-a Importing   r5b-a Pending  Resolved=False/ResolutionFailed "SwiftImage not Ready"   launchers: none
00:12:18 noble-a Validating  (unchanged)
00:12:26 noble-a Ready       r5b-a Scheduling  Resolved=True
00:13:12 launcher pod created;  00:13:24 r5b-a Running;  primaryIP 192.168.99.13
```

- **Waited about 100 s**, `Pending` throughout. It was never `Failed` and had
  no launcher pod until the image was `Ready`.
- The controller logged `"msg"="waiting to resolve" … "reason"="SwiftImage not Ready"`,
  16 times for `r5b-a` and 50 for `r5b-b`.
- **Same behaviour seen in R5-A:** `r5a` was `Pending`/"SwiftImage not Ready"
  for 77 s, from 00:04:21 to 00:05:38.

**(b) A 404 image and its guest, `r5b-b`:**

```text
00:10:46 noble-404 Importing  r5b-b Pending  Resolved=False "SwiftImage not Ready"   (import container: curl 404, restarting)
00:16:58 import Job BackoffLimitExceeded (backoffLimit 6)
00:16:59 noble-404 Failed  Failed=True/ImportFailed "Job has reached the specified backoff limit"
00:17:01 r5b-b Failed  Resolved=False/ResolutionFailed "SwiftImage not Ready"      launcher pods ever: 0
```

- **Timings:** `Pending` for 6 m 14 s; the guest was `Failed` within 2 s of the
  image.
- **Minor finding.** Once the image has failed, the guest shows only
  `phase: Failed` and the unchanged condition `Resolved=False` "SwiftImage not
  Ready", from 00:10:45. It has no event, and nothing says the image *failed*.
  The controller log switches from "waiting to resolve" to "resolution
  failed", with the same reason. An operator reading the guest cannot tell
  "still importing" from "gave up".

## R5-C: sandbox phase and ended slots (#683). PASS

`val-r5-sbx`, with the SwiftKernel `sandbox` and image
`public.ecr.aws/docker/library/alpine:3.20`.

**(a) Three one-shot sandboxes, phase polled every 0.5 s (00:11:43–00:12:00):**

```text
r5c-ok1:   - → Materializing → Running → Completed    exitCode 0, GuestRunning=False/Completed
r5c-ok2:   - → Materializing → Running → Completed    exitCode 0
r5c-exit3: - → Materializing → Running → Failed       exitCode 3, GuestRunning=False/WorkloadFailed
```

No phase went back. Round 4's `Running → Materializing` step is gone.

**(b) A one-slot pool (`r5c-pool`, `minWarm: 1`), then a force-deleted slot:**

```text
00:12:18 pool Ready, slot r5c-pool-slot-bxrwm warm
00:12:33 r5c-co (poolRef r5c-pool, sleep 600) Running; event CheckedOut "claimed warm slot r5c-pool-slot-bxrwm from pool r5c-pool"
00:12:33.940 kubectl delete pod r5c-pool-slot-bxrwm --grace-period=0 --force
00:12:34.427 (+0.3 s) r5c-co Failed  GuestRunning=False/SlotLost
             "claimed warm slot pod r5c-pool-slot-bxrwm is gone and the workload never reported an exit"
             new slot r5c-pool-slot-7mfnw Pending (warm)
00:12:38.507 (+4.4 s) r5c-pool-slot-7mfnw Running (warm)
```

## V9b: warm GPU pool kernel (#659). SKIPPED, but the fix is visible

- **Why skipped.** Phase 1 freed the GPU at 23:50:58. By 00:00:02, though,
  `ft-gpu-pool` had warmed a replacement slot: the Docker Hub rate limit had
  cleared. The slot has held the GPU since then, so the plan's condition for
  V9b is not met.
- **The fix, observed read-only.** The replacement is itself a GPU warm-pool
  slot of a pool with `gpuProfileRef` and no `kernelProfileRef`, which is
  V9b's case:

```text
ft-gpu-pool-slot-j74r2 created 00:00:02, Running 00:00:38, 0 restarts, swiftletd:sha-08d0165, node <worker-2>
  hostPath kernel-artifacts: /var/lib/kubeswift/kernels/field-testing/gpu-sandbox
  launcher: kernel=/var/lib/kubeswift/kernels/field-testing/gpu-sandbox/bzImage
GPU 0000:01:00.0 allocatedTo sandbox:field-testing/ft-gpu-pool-slot-j74r2
```

The pool was not changed.

## V1: smoke (regression). PASS

`NAMESPACE=val-r5-smoke make smoke-test` (2026-09-26 00:18:08–00:30:01), then
`make smoke-test-cleanup` (2026-09-27 08:28):

```text
disk-boot    PASS  hypervisor=cloud-hypervisor  primaryIP=192.168.99.16
kernel-boot  PASS  hypervisor=cloud-hypervisor  primaryIP=192.168.99.18
qemu-boot    PASS  hypervisor=qemu              primaryIP=192.168.99.18
gpu-alloc    PASS  Mock SwiftGPUNode <cp-1>; GPUAllocated=True
multi-nic    PASS  primaryIP=192.168.99.12
=== All scenarios PASSED ===   WARN lines: 0   launchers: swiftletd:sha-08d0165 (all four)
Cannot open initramfs file: 0 lines
```

- **Cleanup.** It left nothing in `val-r5-smoke`.
- **Nothing else changed.** The inventory was identical before and after:
  - the cluster-scoped SwiftGuestClasses and SwiftGPUNodes;
  - everything in `default`;
  - `innercp`'s uid and resourceVersion.
- `default/ubuntu-noble` no longer exists (Phase 0 deleted it), so it could
  not be touched.

## V11-R2: in-place restore (regression). PASS, with a new finding

`local-roundtrip-test.sh --namespace val-r5-rt --no-cleanup` (08:30:02–08:34:21):

```text
Captured on node <worker-1> (pause window 1284ms)
OK: launcher pod is in restore-receive mode, no stager (in-place fast path)   launcher swiftletd:sha-08d0165
OK: sentinel survived: kubeswift-roundtrip-1790498044-30640
=== Tier B round-trip e2e PASS ===
```

| | Value |
|---|---|
| `primaryIP` before, snapshot `guestSpec.primaryIP`, after | `192.168.99.18` / `192.168.99.18` / `192.168.99.18` |
| SwiftRestore | created 08:34:10 → `Ready=True/RestoreReady` 08:34:19 = **9 s** (Restoring → Resuming → Ready) |

**New finding: #681 does not cover a guest created before its image.**

```text
08:30:03.533 controller: "resolution failed" reason="SwiftImage not found: … \"ubuntu-noble\" not found"   (x2)
08:30:03.654 controller: "waiting to resolve" reason="SwiftImage not Ready"   (x20 from here on)
08:30:04 guest snapshot-local-source phase=Failed      <- and it stays Failed while the image imports
08:31:49 guest phase=Scheduling (image Ready)
08:33:30 Running
```

- **What happens.** `local-roundtrip-test.sh` applies the guest (line 142)
  before it creates the SwiftImage (line 148). The guest's first reconcile
  finds no image, and the guest goes `Failed`. 0.1 s later the image exists,
  and every reconcile after that is a "waiting to resolve" wait. **The phase
  stays `Failed` for the whole import (1 m 45 s)**, then recovers on its own.
- **So finding 2 is only half fixed.** "Not Ready" is now a wait, but "not
  found" is still terminal-looking. And a guest that saw "not found" once is
  not moved back to `Pending` when the reason becomes "not Ready".
- **Who hits it.** Anyone who applies the guest before, or in the same
  apply as but ahead of, its image. The script does this on every fresh
  namespace. R5-B applied the image first, which is why it passed.

## Findings

1. **#681 gap: a guest reconciled before its SwiftImage exists shows `Failed`
   until the image is Ready** (V11-R2). "SwiftImage not found" still sets
   `Failed`, and the later "not Ready" wait does not reset the phase to
   `Pending`. It recovers by itself, so no guest is lost. But this is round
   4's finding 2 in another order.
2. **A guest whose image failed does not say so** (R5-B b). It shows `Failed`
   plus `Resolved=False` "SwiftImage not Ready", with no event.
3. **R5-A's premise does not hold on this Longhorn.** A degraded volume
   migrates; only a rebuilding replica blocks it. Round 4's analysis should
   read "rebuilding", not "degraded". #682's message was not seen live. The
   replica-deletion method above would trigger it, with William's OK.
4. **`SlotEnded`'s message is generic** (Phase 1): "launcher pod failed". It
   does not carry the slot pod's own failure.
5. **`kubectl auth can-i … pods/exec` with `--as`** printed the inverse of a
   direct SubjectAccessReview (Phase 3, `phase3-r5.md`). This is a kubectl
   quirk, not a product change.

## State left on dev

- **Controller:** chart `0.0.0-dev.08d0165` (rev 48), `--metrics-secure=true`.
- **No `val-*` namespaces.** The only SwiftGuest is `gpu-cells/innercp`
  (launcher uid `10bd28b8-…`, 0 restarts).
- **`field-testing/ft-gpu-pool`:** `Ready`, and its replacement slot
  `ft-gpu-pool-slot-j74r2` holds the GPU. Not changed by me.
- **cp-1:** 63.8% free, `Schedulable=True`.
- **Nodes:** none cordoned.
- **Deleted during the run:** the temporary StorageClass `val-r5-degraded` and
  the SwiftGuestClasses `val-r5-degraded` and `migration-e2e-live`.
