# Phase 2 r4: dev suites on `9604992`

Run 2026-09-25, 20:22–21:48 UTC, after Phase 1 r4 recovered on dev (`phase1-r4.md`).

- Every guest was created after the upgrade, and every launcher checked ran
  `swiftletd:sha-9604992`.
- Node names are generalised: `cp-1`, `worker-1`, `worker-2`. Pod IPs are redacted.
- **Namespaces:**
  - `val-r4-smoke`: V1, V2, V10;
  - `val-r4-cs`: V7;
  - `val-r4-snap`: V8;
  - `val-r4-nok`: V9;
  - `val-r4-mig`: V3, V4b, V6;
  - `val-r4-d7a`, `val-r4-d7b`, `val-r4-d7b2`: V4a;
  - `val-r4-rt`: V11-R2.

**No guest was lost.** Every live migration that started either completed or
left the guest running on its source.

**Phase 2 is not complete.** V4a live and V11's T3/T5 are **blocked by the
lab's Longhorn**, not by the candidate:
- cp-1's Longhorn disk is below Longhorn's minimum free space, so every new
  volume gets only 2 of its 3 replicas;
- Longhorn will not live-migrate a degraded volume.

The details are under V4. Because not everything passed, the `val-r4-*`
namespaces are kept, as the plan says.

## Results

| # | Verdict |
|---|---|
| V1 | **PASS**: all 5 scenarios, `gpu-alloc` included; 0 WARN lines; cleanup removed only the run's objects |
| V2 | **PASS** |
| V3 | **PASS**: (a) graceful power-off, `Stopped` 6 s after the patch; (b) `StopDeferred` names the migration, the migration completes, then the guest stops gracefully |
| V4 | (a) offline **PASS**. (a) live **BLOCKED (environment)**: failed twice with `DstNeverReady`, because Longhorn would not migrate the fresh, degraded volume; the guest stayed on its source both times. (b) **PASS** |
| V5 | **PASS**: `destination running; waiting for the source's report` appears with `DestinationRunning` in every live migration watched (v6-b, v4b-mig); 0 `SourceCompleteMissing` |
| V6 | **PASS**: waited 2.1 s in Validating (`AwaitingTerminatingPods`), then completed |
| V7 | **PASS** (speedup 0.6x, informational) |
| V8 | **PASS**: the namespace was gone in 19 s |
| V9 | (a) **PASS**. (b) **SKIPPED**: there is no free GPU |
| V10 | **PASS** |
| V11 | R2 **PASS**. T3 and T5 **NOT RUN**: blocked by the same Longhorn cause as V4a live |

## V1: smoke in a namespace (all scenarios). PASS

`NAMESPACE=val-r4-smoke make smoke-test` (20:22–20:34), then
`NAMESPACE=val-r4-smoke make smoke-test-cleanup` (20:35).

```text
disk-boot    PASS  hypervisor=cloud-hypervisor (expected)  primaryIP=192.168.99.11
kernel-boot  PASS  hypervisor=cloud-hypervisor (expected)  primaryIP=192.168.99.14
qemu-boot    PASS  hypervisor=qemu (expected)              primaryIP=192.168.99.18
gpu-alloc    PASS  Mock SwiftGPUNode <cp-1> (named after an existing Node, which has no GPU); GPUAllocated=True; gpu.nodeName=<cp-1>
multi-nic    PASS  primaryIP=192.168.99.12
=== All scenarios PASSED ===        WARN lines in the whole log: 0
launchers: swiftletd:sha-9604992 (all four)
```

**Cleanup removed only the run's objects.** Before/after inventory:
- **Removed:**
  - every SwiftGuest, SwiftImage, SwiftKernel, SwiftSeedProfile, SwiftGPUProfile
    and runtime-intent ConfigMap in `val-r4-smoke`;
  - the cluster-scoped SwiftGuestClass `default` and the mock SwiftGPUNode `<cp-1>`,
    which this run created;
  - the three guest root PVCs, garbage-collected within 40 s.
- **Untouched:** the other guest classes (the pre-existing `cs-class`,
  `snapshot-*`, etc.), the real SwiftGPUNode `<worker-2>`, and everything in
  `default`.
- **`default/ubuntu-noble`:** uid `ed9f5a44-…`, resourceVersion `11410` before
  the run and after the cleanup.

## V2: guest and pod IPs (#676). PASS

