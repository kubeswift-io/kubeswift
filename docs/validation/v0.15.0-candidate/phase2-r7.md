# Phase 2 r7: dev on `8afbad3`

Run 2026-09-28, 08:45–09:06 UTC, after Phase 1 r7 passed on dev (`phase1-r7.md`).
Node names are generalised: `cp-1`, `worker-1`, `worker-2`. Pod IPs are redacted.

| # | Verdict |
|---|---|
| R7-D | **PASS**, on a new guest. The first attempt on the kept `val-r6-dnr/r6d` was refused with `ImageTagMismatch` (see below). With William's OK it ran on a new guest `val-r7-dnr/r7d` created after the upgrade. Its `DstNeverReady` message names `PVC "swiftguest-root-r7d" not attached to node <cp-1> (VolumeAttachment csi-…)`. The target VolumeAttachment was gone 171 s after the failure |
| V4a live | **PASS**: "All checks passed", the Longhorn healthy wait logged, `observedTransferDuration` 19.66 s |

**No guest was lost.** Both scenarios passed, so `val-r6-dnr`, `val-r7-mig` and
`val-r7-dnr` were deleted. All three were gone within 21 s, leaving no
VolumeAttachment. The SwiftGuestClass `migration-e2e-live` (left by the script's
`--no-cleanup`) was deleted too.

## R7-D: `DstNeverReady` names the volume attach (#691). PASS

**William approved** deleting a Longhorn replica for R7-D. He then chose the
new-guest retry described below.

### First attempt, on the kept guest: refused by the image-tag check

`val-r6-dnr/r6d` still ran its round-6 launcher, `swiftletd:sha-4f08e87`.

```text
08:45:20 delete replica …-r-7ddf0cde (<worker-2>);  08:45:42 engine shows 139b5a8f=WO
08:45:42.184 swiftctl -n val-r6-dnr migrate r6d --to <worker-1> --preferred-mode live --allow-ip-change --name r7d-mig
08:45:42.365 Failed  failureReason=ImageTagMismatch
  "image tag mismatch: source pod uses \"ghcr.io/kubeswift-io/kubeswift/swiftletd:sha-4f08e87\",
   controller default is \"ghcr.io/kubeswift-io/kubeswift/swiftletd:sha-8afbad3\" (LBA-1 trip-wire)"
```

- **By design.** The controller refuses a live migration whose source
  launcher's image differs from its own default. That is why the plan's
  general rule is that every guest must be created after the upgrade.
  "Repeat on the kept guest" conflicts with that rule.
- **Nothing was touched.** No destination pod was created. The guest kept
  running: same launcher uid `26973c1b-…`, uptime 6070 → 6230 s, and the fill
  and its checksum unchanged. The deleted replica rebuilt, and the volume was
  healthy at 08:47:22.
- **Minor.** The refused migration recorded only its `Ready=False/MigrationFailed`
  condition, and **no event**.

### Second attempt: a new guest on `sha-8afbad3` (William's choice)

**Setup:**
- guest `val-r7-dnr/r7d`, class `small-migratable`, launcher
  `sha-8afbad3`, on worker-1;
- 6 GiB of random data in `/var/tmp/fill`, volume actual size 9.1 GiB,
  `healthy`;
- the deleted replica on worker-2; the target cp-1.

```text
08:54:45.204 kubectl -n longhorn-system delete replicas.longhorn.io …-r-774ce26b (<worker-2>)
08:55:08.541 engine: 993b007c=WO (rebuilding), 2 × RW
08:55:08.652 swiftctl -n val-r7-dnr migrate r7d --to <cp-1> --preferred-mode live --allow-ip-change --name r7d-mig
08:55:08 Validated; DestinationPodCreated r7d-mig-fc6b98 on <cp-1>; VolumeAttachment <cp-1> attached=false
08:56:08 Failed  DstNeverReady (60 s);  events: Warning DestinationPodNeverReady, Normal DestinationPodCleanedUp
```

**The full `failureMessage`** (the `DestinationPodNeverReady` event carries
the same text):

```text
destination pod "r7d-mig-fc6b98" never reached Ready within 1m0s budget: PVC "swiftguest-root-r7d" not attached to node <cp-1> (VolumeAttachment csi-c2732e01b23c892517c4a575136bd32afae64d935337b413f055c8e67fd9d491); init container "network-init" waiting: PodInitializing
```

