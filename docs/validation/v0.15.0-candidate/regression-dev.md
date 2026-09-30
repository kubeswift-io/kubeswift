# Regression run, Phase B: dev on v0.15.0

Run 2026-09-30, 09:33–11:02 UTC, after Phase A (`regression-a.md`).
- **Setup.** Scripts ran from a worktree at `v0.15.0` (`37b8c27`). Node names
  are generalised (`cp-1`, `worker-1`, `worker-2`), and pod IPs and guest
  MACs are redacted.
- **Launchers.** Every guest was created in this run, and every launcher ran
  `swiftletd:v0.15.0`.

## Summary

| # | Scenario | Verdict |
|---|---|---|
| G1 | Smoke, all scenarios | **PASS**: 5/5 (`gpu-alloc` passes on its mock node), 0 WARN lines; cleanup removed only the run's objects |
| G2 | Guest and pod IPs | **PASS** |
| G3 | Cross-node TCP | **PASS** |
| G4 | Kernel boot, no initramfs errors | **PASS** |
| G5 | Guest before image, and in-place restore | **PASS**: never `Failed`; restore 9 s; the D4 handle is right |
| G6 | Failed image, then recreated | **PASS**: exactly one `ResolutionFailed` event; the image `Failed` after 364 s |
| G7 | Other missing references wait | **PASS** (boot slower under load; see timings) |
| G8 | A pool created with its image does not churn | **PASS**: exactly 2 SwiftGuests (boot slower under load) |
| G9 | Clone restore (Tier B) | **BLOCKED-ENV**: clone A passes; clone B cannot be scheduled on the capture node (CPU), as in round 1's D3 |
| G10 | Tier A CSI snapshot, and clonestrategy | **PASS** / **PASS** (speedup 0.7x, informational) |
| G11 | Bad snapshot hostPath | **PASS**: refused at admission |
| G12 | A namespace holding a snapshot deletes | **PASS**: gone in 18 s |
| G13 | runPolicy Stopped | **PASS**: (a) graceful, `Stopped` at +5.2 s; (b) `StopDeferred`, the migration completes, then a graceful stop |
| G14 | Migration script, offline and live | **Offline FAIL (FINDING B1)**: the script's class is rejected by the CRD. **Live PASS.** A supplementary offline run without `--storage-class` passes |
| G15 | A pinned guest follows its migration | **PASS** |
| G16 | A migrated guest reports its stop | **PASS** (third attempt; see G16) |
| G17 | Cancel mid-transfer (T3) | **PASS**: source `failed` 24.9 s after the cancel |
| G18 | A migration right after a cancel, to a busy node (V6) | **PASS**: waited 2.1 s (`AwaitingTerminatingPods`), then completed |
| G19 | Cancel racing completion, three attempts (T5) | **PASS**: one VM survives each attempt; the `DestinationRunning` cancel gives `CancelIgnored` |
| G20 | Sandbox phases and a lost slot | **PASS** |
| G21 | A sandbox with no kernel waits | **PASS** |
| G22 | Launcher failure messages | **Not observed**: no sandbox or slot launcher failed |
| G23 | TokenRequest gate | **PASS** |
| G24 | Gateway exec gate | **PASS** |
| G25 | Secure metrics | **PASS**: 400 / 401 / 403 / 200; the Prometheus target is `up` with 226 `kubeswift_` series |

**No guest was lost.** No namespace failed to delete, and every `val-reg-*`
namespace is gone. G9's was deleted after its evidence was saved, to free
worker-1 for later scenarios.

## Findings

**B1: the migration script's offline class is rejected by the
SwiftGuestClass CRD.**
- **Pre-existing, not a v0.15.0 regression.** The rule dates from
  2026-04-30 (`ed0f524`) and is in v0.13.15, v0.14.0 and v0.14.1.
- **The symptom.** `migration-test.sh --mode offline --storage-class
  longhorn-migratable` builds `storage: {storageClassName: …}`, and the API
  server refuses it:

```text
The SwiftGuestClass "migration-e2e-offline" is invalid: spec.storage: Invalid value: "object": no such key: accessMode
evaluating rule: accessMode=ReadWriteMany requires volumeMode=Block; Filesystem RWX is not live-migration-capable
```

- **The cause.** The rule is `!(self.accessMode == 'ReadWriteMany' &&
  (!has(self.volumeMode) || self.volumeMode == 'Filesystem'))`, with no
  `has(self.accessMode)` guard. Server dry-runs:
  - `{storageClassName}`: **rejected**.
  - `{storageClassName, volumeMode: Block}`: accepted, because the right
    side short-circuits.
  - `{storageClassName, accessMode: ReadWriteOnce}`: accepted.
  - `{…, accessMode: ReadWriteMany, volumeMode: Block}`: accepted.