```text
NAMESPACE      NAME             PHASE     NODE       GUEST IP        POD IP      AGE           (default columns)
val-r4-smoke   faas-test        Running   <worker-1> 192.168.99.14   <pod-ip>    7m3s
…
-o wide adds: IP SCOPE (Pod) between POD IP and HYPERVISOR
```

- For every running smoke guest, `status.network.primaryIPScope=Pod`, and
  `status.network.podIP` equals the launcher pod's `status.podIP`: `yes` for
  faas-test, multi-nic-test, qemu-test and sample.
- **Two nat guests sharing a Guest IP**, caught by a watcher at 20:41:24:

```text
gpu-cells/innercp     Running <worker-2> GUEST IP 192.168.99.20  POD IP <pod-ip A>  IP SCOPE Pod
val-r4-cs/cs-copy-2   Running <cp-1>     GUEST IP 192.168.99.20  POD IP <pod-ip B>  IP SCOPE Pod
```

- Pod IP A ≠ pod IP B: different nodes' pod subnets, each equal to its own
  launcher's `status.podIP`. This is the duplicate-`primaryIP` case from round 1,
  now told apart by `Pod IP` and labelled `IP Scope: Pod`.

## V3: runPolicy Stopped (#678). PASS

**(a)** Guest `val-r4-mig/v3a` (launcher on cp-1, grace period 30 s):

```text
21:27:25.669 patch runPolicy=Stopped
21:27:25     SwiftGuest event Stopping: "runPolicy is Stopped; deleted launcher pod v3a, the guest shuts down within its termination grace period"
21:27:25.924 launcher: sigterm_received; requesting guest ACPI power-off → guest_poweroff_requested
21:27:29.556 guest firmware: "ResetSystem2: ResetType Shutdown"
21:27:29.694 launcher: vm_stopped_gracefully
21:27:31     phase=Stopped (+6 s); GuestRunning=False/Stopped "the guest is stopped; it has no launcher"
```

**(b)** Guest `val-r4-mig/v3b`, live migration `v3b-mig` worker-1 → cp-1,
patched to `Stopped` once it was in StopAndCopy:

```text
21:28:12 SendIssued / ReceiveIssued (StopAndCopy)
21:28:13 SwiftGuest event StopDeferred: "runPolicy is Stopped; waiting for SwiftMigration v3b-mig to finish before stopping the guest"   (count 2, last 21:28:32)
21:28:32 CutoverStep1;  destination: dispatch_migration_receive_complete state=Running elapsed_ms=19597
21:28:34 v3b-mig Completed   observedTransferDuration=19.661s  observedDowntime=1.971s
21:28:44 SwiftGuest event Stopping: "deleted launcher pod v3b-mig-c545dc"   (the destination pod)
21:28:44.995 destination launcher: sigterm_received → guest_poweroff_requested
21:28:46.810 vm_stopped_gracefully
21:28:49 phase=Stopped
```

The guest stopped only after the migration completed, on the node it had
migrated to. Neither launcher log shows a hard kill.

## V4: migration script and pinned repin (#679, #680)

### (a) offline: PASS

```text
NAMESPACE=val-r4-d7a migration-test.sh --mode offline --source <worker-1> --target <worker-2>
Cordoned while the guest is placed: <worker-2> <cp-1>
Launcher scheduled on <worker-1>; the other nodes are uncordoned
Migration completed in 53s (resolved mode: offline)
PASS: disk sentinel survived the migration
PASS: webhook rejected migration of guest with migration.enabled=false
All checks passed.
```

- The guest landed on `--source`, which is the #679 fix.
- No node was left cordoned.
- The script deleted its namespace.

### (a) live: BLOCKED by the environment (two attempts)

`migration-test.sh --mode live --guest-class small-migratable --source <worker-1>
--target <worker-2> --no-cleanup`, in `val-r4-d7b` and then `val-r4-d7b2`.

- **What happened.** Both attempts placed the guest on `--source` correctly,
  then failed the same way, about 3 minutes after the guest's root volume was
  created:

```text
[5s]…[60s] phase=Preparing detail=waiting for destination pod ready
[65s] phase=Failed   failureReason=DstNeverReady
      "destination pod \"e2e-guest-mig-…\" never reached Ready within 1m0s budget"
destination pod event: FailedAttachVolume "AttachVolume.Attach failed for volume … failed to attach to node <worker-2>"
Longhorn attachment ticket for the destination: Satisfied=False "waiting for volume to migrate to node <worker-2>"
Longhorn volume: migratable, RWX Block, state=attached on <worker-1>, robustness=degraded, engine replicaModes: 2 × RW
```