**Every pass condition holds:**
- `DstNeverReady` fired at 60 s.
- Right after `within 1m0s budget:`, the message reads `PVC "<root pvc>"
  not attached to node <target> (VolumeAttachment csi-…)`, then the init
  container part. No `attachError` had been set yet, so none is quoted.
- The `DestinationPodNeverReady` event has the same text.
- The guest is untouched on worker-1:
  - the same launcher uid `a4691b90-…`, 0 restarts;
  - boot time 08:51:53 in both readings (uptime 139 s before, 440 s at
    08:59:13), so it never rebooted;
  - the fill is still 6442450944 bytes, with the same checksum of its first
    100 MiB (`a1b9e150b3178d17`).
- No destination pod is left.

**The target VolumeAttachment (#692):**

```text
08:56:45 rebuild done (3 × RW), 37 s after the failure
08:56:53 target attach completes anyway: VolumeAttachment <cp-1> attached=true; Longhorn migration engine on <cp-1>; migrationNodeID=<cp-1>
08:58:59 the <cp-1> VolumeAttachment is gone; migrationNodeID cleared; only <worker-1> remains
```

**It took 171 s from the failure**, about 2 min after the attach completed.
In round 6 it was about 4 m 20 s. Either way it clears on its own, and #692
tracks it.

## V4a live (regression). PASS

`NAMESPACE=val-r7-mig migration-test.sh --mode live --source <worker-1>
--target <worker-2> --storage-class longhorn-migratable --no-cleanup`, from
the `8afbad3` checkout (08:59:32–09:04:51). It ran after R7-D's target
VolumeAttachment had gone.

```text
Launcher scheduled on <worker-1>; the other nodes are uncordoned
Guest uptime before: 21s
Waiting for Longhorn volume pvc-8e43ad81-… (PVC swiftguest-root-e2e-guest) to be healthy (max 15min)...
Longhorn volume pvc-8e43ad81-… is healthy
Migration completed in 53s (resolved mode: live)
Post-migration pod e2e-guest-mig-2c86bc on <worker-2>   (swiftletd:sha-8afbad3)
PASS: disk sentinel survived the migration
PASS: tmpfs sentinel survived and uptime kept counting (21s -> 175s): the running VM moved
PASS: webhook rejected migration of guest with migration.enabled=false
All checks passed.
```

- **The volume wait.** The volume was created at 09:01:02 and the guest was
  running at 09:01:48. The migration started at 09:03:52. The script does not
  timestamp its wait line; from the uptime reading, the wait was about
  1 m 40 s.
- **Migration:** `observedTransferDuration=19.66s`, `observedDowntime=2.28s`,
  0 `SourceCompleteMissing`, and no node left cordoned.
- **V5-style phaseDetail watch (0.2 s):**

```text
09:04:19.975 transferring guest state   26 → 52 → 79
09:04:39.200 destination running; waiting for the source's report   DestinationRunning
09:04:39.558 cutover: completing
09:04:41.322 Completed "destination guest healthy (IP 192.168.99.10)"
```

## Findings

1. **"Repeat on the kept guest" does not survive an upgrade.** A launcher
   from the previous candidate cannot be live-migrated by the new controller
   (`ImageTagMismatch`, by design). Future re-runs should always use a guest
   created after the upgrade, as the plan's general rule already says.
2. **A migration refused at validation records no event** (minor), only its
   `Ready=False/MigrationFailed` condition. Every other failure path seen in
   this validation also emits a Warning event.

## State left on dev

- **Controller:** chart `0.0.0-dev.8afbad3` (rev 50), `--metrics-secure=true`.
- **No `val-*` namespaces.** The only SwiftGuest is `gpu-cells/innercp`
  (launcher uid `10bd28b8-…`, 0 restarts).
- **`field-testing/ft-gpu-pool-slot-j74r2`:** uid `6aeee96c-…`, 0 restarts,
  still holding the GPU.
- **cp-1:** 63.6% free, `Schedulable=True`. **Nodes:** none cordoned.
- **VolumeAttachments:** none left for any of the deleted test volumes.