- **Who hits it.** Any user class that sets only `storageClassName` gets
  this unhelpful error. No shipped sample hits it.
- **The offline migration path itself works.** G14b is the same test without
  `--storage-class`, and it passes.

**B2: `swiftctl start` does not restart a guest that powered off from inside.**
- **The symptom.** After the in-guest poweroff (G16's first attempt), the
  guest reads `Stopped`, with `runPolicy: Running` and its launcher pod
  `Succeeded`.
- **What `start` does.** It only sets `runPolicy=Running`, which was
  already set, so it changes nothing. It still prints "controller will
  recreate the pod", and the guest stayed `Stopped` for 5 minutes.
- **What works.** `swiftctl restart`, which deletes the pod, brought the
  guest back at once.
- **Minor.** It is a message and UX gap.

**B3: controller error noise while a namespace with guests is deleted.**
- **The volume.** 305 of dev's 317 `Reconciler error` lines were the
  controller trying to create launcher pods, ServiceAccounts, RoleBindings,
  ConfigMaps or kernel-pull Jobs in a namespace being deleted ("unable to
  create new content in namespace … because it is being terminated"). That
  is about 10–30 lines per test namespace deletion.
- **Harmless.** Every namespace finished deleting, in 6–55 s.
- **Not new, as far as is known.** Earlier rounds did not count errors per
  deletion, so this is not flagged as new.

**B4: a migration refused at validation records no event** (seen again).
- **The two refusals.** Both were G16 attempts, refused for insufficient CPU
  on the target.
- **Status and metrics say so, events do not.** They carry only
  `Ready=False/MigrationFailed`, with no Warning event. They did increment
  `kubeswift_migration_total{result="failed"}` (now 2), which fired
  `KubeSwiftMigrationFailures`. That alert is **expected** from those two
  refusals.
- This is round 7's finding 2 again.

**Timings more than 50% slower, all under parallel load** (see the comparison
table):
- **What was slower.**
  - G7: class → IP 120 s (round 6: 73 s).
  - G8: both replicas `Running` in 5 m 29 s (round 6: 2 m 55 s).
  - G10b: `cs-copy-1` 144 s (round 4: 79 s), `cs-snap-2` 143 s (round 4: 77 s).
- **Why these are not comparable.** Each ran while other scenarios were
  importing or cloning images on Longhorn at the same time:
  - G5, G7 and G8 together;
  - G10b together with G15/G16.
  - In every case the time went into the image import and the root-disk
    clone, not into KubeSwift's own steps.
- **Not claimed as regressions.** The migration, restore and cancel timings,
  which do not depend on Longhorn copies, match earlier rounds closely.

**Pre-existing alert, not v0.15.0: `CPUThrottlingHigh` on `gpu-discovery`.**
- **It predates this build.** It has fired for every `gpu-discovery` pod
  since at least 2026-09-25, all on dev images. Throttling runs at 56–71%
  against its 100m CPU limit; the new pod is at 60%.

**My harness errors:**
- In G16's first attempt, my script did not stop when the migrate-back was
  refused. It powered the guest off from its relaunched, non-migrated
  launcher, which leaves nothing valid to judge R7 on.
- In G25, my first pod-IP lookup used a wrong label; its fallback worked.

## Timing comparison

| Scenario | This run | Earlier |
|---|---|---|
| G1 smoke, 5 scenarios | 11 m 11 s | round 6: 13 m 27 s |
| G5 in-place restore | 9 s (pause window 1663 ms) | round 6: 7 s (1114 ms); round 2: 9 s (1674 ms) |
| G6 404 image → `Failed` | 364 s | round 6: 374 s |
| G6 recreated image → guest `Running` | 3 m 15 s | round 6: 2 m 33 s (+27%) |
| G7 class created → `Scheduling` | 8 s | round 6: 1 s |
| G7 class created → IP | 120 s ⚠ | round 6: 73 s (+64%, load) |
| G8 apply → both replicas `Running` | 5 m 29 s ⚠ | round 6: 2 m 55 s (+88%, load) |
| G10b copy / snapshot boots | 144, 72 / 145, 143 s ⚠ | round 4: 79, 73 / 142, 77 s (load); speedup 0.7x (round 4 0.6x) |
| G12 namespace with a snapshot → gone | 18 s | round 4: 19 s |
| G13a patch → `Stopped` | 5.2 s | round 4: 6 s |
| G13b migration transfer / downtime | 19.828 s / 1.34 s | round 4: 19.661 s / 1.97 s |
| G14 live transfer / downtime | 19.783 s / 2.02 s | round 7: 19.66 s / 2.28 s |
| G14b offline migration | 54 s | round 4: 53 s |
| G15 transfer / downtime | 28.522 s / 1.70 s | round 4: 19.681 s / 1.69 s (+45%, under G10b load) |
| G16 transfer / downtime | 19.785 s / 1.63 s | n/a |
| G17 cancel → source `failed` | 24.9 s | round 4: 24.0 s; round 3: 25.1 s |
| G18 wait in Validating | 2.1 s | round 4: 2.1 s |
| G18 `v6-b` transfer (10 GiB) / downtime | 2 m 35.725 s / 2.27 s | round 4 T5-t3: 2 m 35.583 s / 1.81 s |
| G19 t1 / t2 cancel → source `failed` | 26.8 s / 26.5 s | round 4: 27.9 s / 27.3 s |
| G19 t3: cancel after destination ran; source `complete` after it | 0.44 s; 2.65 s | round 4: 0.49 s; 2.7 s |
| G19 t3 transfer / downtime | 2 m 35.533 s / 1.42 s | round 4: 2 m 35.583 s / 1.81 s |
| G19 gaps between attempts | 22.2 s, 22.5 s | round 4: 23.5 s, 22.6 s |
| G20 `SlotLost` after the force-delete; new slot running | 0.3 s; 4.3 s | round 5: 0.3 s; 4.4 s |
| G20 one-shot create → `Completed` | 8.1 s | round 5: 15.7 s |
| G21 kernel `Ready` → launcher pod | 6 s | round 4: 8 s |
| G25 `kubeswift_` series scraped | 226 | round 2: 130 |

**Warning events and phaseDetails.**
- **No new Warning event kinds.** Besides `ResolutionFailed` (once, in G6,
  as designed), the Warnings were:
  - `FailedScheduling "unbound immediate PersistentVolumeClaims"`, transient,
    while a PVC was provisioned;
  - the PodDisruptionBudget's `CalculateExpectedPodCountFailed`, round 4's
    finding 6.
- **No new phaseDetails.** They were `cutover: deleting source pod`,
  `Resuming "waiting for guest health on destination"`, and `waiting for the
  source launcher to finish a previous send`. All were seen in earlier
  rounds.

## B1: boot and networking

**G1: smoke** (`NAMESPACE=val-reg-smoke make smoke-test`, 09:33:16–09:44:27):

```text
disk-boot    PASS  hypervisor=cloud-hypervisor  primaryIP=192.168.99.13
kernel-boot  PASS  hypervisor=cloud-hypervisor  primaryIP=192.168.99.17
qemu-boot    PASS  hypervisor=qemu              primaryIP=192.168.99.18
gpu-alloc    PASS  Mock SwiftGPUNode <cp-1> (no GPU there); GPUAllocated=True   (the real GPU stays with ft-gpu-pool)
multi-nic    PASS  primaryIP=192.168.99.12
=== All scenarios PASSED ===   WARN lines: 0   launchers: swiftletd:v0.15.0 (all four)
```

- **Cleanup** (`make smoke-test-cleanup`) left `val-reg-smoke` empty. The
  namespace was then deleted in 8 s.
- **Nothing else changed.** The inventory was identical before and after:
  - the SwiftGuestClasses and SwiftGPUNodes;
  - everything in `default`;
  - `innercp`'s uid and resourceVersion.
- **`default/ubuntu-noble`** no longer exists (deleted in round 5), so it was
  not there to touch.

**G2: guest and pod IPs**, while G1's guests ran:
- **The columns:** `Guest IP` and `Pod IP` by default; `-o wide` adds `IP
  Scope`.
- **Each running smoke guest:** `primaryIPScope=Pod`, and `podIP` equals its
  launcher's `status.podIP`, on `swiftletd:v0.15.0`.
- **Two nat guests share `192.168.99.12`:** `val-reg-img/r6b` and
  `val-reg-smoke/multi-nic-test`, with different Pod IPs.

**G3: cross-node TCP.**
- **The run:** `NS=val-reg-b0 SWIFTGUESTCLASS=small
  test/networking/b0-cross-node-tcp.sh validate`, using the `faas-minimal`
  kernel from the sample.
- **Result:** `==> ALL CHECKS PASSED`, launcher `swiftletd:v0.15.0`. Cleanup
  passed, and the namespace was deleted in 41 s.

**G4: kernel boot.**
- **The smoke guest:** `faas-test` loaded
  `kernel=/var/lib/kubeswift/kernels/val-reg-smoke/faas-minimal/bzImage` and
  `initramfs=…/rootfs.cpio.gz`.
- **The sandbox:** G21's loaded `…/kernels/val-reg-nok/sandbox/bzImage`.
- **`Cannot open initramfs`:** 0 lines in every smoke launcher and in the
  sandbox.

## B2: images and references

**G5: guest before its image, plus the in-place restore**
(`local-roundtrip-test.sh --namespace val-reg-rt --no-cleanup`, 09:45:52–09:51:49):

```text
controller: 2 × "waiting to resolve" reason="SwiftImage not found: …", then 31 × "waiting to resolve" reason="SwiftImage not Ready"
09:45:55 Pending  Resolved=False "SwiftImage not Ready"    (never Failed; no Warning event on the guest)
09:48:53 Scheduling;  09:51:01 Running;  09:51:24 primaryIP 192.168.99.11
Captured on node <worker-1> (pause window 1663ms); in-place fast path; sentinel survived; === Tier B round-trip e2e PASS ===
SwiftRestore created 09:51:38 -> Ready=True/RestoreReady 09:51:47 (9 s); primaryIP 192.168.99.11 before, in the snapshot, after
D4: status.memorySnapshot.handle = /var/lib/kubeswift/snapshots/val-reg-rt_snapshot-local-mem
```

**G6: a failed image, then a recreated one** (`val-reg-img`):

```text
09:33:42 apply img404 (404 URL) + r6b;  09:33:44 r6b Pending "SwiftImage not Ready", img404 Importing
09:39:44 img404 Failed/ImportFailed "Job has reached the specified backoff limit"   (364 s)
         r6b Failed  Resolved=False "SwiftImage failed: Job has reached the specified backoff limit"
         event: Warning ResolutionFailed x1 "SwiftImage failed: Job has reached the specified backoff limit";  launcher pods: 0
09:39:51 delete img404; apply a working img404 (the same name)
09:43:06 r6b Running, primaryIP 192.168.99.12, swiftletd:v0.15.0;  still exactly one ResolutionFailed event
```

- **R8.** The image stayed `Importing` until the Job gave up, then went
  `Failed`. It took 364 s here; round 2 said about 10 minutes.

**G7: other missing references wait** (`val-reg-ref`, with an image already
`Ready`):

```text
09:48:38 apply r6c (class val-reg-class and seed r6-seed both missing)
09:48:40 Pending  Resolved=False "SwiftGuestClass not found: … \"val-reg-class\" not found"   (no launcher)
09:49:08 seed created (message unchanged; the class is checked first);  09:49:38 class created
09:49:47 Scheduling;  09:50:59 launcher;  09:51:19 Running;  09:51:41 IP   (swiftletd:v0.15.0; no Warning event on the guest)
```

The class `val-reg-class` was deleted afterwards.

**G8: a pool created with its image** (`val-reg-pool`, one apply at 09:45:56):

```text
09:45:58 r6f-0[3ad68d2f], r6f-1[dec85b98]: Pending "SwiftImage not Ready"   (image Importing)
09:48:53 image Ready -> both Scheduling;  09:51:05 r6f-1 Running;  09:51:25 r6f-0 Running (ready 2/2)
distinct SwiftGuest objects ever: 2;  Failed ever: 0;  pool events: none;  launchers: swiftletd:v0.15.0
```

## B3: snapshots and restore

**G9: clone restore (Tier B). BLOCKED-ENV**
(`local-clone-identity-test.sh --namespace val-reg-clone --no-cleanup`, 10:08:30–10:19:17):

```text
source Running on <worker-1>; Tier B memory snapshot Ready
snapshot-local-clone-a: GuestRunning=True after 126 s; SwiftRestore Ready after 131 s     (round 1: 126 s / 128 s)
snapshot-local-clone-b: FAIL never reached GuestRunning=True
  pod nodeSelector kubernetes.io/hostname=<worker-1> (the capture node), cpu request 2
  PodScheduled=False "0/3 nodes are available: 1 Insufficient cpu, 2 node(s) didn't match Pod's node affinity/selector"
  <worker-1> requests 6390m of 8000m with the source and clone A
```

- **Why it cannot pass here.** The clones are pinned to the node holding the
  local snapshot. That node cannot hold three 2-CPU guests on top of its
  ~2.4 CPU of system pods, and no dev node could. This is round 1's D3.
- **Status stays honest.** Clone B reads `Pending`, and its restore
  `Restoring`, not `Failed`.
- **Deleted.** The namespace was deleted after its evidence was saved, to
  free worker-1.

**G10a: Tier A CSI snapshot**
(`NAMESPACE=val-reg-snapa snapshot-test.sh --vsclass longhorn-snapshot-vsc`, 10:01:26–10:07:04):
- The VolumeSnapshot `swift-snap-snapshot-e2e-snap` was `readyToUse`, with a
  matching handle.
- The SwiftRestore was `Ready`, and the restored guest runs.
- `=== snapshot e2e PASS ===`, with both launchers on `v0.15.0`.

**G10b: clonestrategy** (`NAMESPACE=val-reg-cs … --vsclass longhorn-snapshot-vsc`, 10:18:49–10:30:25):

```text
copy: cs-copy-1 144 s, cs-copy-2 72 s;  snapshot: cs-snap-1 145 s, cs-snap-2 143 s
cs-snap-1, cs-snap-2: root PVC dataSource VolumeSnapshot;  cs-source-snap clone seed: cs-source-snap-clone-seed
speedup 0.7x (threshold 3x, informational) — "Expected on a CSI driver that implements snapshot+dataSource as a full copy (e.g. Longhorn)"
=== clone-strategy e2e PASS (snapshot path used; speedup informational) ===   launchers: swiftletd:v0.15.0 (all four)
```

**G11: bad snapshot hostPath** (webhook on):

```text
Error from server (Forbidden): admission webhook "vswiftsnapshot.snapshot.kubeswift.io" denied the request:
spec.backend.local.hostPath must be omitted or be /var/lib/kubeswift/snapshots/val-reg-hp_val-reg-bad-hostpath,
the directory derived from the snapshot's namespace and name (got "/var/lib/kubeswift/snapshots/elsewhere")
objects created: 0
```

**G12: a namespace holding a snapshot deletes** (`val-reg-snap`, after a
round trip that passed with a 1677 ms pause window):

```text
09:58:34 before: drwxr-xr-x /var/lib/kubeswift/snapshots/val-reg-snap_snapshot-local-mem   (<worker-1>; finalizer kubeswift.io/snapshot-hostpath-cleanup)
09:58:37 kubectl delete ns val-reg-snap
09:58:49 (+12 s) cleanup pod kubeswift-system/swift-snap-cleanup-snapshot-local-mem-<hash> Pending@<worker-1>
09:58:53 (+16 s) cleanup pod gone
09:58:55 (+18 s) namespace gone;  after: "No such file or directory"
```

## B4: runPolicy

**G13 (a):** `val-reg-rp/v3a` on cp-1.

```text
10:40:47.235 patch runPolicy=Stopped
10:40:47 SwiftGuest event Stopping "runPolicy is Stopped; deleted launcher pod v3a, the guest shuts down within its termination grace period"
10:40:47.400 launcher: sigterm_received; requesting guest ACPI power-off -> guest_poweroff_requested
10:40:51.200 guest firmware "ResetSystem2: ResetType Shutdown";  10:40:51.326 vm_stopped_gracefully
10:40:52.428 (+5.2 s) phase=Stopped
```

**G13 (b):** `val-reg-rp/v3b`, live migration `g13b-mig` worker-1 → worker-2.
The guest was patched to `Stopped` once "transferring guest state" appeared.

```text
10:41:08 SendIssued / ReceiveIssued;  10:41:10.781 runPolicy=Stopped patched
10:41:10 SwiftGuest StopDeferred "runPolicy is Stopped; waiting for SwiftMigration g13b-mig to finish before stopping the guest"
10:41:29.614 destination: dispatch_migration_receive_complete state=Running;  10:41:31 Completed (transfer 19.828 s, downtime 1.34 s)
10:41:32 SwiftGuest Stopping "deleted launcher pod v3b-mig-824ee2" (the destination);  10:41:32.460 sigterm -> ACPI power-off
10:41:34.446 vm_stopped_gracefully;  10:41:35 phase=Stopped
```

## B5: live migration

**G14: the migration script.**
- **Offline, as specified: FAIL (B1).** It failed at 09:53:57, before any
  guest existed: the SwiftGuestClass was rejected, and nothing was created.
- **Live: PASS** (`val-reg-g14live`, 09:53:59–10:00:30):

```text
Launcher scheduled on <worker-1>; the other nodes are uncordoned;  uptime before 25s
Waiting for Longhorn volume … to be healthy (max 15min)... / Longhorn volume … is healthy
Migration completed in 43s (resolved mode: live); post-migration pod on <worker-2>
PASS: disk sentinel; PASS: tmpfs sentinel and uptime (25s -> 169s); PASS: webhook rejected migration.enabled=false; All checks passed.
transfer 19.783 s, downtime 2.02 s;  SourceCompleteMissing 0
phaseDetail (0.2 s): … 10:00:15.646 "destination running; waiting for the source's report" (DestinationRunning) -> 10:00:16.372 "cutover: completing" -> Completed
```

- **G14b, a supplementary offline run without `--storage-class`: PASS**
  (`val-reg-g14off`, 10:01:26–10:06:18). The guest was placed on `--source`,
  the migration completed in 54 s, the disk sentinel survived, "All checks
  passed", and no node was left cordoned.

**G15: a pinned guest follows its migration.**
- **The guest:** `val-reg-pin/pin15`, `spec.nodeName: <worker-1>`, class
  `small-migratable`.

```text
10:24:53 Running on <worker-1>, swiftletd:v0.15.0, volume healthy
g15-mig -> <worker-2>: … 10:25:38.202 "destination running; waiting for the source's report" -> 10:25:39.919 Completed (transfer 28.522 s, downtime 1.70 s)
10:25:40 spec.nodeName=<worker-2>  status.nodeName=<worker-2>
10:25:40.840 delete launcher pin15-mig-3d52f9 -> 10:25:44 new pod pin15@<worker-2> Pending -> 10:25:57 Running -> 10:26:31 IP 192.168.99.13
```

**G16: a migrated guest reports its stop. PASS on the third attempt.**
1. **The first attempt was invalid.** `g16-mig` (back to worker-1) was
   refused at validation, "target node <worker-1> has insufficient CPU
   headroom: need 2, have 610m": G10b's guests were running. My script did
   not stop, and it powered the guest off from its **non-migrated**
   relaunched launcher. The guest then stayed `Stopped` (B2) until
   `swiftctl restart`.
2. **The second attempt, `g16b-mig` to cp-1**, was refused the same way:
   "have 520m (allocatable 8, used 7480m)".
3. **The third attempt, `g16c-mig` to cp-1**, ran once G10b's namespace was
   deleted:

```text
10:35:17 Completed (transfer 19.785 s, downtime 1.63 s); migrated launcher pin15-mig-d0f962 on <cp-1>
env KUBESWIFT_GUEST_NAME=pin15;  launcher log: w16_guest_running_reported guest=val-reg-pin/pin15
10:35:21.791 ssh … sudo systemctl poweroff
10:35:28 GuestRunning=False reason=VmStopped (pod still Running) -> 10:35:29 phase=Stopped GuestRunning=False/LauncherExited, pod Succeeded
report_failed … not found: 0 lines
```

This is the same shape as round 2's R7: `VmStopped`, then overwritten by
`LauncherExited` once the pod is `Succeeded`.

**G17–G19** ran on `val-reg-mig/t3g`:
- class `val-migratable-16g` (2 CPU, 16 GiB, RWX Block), pinned to cp-1 at
  creation, `swiftletd:v0.15.0`;
- a 10 GiB random-data tmpfs, and sentinel `VAL-REG-T3-1790765156`.

**G17 (T3), to worker-2**, cancelled at "transferring guest state", progress 9:

```text
10:47:17.962 cancelRequested=true;  CancelIssued -> "waiting for cancel acknowledgment"
10:47:18.387 source pod: migration-action-id gone (0.4 s after the cancel)
10:47:22.385 Cancelled "destination pod deleted after swiftletd cancel ack"   (no CancelAckTimeout)
10:47:22.417 source CH "Migration failed: … Connection reset by peer"
10:47:42.580 migration_send_failed id=t3-cancel:send:1 detail=the migration connection to <dst-ip>:6789 closed, and the source guest is still running …
10:47:42.942 source migration-status=failed   (24.9 s after the cancel)
10:47:46 same source pod uid, 0 restarts; sentinel present; uptime 74 -> 137 s over ~63 s; SSH works
```

**G18 (V6).** The rewriter was stopped first.
- **The busy node.** Worker-2 (4600m requested) has room for one 2-CPU
  destination but not two.
- **The chase.** `v6-a` to worker-2 was cancelled 3 s into the transfer,
  and `v6-b` was created 0.15 s after `v6-a` read `Cancelled`.

```text
10:48:14.785 v6-b created
10:48:14.914 Validating "waiting for terminating pods on the target node to release resources"  Compatible=Unknown/AwaitingTerminatingPods
10:48:16.992 Preparing (2.1 s wait)
10:48:24.833 StopAndCopy "waiting for the source launcher to finish a previous send";  10:48:35.223 "transferring guest state" 3 -> 95
10:51:08.224 "destination running; waiting for the source's report" DestinationRunning;  10:51:11.293 "cutover: completing"
10:51:12.378 Completed   transfer 2m35.725s, downtime 2.27 s
after: t3g on <worker-2>, spec.nodeName moved <cp-1> -> <worker-2> (#680); sentinel present; uptime 344 s
```

**G19 (T5), from worker-2 to worker-1, on the static 10 GiB:**

| | t1 (progress ≥ 90) | t2 (~145 s into the transfer) | t3 (on `DestinationRunning`) |
|---|---|---|---|
| Created / source `action_accept` | 10:51:19 / 10:51:34.3 | 10:54:22 / 10:54:35.6 | 10:57:27 / 10:57:41.5 |
| Cancel | 10:53:54.838 (progress 92) | 10:57:01.327 (progress 95) | 11:00:14.835 |
| Outcome | `Cancelled` 10:53:59.6, graceful ack | `Cancelled` 10:57:04–05, graceful ack | **`CancelIgnored`** ("received post-cutover; migration cannot be reversed"), then `Completed` 11:00:18.5 |
| Source `migration_send_failed` / `failed` | 10:54:21.497 / 10:54:21.611 (**+26.8 s**) | 10:57:27.371 / 10:57:27.857 (**+26.5 s**) | `dispatch_migration_send_complete` 11:00:17.048; `w23_terminal_write_signal_fired completed=true` 11:00:17.078, before `…received` |
| Survivor | source; sentinel present; uptime 512 s | source; uptime 697 s | destination on worker-1; uptime 891 s |
| Gap before the next attempt | 22.2 s | 22.5 s | |

- **t3 in detail.** The destination reported `running` at 11:00:14.398, and
  the cancel came 0.44 s later. The source reported `complete` 2.65 s after
  the destination ran. Transfer 2m35.533s, downtime 1.42 s.
- **Whole namespace:** 0 `SourceCompleteMissing`, 0 `CancelAckTimeout`. Six
  migrations in total:
  - `t3-cancel`, `v6-a`, `t5-t1` and `t5-t2` `Cancelled`;
  - `v6-b` and `t5-t3` `Completed`.

## B6: sandboxes

**G20: sandbox phases and a lost slot** (`val-reg-sbx`, image
`public.ecr.aws/docker/library/alpine:3.20`):

```text
(a) r5c-ok1: - -> Materializing -> Running -> Completed (exit 0);  r5c-ok2: the same (exit 0);  r5c-exit3: … -> Failed, exitCode 3, WorkloadFailed
    no phase went back
(b) pool r5c-pool warm; r5c-co (poolRef, sleep 600) Running 09:35:27.056
    09:35:27.227 kubectl delete pod r5c-pool-slot-mtsrn --grace-period=0 --force
    09:35:27.722 (+0.3 s) r5c-co Failed  GuestRunning=False/SlotLost "claimed warm slot pod r5c-pool-slot-mtsrn is gone and the workload never reported an exit"
    09:35:31.767 (+4.3 s) new slot r5c-pool-slot-g4s87 Running (warm)
launchers: swiftletd:v0.15.0
```

**G21: a sandbox with no kernel waits** (`val-reg-nok`):

```text
09:34:20 g21-sbx Pending  Resolved=False/KernelNotFound "no SwiftKernel named \"sandbox\" in namespace \"val-reg-nok\""   pods: 0
09:34:38 SwiftKernel created;  09:34:40 kernel Pulling;  09:34:44 kernel Ready   (the sandbox's reason stayed KernelNotFound across the 6 s pull)
09:34:50 launcher pod, Materializing;  09:34:57 Running;  09:35:00 Completed, exit 0
launcher swiftletd:v0.15.0, kernel=/var/lib/kubeswift/kernels/val-reg-nok/sandbox/bzImage, 0 initramfs errors
```

- V9 (b) is skipped: the GPU is held by `ft-gpu-pool`.
- **G22** was not observed: there were 0 `SlotEnded` and 0 `GuestFailed`
  events.

## B7: security

**G23: TokenRequest gate** (`val-reg-tok`, as round 1's D10):

```text
control: kubectl create token default --as=val-tok-user -> token issued
kubectl create token kubeswift-launcher --as=val-tok-user -> forbidden: ValidatingAdmissionPolicy 'kubeswift-launcher-sa-tokenrequest-gate' … denied request:
  tokens for the KubeSwift launcher ServiceAccounts are reserved: …
```

**G24: gateway exec gate**, as `system:serviceaccount:kubeswift-system:kubeswift-gateway`
on `val-reg-ref/r6c`, with `consoleBridge("val-reg-ref","r6c")` taken from
`internal/gateway/exec_bridge.go` at the tag:

```text
sh -c id                 -> The pods "r6c" is invalid: ValidatingAdmissionPolicy 'kubeswift-gateway-exec-gate' … denied request:
                            the kubeswift gateway's credential may exec only the console, sandbox shell and sandbox log bridges, in a launcher container
bridge + "; id"          -> the same denial
bridge, with a TTY       -> admitted; the serial console replayed the boot log (BdsDxe … Linux version 6.8.0-142-generic …);
                            held open until my 12 s timeout interrupted it
```

**G25: secure metrics**, with no Helm change. From a `curlimages/curl` pod to
the controller on `:8080`:

```text
http://<pod>:8080/metrics                              -> 400 (plain HTTP to the TLS server)
https, no token                                        -> 401
https, token of an unbound SA (val-reg-met)            -> 403
https, token of monitoring/monitoring-kube-prometheus-prometheus -> 200, 226 kubeswift_ lines
Prometheus: job=kubeswift-controller-manager-metrics health=up lastError=""; up=1; count({__name__=~"kubeswift_.+"}) = 226
```

**D12's browser half is William's:**
1. **With the capability.** Sign in to the UI as a user whose role has the
   **Console** capability, open a running guest, and open its console. It
   should connect.
2. **Without it.** Do the same as a user without Console. It should fail
   with 403 `cannot create swiftguests/console`.
3. **The role migration.** Run the CHANGELOG's `jq` role migration, and
   report which Access-editor roles it changed.

## At the end

- **kubeswift pods:** the same as in Phase A, with 0 restarts:
  - `controller-manager` (09:11:31), `kubeswift-gateway` (09:11:31);
  - `gpu-discovery`, `kubeswift-dra-driver` (09:11:33);
  - `kubeswift-ui` (2026-09-11).
- **Controller log** (captured from 09:28:28, 1 attach, 758 lines): 317
  ERROR lines, 0 panics.
  - **305 × "…because it is being terminated" (B3)**, by the namespace
    deletion each came from:
    - `val-reg-img` (G6); `val-reg-rt` (G5); `val-reg-ref` (G7);
      `val-reg-pool` (G8);
    - `val-reg-snap` (G12); `val-reg-snapa` (G10a); `val-reg-cs` (G10b);
      `val-reg-clone` (G9);
    - `val-reg-g14live` and `val-reg-g14off` (G14); `val-reg-mig`
      (G17–G19);
    - `val-reg-sbx` (G20); `val-reg-nok` (G21).
  - **8 × optimistic-lock conflicts** ("the object has been modified"), on:
    - the SwiftKernel `sandbox` (G21) and SwiftSandbox `g21-sbx` (G21);
    - the pool `r5c-pool` and sandbox `r5c-co` (G20);
    - the SwiftKernel `faas-minimal` (G1);
    - the SwiftImage `ubuntu-noble` in `val-reg-ref` (G7) and in
      `val-reg-clone` (G9);
    - the SwiftRestore `snapshot-local-clone-a` (G9).
  - **4 × "not found"** during namespace deletion:
    - SwiftRestores pointing at guests already deleted: G10a, and G9 × 2;
    - the SwiftImage `cs-source-snap` (G10b).
  - **Every error maps to a scenario. None is unexplained.**
- **Alerts (Prometheus, dev):**
  - `KubeSwiftMigrationFailures`: **expected**, from G16's two refused
    migrations (B4).
  - `CPUThrottlingHigh` on `gpu-discovery`: pre-existing, see above.
  - `KubeControllerManagerDown`, `KubeSchedulerDown`: k0s control-plane
    scraping, not kubeswift.
- **Clean-up:**
  - **Nothing left.** No `val-reg-*` namespace remains, and none failed to
    delete (6–55 s each). There are 0 VolumeAttachments and 0 Longhorn
    volumes left from test PVCs.
  - **Temporary objects deleted:** `val-migratable-16g`, `val-reg-class`,
    `migration-e2e-live` and `migration-e2e-offline`. The test namespaces'
    SwiftKernels went with their namespaces.
  - **Untouched:**
    - `innercp` (launcher uid `10bd28b8-…`, 0 restarts);
    - `field-testing/ft-gpu-pool-slot-j74r2` (uid `6aeee96c-…`, 0 restarts,
      GPU still allocated to it).
  - **No node cordoned**, and cp-1's Longhorn disk is at 63.2% free.