- **Cause: the volume was degraded at migration time.** Longhorn does not
  start a live migration of a degraded volume, so the destination's attach
  waits and the controller's 60 s destination budget runs out first.
  - **Attempt 1:** the volume was created at 20:55:59. Its third replica
    (cp-1) was still rebuilding when the migration ran, and the volume became
    healthy only at 21:03:44, 7 m 45 s after creation.
  - **Attempt 2:** the volume was created at 21:18:47 and **never** gets its
    third replica. cp-1's Longhorn disk is `Schedulable=False/DiskPressure`:
    42 GiB of 195 GiB free (21%), below `storage-minimal-available-percentage=25`.
    `replica-soft-anti-affinity` is `false`, so the third replica cannot go
    to another node. That volume is still degraded now.
  - **It still holds:** the V11-R2 guest's volume, created at 21:44, has the
    same shape: 2 replicas running, the cp-1 one unscheduled.
- **No loss.** In both attempts the guest stayed `Running` on its source pod.
  `val-r4-d7b2/e2e-guest` is still running there.
- **The migration path itself works.** Every live migration whose volume was
  healthy completed, each with `observedTransferDuration` set:
  - `v6-b`: 19.676 s;
  - `v3b-mig`: 19.661 s;
  - `v4b-mig`: 19.681 s.

  V3b and V4b waited for a healthy volume before starting.
- **Not a V4a pass.** The criterion is "All checks passed" from the script,
  and that needs the script to run against a volume Longhorn will migrate.
  See "Findings" for what the failure message could say.

`val-r4-d7b` was deleted after its evidence was saved, to free worker-1 CPU
for the second attempt. `val-r4-d7b2` is kept.

### (b) pinned repin: PASS

Guest `val-r4-mig/v4p` (class `small-migratable`, `spec.nodeName: <worker-1>`,
volume healthy with 3 replicas).

- **A first launch attempt failed, on capacity.** Its first launcher was
  rejected by the kubelet with `OutOfcpu`: the pod is pinned, so it skips the
  scheduler, and worker-1 was full at the time. Once `v3b` had migrated off
  worker-1, I deleted that pod. The guest relaunched on worker-1 at 21:31:25.

```text
21:32:54 v4b-mig created → <worker-2>
21:33:10 DestinationPodReady → StopAndCopy; transferring 26 → 52 → 79
21:33:30.906 "destination running; waiting for the source's report"  DestinationRunning=True    <- V5
21:33:31.655 "cutover: completing" (PodRefSwapped=True/CutoverStep1Complete)
21:33:33.018 Completed "destination guest healthy (IP 192.168.99.13)"
             observedTransferDuration=19.681s  observedDowntime=1.691s
21:33:45 spec.nodeName=<worker-2>  status.nodeName=<worker-2>  podRef=v4p-mig-a90fdd (on <worker-2>)
21:33:45 kubectl delete pod v4p-mig-a90fdd
21:33:48 new launcher pod v4p created, node=<worker-2>
21:33:56 pod Running;  21:34:19 guest Running, Guest IP 192.168.99.20
```

- `spec.nodeName` moved from worker-1 to worker-2 at cutover, and the
  relaunched pod runs on worker-2. This is #680.
- **A same-node duplicate Guest IP.** After the relaunch, `v4p`'s Guest IP
  `192.168.99.20` is the same as `innercp`'s, and both run on worker-2:

```text
gpu-cells/innercp   Running <worker-2> GUEST IP 192.168.99.20  POD IP <pod-ip A>  IP SCOPE Pod
val-r4-mig/v4p      Running <worker-2> GUEST IP 192.168.99.20  POD IP <pod-ip C>  IP SCOPE Pod
```

- This is William's round-1 concern, now on one node. Each launcher has its
  own `br0`, so nothing conflicts, and the Pod IP and IP Scope columns tell
  them apart.

## V5: the DestinationRunning wait (#671). PASS

The 0.1–0.2 s poller saw the new phaseDetail between "transferring guest state"
and "cutover: completing", together with `DestinationRunning=True`:
- `v6-b`: 20:49:28.503 → 20:49:29.182;
- `v4b-mig`: 21:33:30.906 → 21:33:31.655.

- **`v3b-mig`** completed with the same event sequence. Its poller also drove
  V3b's patch.
- **Warning events in every `val-r4-*` namespace:** 0 `SourceCompleteMissing`,
  0 `CancelAckTimeout`.

## V6: a migration right after a cancel, to a busy node (#673). PASS

**Setup.**
- Worker-2 is at 4600m of 8 CPU requested (it hosts `innercp`), so it has room
  for one 2-CPU destination but not two.
- Guest `val-r4-mig/v6g` (class `small-migratable`, `spec.nodeName: <worker-1>`,
  `sha-9604992`).
- `v6-a` to worker-2, cancelled 3 s into the transfer. A 0.1 s poller created
  `v6-b` to worker-2 the moment `v6-a` read `Cancelled`.

```text
v6-a 20:48:41.7 "transferring guest state" → 20:48:45.0 cancel → "waiting for cancel acknowledgment" → 20:48:48.9 "deleting destination pod"
     → 20:48:49.2 Cancelled "destination pod deleted after swiftletd cancel ack"   (CancelIssued → Cancelled; no CancelAckTimeout)
v6-b created 20:48:49.294 (0.16 s after v6-a was seen Cancelled)
20:48:49.407 Validating "waiting for terminating pods on the target node to release resources"   Compatible=Unknown/AwaitingTerminatingPods
20:48:51.535 Preparing (Compatible=True)        <- waited 2.1 s for v6-a's destination pod to go
20:48:58.019 StopAndCopy "waiting for the source launcher to finish a previous send"
20:49:09.233 "transferring guest state" 26 → 52 → 79
20:49:28.503 "destination running; waiting for the source's report"   DestinationRunning=True        <- V5
20:49:29.182 "cutover: completing" → 20:49:30.577 Completed
observedTransferDuration=19.676s  observedDowntime=2.514345855s
events v6-b: DestinationPodCreated, Validated, DestinationPodReady, ReceiveIssued, SendIssued, CutoverStep1, Completed
```

- **SourceCompleteMissing / CancelAckTimeout in `val-r4-mig`:** 0 / 0.
- **#680, seen here too:** `v6g` was created pinned to worker-1. After the live
  migration its `spec.nodeName` is `<worker-2>`, and it runs there.

## V7: clonestrategy on Longhorn (#679). PASS

`clonestrategy-test.sh --vsclass longhorn-snapshot-vsc` in `val-r4-cs`:

```text
N=2  MIN_SPEEDUP=3x (informational)
cs-source-snap clone seed: cs-source-snap-clone-seed
copy: 79s 73s  |  snapshot: 142s 77s
cs-snap-1: root PVC dataSource VolumeSnapshot;  cs-snap-2: root PVC dataSource VolumeSnapshot
speedup 0.6x (threshold 3x, informational) — "Expected on a CSI driver that implements snapshot+dataSource as a full copy (e.g. Longhorn)… Not a failure unless --require-speedup is given."
=== clone-strategy e2e PASS (snapshot path used; speedup informational) ===
```

`val-r4-cs` was deleted afterwards and was gone in 22 s.

## V8: snapshot namespace deletion (#675). PASS

- `local-roundtrip-test.sh --namespace val-r4-snap --no-cleanup`: PASS. The
  sentinel survived, and the capture was on worker-1 at
  `/var/lib/kubeswift/snapshots/val-r4-snap_snapshot-local-mem`, with the
  `snapshot-hostpath-cleanup` finalizer.
- Then `kubectl delete ns val-r4-snap`. A `kubectl debug node/<worker-1>` pod
  listed the directory before and after:

```text
20:27:43 before: drwxr-xr-x  val-r4-snap_snapshot-local-mem
20:27:45 kubectl delete ns val-r4-snap
20:27:59 (+14s) cleanup pod kubeswift-system/swift-snap-cleanup-snapshot-local-mem-560b34d51b Pending@<worker-1>  (image busybox:1.36.1, container rm)
20:28:02 (+17s) cleanup pod gone (deleted after it succeeded)
20:28:04 (+19s) namespace val-r4-snap GONE
20:28:04 after: the directory no longer exists
```

## V9: sandbox kernel checks (#674)

**(a) PASS.**
- **Setup:** a SwiftSandbox `val-r4-nok/v9-sbx`, in a namespace with no
  SwiftKernel named `sandbox`. The image is `public.ecr.aws/docker/library/alpine:3.20`
  rather than Docker Hub, which is rate-limiting this cluster.

```text
20:23:03 phase=Pending  Resolved=False reason=KernelNotFound "no SwiftKernel named \"sandbox\" in namespace \"val-r4-nok\""   pods: 0
20:23:23 SwiftKernel val-r4-nok/sandbox created (the sample)
20:23:28 kernel=Pulling  sandbox Resolved=False/KernelNotFound → 20:23:33 KernelNotReady
20:23:37 kernel=Ready
20:23:45 launcher pod v9-sbx created (the first pod of any kind for the sandbox)
20:23:48 Materializing, Resolved=True → 20:23:59 Running → 20:24:10 Completed, exitCode 0
launcher: intent_loaded kernel=/var/lib/kubeswift/kernels/val-r4-nok/sandbox/bzImage (the new <ns>/<name> layout)
```

- **Minor:** the phase read `Running`, then `Materializing` again for about 4 s
  (20:24:06), then `Completed`. The status goes back one step at the end.

**(b) SKIPPED.** Worker-2's only GPU (`0000:01:00.0`) is `allocated: true,
allocatedTo: sandbox:field-testing/ft-gpu-pool-slot-dfjtk`.
- That is the pre-existing warm pool's slot pod, which has been `Failed` since
  2026-09-24 21:42 (`Cannot open initramfs file`, the old #659/#660).
- There is no free GPU, and freeing it would mean touching `field-testing`.
- **Worth a look:** a slot pod that failed a day ago still holds the node's only
  GPU allocation.

## V10: kernel boot after the re-pull (#677). PASS

- **Smoke `kernel-boot`:** PASS. The launcher loaded
  `/var/lib/kubeswift/kernels/val-r4-smoke/faas-minimal/bzImage` + `rootfs.cpio.gz`.
- **SwiftSandbox `v9-sbx`:** `Completed`, exit 0, from
  `/var/lib/kubeswift/kernels/val-r4-nok/sandbox/`.
- **`Cannot open initramfs file`:** 0 lines, in every `val-r4-smoke` launcher
  and in `v9-sbx`.

## V11: regression

### R2: in-place restore. PASS

`local-roundtrip-test.sh --namespace val-r4-rt --no-cleanup` (21:42:39–21:46:22):

```text
Captured on node <cp-1> (pause window 4235ms)
OK: launcher pod is in restore-receive mode, no stager (in-place fast path)   launcher swiftletd:sha-9604992
OK: sentinel survived: kubeswift-roundtrip-1790372758-30467
=== Tier B round-trip e2e PASS ===
```

| | Value |
|---|---|
| `primaryIP` before, snapshot `guestSpec.primaryIP`, after | `192.168.99.19` / `192.168.99.19` / `192.168.99.19` |
| SwiftRestore | created 21:46:14 → `Ready=True/RestoreReady` at 21:46:20 = **6 s** (Restoring → Resuming → Ready) |
| Snapshot | `snapshot-local-mem`, finalizer `kubeswift.io/snapshot-hostpath-cleanup` |

- The address is kept, and the restore takes more than ~5 s, as in rounds 1 and 2.
- **The pause window was 4.2 s**, against 1.7 s in round 2 and in V8's run of
  the same script. This capture ran on cp-1, whose disk is nearly full (see V4).
- V8 had already run this script (on worker-1): PASS, sentinel kept, the
  address `192.168.99.10` before and after. This run adds the restore timing.

### T3 and T5: NOT RUN (blocked)

- **What they need.** Each needs a new `val-migratable-16g` guest live-migrated
  to another node:
  - T3: cancel 15 s into the transfer;
  - T5: three attempts racing completion.
- **Why they cannot run.** With cp-1's Longhorn disk below its threshold, such
  a guest's volume gets 2 of 3 replicas and stays degraded. Longhorn then
  refuses the migration, as in V4a live. The run would fail with
  `DstNeverReady` before the transfer that T3 and T5 test.
- **The guest reuse option does not fit.** The one healthy migratable guest
  left (`v4p`) is 2 GiB, which is too small for T3's 10 GiB tmpfs.
- **Partial cover.** V6 cancelled a live migration mid-transfer on this build
  (`v6-a`, graceful path, no `CancelAckTimeout`). But it did not check T3's
  source-side criteria (`migration-status` turning `failed`, the
  `migration_send_failed` detail). So T3 is not claimed.

## Findings

1. **A live migration of a degraded Longhorn volume fails after 60 s with a
   message that does not name the cause** (V4a live).
   - The SwiftMigration says only `DstNeverReady: destination pod … never
     reached Ready within 1m0s budget`. The reason is in the destination pod's
     `FailedAttachVolume` event and Longhorn's attachment ticket ("waiting for
     volume to migrate to node").
   - Validation could check the volume's robustness first, or the failure could
     carry the attach event. A fresh volume is degraded for minutes while
     Longhorn builds its replicas (7 m 45 s here), so this hits anyone who
     migrates a new guest soon after creating it.
   - The guest was never at risk: it stayed on its source.
2. **A new guest reads `Failed` while its SwiftImage imports** (V11-R2).
   - `val-r4-rt/snapshot-local-source` was `Failed` from 21:42:54 to 21:44:15.
     The controller logged `resolution failed … reason="SwiftImage not Ready"`.
     It then went `Scheduling` and booted normally.
   - A recoverable wait is shown as a terminal phase. I did not check whether
     this predates the candidate.
3. **Two guests on one node can share a Guest IP** (V4b). `v4p` and `innercp`
   both have `192.168.99.20` on worker-2. The #676 columns tell them apart,
   but William's round-1 concern stands.
4. **A sandbox's phase goes back one step before `Completed`** (V9a):
   `Running` → `Materializing` for about 4 s → `Completed`.
5. **A failed warm-pool slot still holds the node's only GPU** (V9b).
   `field-testing/ft-gpu-pool-slot-dfjtk` has been `Failed` since 2026-09-24
   and still holds the GPU allocation.
6. **Minor: guest PDBs log `CalculateExpectedPodCountFailed`.** Every guest's
   PodDisruptionBudget (`maxUnavailable: 0`) reports `SyncFailed`: "swiftguests
   … does not implement the scale subresource". `disruptionsAllowed` is 0
   either way, so eviction is still blocked as intended. It is event noise,
   seen on the 9-day-old `innercp` PDB too, so not new.

## Environment notes (dev)

- **cp-1's Longhorn disk is below its free-space threshold:** 42 GiB of
  195 GiB (21%), with `storage-minimal-available-percentage=25`.
  - This blocks V4a live, T3 and T5.
  - Deleting the finished `val-r4-mig` guests (`v3a`, `v3b`, `v4p`, about
    3.5 GiB each on cp-1) would bring it to about 27%. That is enough for
    roughly one more fresh volume.
  - A lasting fix is William's: free space on cp-1, or change the Longhorn
    setting.
- **Worker-1 and worker-2 CPU is tight.**
  - Worker-2 is at 6600m of 8000m: `innercp`, plus `v4p` since V4b.
  - Worker-1 is at 4440m, with `val-r4-d7b2/e2e-guest` still running there.
  - Worker-2 has no room for a 2-CPU destination now. Worker-1 has room for
    one.

## State left on dev

- **Controller:** chart `0.0.0-dev.9604992` (rev 47), `--metrics-secure=true`.
- **Namespaces kept** (not every scenario passed):
  - `val-r4-smoke`: V1 cleanup ran, so it holds no guests.
  - `val-r4-nok`: the SwiftKernel `sandbox`.
  - `val-r4-mig`:
    - `v3a` and `v3b`, both `Stopped`;
    - `v4p`, `Running` on worker-2;
    - SwiftMigrations `v3b-mig`, `v4b-mig`, `v6-a`, `v6-b`.
  - `val-r4-d7b2`:
    - `e2e-guest`, `Running` on worker-1, with the degraded volume;
    - SwiftMigration `e2e-mig`, `Failed`.
  - `val-r4-rt`:
    - `snapshot-local-source`, `Running` on cp-1;
    - SwiftSnapshot `snapshot-local-mem` (local, with the cleanup finalizer);
    - SwiftRestore `snapshot-local-inplace`.
- **Deleted during the run:**
  - `val-r4-cs` (22 s) and `val-r4-snap` (19 s, the V8 check);
  - `val-r4-d7a`, by the script;
  - `val-r4-d7b`.
- **Unchanged:** `gpu-cells/innercp` (uid `10bd28b8-…`, 0 restarts), and
  `default/ubuntu-noble` (uid `ed9f5a44-…`, rv `11410`).
- **Nodes:** none cordoned.
- **Lab change from Phase 1** (authorised): `kube-system/kube-multus-ds` memory
  100Mi/500Mi, and `cp -f` in its init container.
