# Phase 2 r2: dev suites on `7a1a76d`

Run 2026-09-25, 07:23–08:55 UTC, after Phase 1 r2 passed on dev (`phase1-r2.md`).
Every guest below was created after the upgrade, and every launcher checked ran
`swiftletd:sha-7a1a76d`. Node names are generalised: `cp-1`, `worker-1`,
`worker-2`. Pod IPs are redacted.

**Namespaces.** Main is `val-r2`: R4–R6's guest `r4g`, R8's image, R9's probe.
`migration-test.sh` deletes the namespace it runs in, so R3 ran in
`val-r2-d7a` / `val-r2-d7b`. R2 ran in `val-r2-rt`. R1 ran in `default`,
because the smoke samples hard-code it.

## Results

| # | Verdict |
|---|---|
| R1 | **PASS** |
| R2 | **PASS** |
| R3 | **PASS** (D7a and D7b) |
| R4 | **PARTIAL.** The cancel takes the graceful path, and the source hangup is gone. But the source's `migration-status` turns `failed` only after **9 m 46 s**, at swiftletd's 600 s deadline, not "within about a minute" |
| R5 | **Completed** (the round-1 loss sequence now completes safely). The sentinel and uptime survive, and `w23 … completed=true` fires before `received`. **But a `SourceCompleteMissing` event appears**: the controller cut over on the destination's `running` 3.6 s before the source reported `complete` |
| R6 | **PASS** on its criteria in all three attempts: exactly one VM each time, no Failed migration, the survivor intact. **But two new defects make R6 slow and unsafe to automate:** a cancelled send blocks the source for 600 s, and a queued send from a cancelled migration is replayed later against the dead destination IP (see R6) |
| R7 | **PASS**, with a note: `VmStopped` was observed, then replaced by `LauncherExited` |
| R8 | **PASS**, with a note: the Job retries inside one pod, so the `failed ≥ 1` window does not occur |
| R9 | **PASS**: plain HTTP refused, unbound token 403, bound token 200, the Prometheus target up. dev is left secure |

---

## R1: disk-boot smoke. PASS

`test/smoke/boot-test.sh --scenario disk-boot --no-cleanup`, 07:23–07:25:

```text
hypervisor=cloud-hypervisor (expected)
primaryIP=192.168.99.19
disk-boot: PASS  /  === All scenarios PASSED ===
launcher image=ghcr.io/kubeswift-io/kubeswift/swiftletd:sha-7a1a76d   runtime.hypervisor=cloud-hypervisor GuestRunning=True
```

- In round 1 `hypervisor=` read empty at this point. It is populated now.
- The script still warns `GuestRunning=` (read the instant phase turns Running),
  and the condition is `True` right after.
- Cleaned up by hand, keeping the lab's `default/ubuntu-noble`.

## R2: in-place restore (#657). PASS

`local-roundtrip-test.sh --namespace val-r2-rt --no-cleanup`:

```text
Captured on node <worker-1> (pause window 1674ms)
OK: launcher pod is in restore-receive mode, no stager (in-place fast path)   launcher swiftletd:sha-7a1a76d
OK: sentinel survived: kubeswift-roundtrip-1790321359-24465
=== Tier B round-trip e2e PASS ===
```

| | Value |
|---|---|
| `primaryIP` before, snapshot `guestSpec.primaryIP`, after | `192.168.99.10` / `192.168.99.10` / `192.168.99.10` (reported at 07:29:29, before Ready) |
| SwiftRestore | 07:29:24 → 07:29:33 = **9 s** (Restoring → Resuming → Ready) |
| Handle | `/var/lib/kubeswift/snapshots/val-r2-rt_snapshot-local-mem` |

**Cleanup, and a confirmation of the round-1 finalizer defect.** Deleting the
SwiftSnapshot **before** the namespace worked: the snapshot was gone in 5 s,
and the namespace 13 s later. The deadlock happens only when a namespace is
deleted with a local snapshot still in it.

## R3: migration script. PASS

**D7a** (`--mode offline`, worker-1 → worker-2, ns `val-r2-d7a`): "All checks
passed", migration in **121 s**.

```text
Phases: 07:32:24 Preparing "waiting for source pod termination" → "waiting for volume detach" →
        07:32:39 Resuming "awaiting destination launcher start" → 07:34:03 "awaiting GuestRunning=True on destination" (new) →
        "awaiting primaryIP discovery" → 07:34:24 Completed (IP 192.168.99.16)
```

No node was cordoned before or after.

**D7b** (`--mode live --guest-class small-migratable --no-cleanup`, ns
`val-r2-d7b`): "All checks passed", migration in **43 s**.

```text
PASS: disk sentinel survived; PASS: tmpfs sentinel survived and uptime kept counting (21s -> 69s)
PASS: webhook rejected migration of guest with migration.enabled=false
Phases: Preparing → StopAndCopy "issuing send on source" → "transferring guest state" 26/52/79% → "cutover: completing" → 07:37:26 Completed
SwiftMigration: mode=live, observedDowntime=1.999664112s, observedTransferDuration=<empty>   (round 1 recorded 19.821s)
pods "e2e-guest" not found; status.podRef=e2e-guest-mig-448470 (Running on worker-2, swiftletd:sha-7a1a76d)
```

**Small regression to note:** `observedTransferDuration` is no longer filled
for a live migration. It was empty on R5 as well.

## R4: cancel mid-transfer. PARTIAL

**Setup:**
- A new guest `val-r2/r4g` (class `val-migratable-16g`), created after the
  upgrade, on cp-1, launcher `sha-7a1a76d`, uid `4d89e9a7-…`.
- A 10 GiB tmpfs of `/dev/urandom` data, rewritten in a loop.
- The sentinel `VAL-R4-1790321476`, uptime 546 s.
- A live migration to worker-1, with `cancelRequested` set 15 s into
  "transferring guest state".

```text
07:38:44.2 created r4-cancel -> <worker-1>
07:38:58.3 StopAndCopy "transferring guest state"
07:39:13.5 >>> cancelRequested=true (progress 6)
07:39:13.9 StopAndCopy "waiting for cancel acknowledgment"        <- new: the graceful path
07:39:16.9 Cancelled "destination pod deleted after swiftletd cancel ack"
events: CancelIssued 07:39:13 → Cancelled 07:39:16 "destination pod deleted after swiftletd cancel ack"; no CancelAckTimeout
```

| Criterion | Result |
|---|---|
| Ends `Cancelled` | ✔ |
| `CancelIssued`, then "destination pod deleted after swiftletd cancel ack"; no `CancelAckTimeout` | ✔ (3.4 s from cancel to Cancelled) |
| Destination log: the cancel dispatched before any SIGTERM | ✔ `07:39:13.728 action_accept … MigrationCancel id=r4-cancel:cancel:0` → `07:39:13.750 dispatch_migration_cancel` → `07:39:16.777 dispatch_migration_cancel_killed pid=55`. No `sigterm_received` |
| Source: sentinel, uptime, SSH | ✔ `VAL-R4-1790321476`, uptime 546 → 1160 s (continuous), SSH works |
| No ESTABLISHED to the old destination's :6789 | ✔ **fixed**: the source CH got a RST at 07:39:17 (`Connection reset by peer (os error 104)`), and at 07:39:18 `ss` shows no :6789 socket in any state. Round 1: 16 min ESTABLISHED |
| Source `migration-status` turns `failed` within ~1 min | ✘ **9 m 46 s**: `sending` until `07:48:59 migration_send_failed id=r4-cancel:send:1 … "source guest still Running past the migration deadline (v53 auto-resumes the source on a failed transfer)"`, then `w23_terminal_write_signal_fired … completed=false` |

**Why it is slow.** Cloud Hypervisor's send errored at 07:39:17, but swiftletd
does not act on that error. It learns the send failed only from its 600 s
deadline, when the VM is "still Running past the deadline". This matters beyond
the status (see R6): the source's action slot stays busy that whole time.

## R5: the round-1 loss sequence. Completed, with one flagged event

Right after R4, and after its source deadline, a plain live migration of the
same `r4g` (the same source pod `4d89e9a7-…`, which had just had the cancelled
send) from cp-1 to worker-1:
- The memory rewriter was stopped first, so the transfer could converge.
- "Back to its first node" doesn't fit, because R4's cancel left the guest on
  its first node. I migrated it to R4's target.

```text
07:49:54 created r5-plain; 07:50:07 StopAndCopy "transferring guest state" progress=95   <- stale (R4's estimate), then real 3%/5 s
07:52:40 event SourceCompleteMissing: destination pod "r4g-mig-99c62d" runs the guest but the source never reported complete (id=r5-plain:send:1); cutting over
07:52:40 CutoverStep1 (podRef = r4g-mig-99c62d)
07:52:41 "cutover: completing"; 07:52:44 Completed, downtime 3.41 s
source log: 07:52:43.424 vm_exited_post_send_migration
            07:52:43.624 dispatch_migration_send_complete id=r5-plain:send:1 elapsed_ms=155735
            07:52:43.639 w23_terminal_write_signal_fired id=r5-plain:send:1 completed=true
            07:52:43.639 w23_terminal_write_signal_received; safe to exit
guest after: Running on r4g-mig-99c62d (worker-1), sentinel VAL-R4-1790321476, uptime 1207 → 1842 s (continuous)
```

- ✔ **Completed.** This is the round-1 sequence that lost the guest.
- ✔ The sentinel and uptime survived.
- ✔ `completed=true` fired for this send before `received`.
- ✘ **`SourceCompleteMissing` appeared.** Its text says the source "never
  reported complete", but the source reported it 3.6 s after the controller
  cut over on the destination's `running`.
  - The cutover itself was correct and safe. The event and its wording are
    premature in the ordinary case, not rare.
- ? **The source pod's final `migration-status: complete` could not be read:**
  the pod was deleted at cutover. Its log shows the `complete` write fired.

## R7: a migrated guest reports its stop. PASS, with a note

On D7b's migrated guest (`val-r2-d7b/e2e-guest`, pod `e2e-guest-mig-448470`):

```text
env: KUBESWIFT_GUEST_NAME=e2e-guest
launcher log: 07:37:24 w16_guest_running_reported guest=val-r2-d7b/e2e-guest     (the guest, not the pod)
07:37:59 ssh … sudo systemctl poweroff
07:38:04 GuestRunning=False reason=VmStopped   (pod still Running)
07:38:22 phase=Stopped, GuestRunning=False reason=LauncherExited "the launcher exited; the VM is not running"; pod Succeeded
report_failed … not found: 0 lines
```

`VmStopped` is reported, then overwritten by the controller's `LauncherExited`
once the pod reaches Succeeded. A reader after the fact sees `LauncherExited`.

## R8: an import failure only when the Job gives up. PASS, with a note

SwiftImage `val-r2/r8-bad` with `source.http.url: https://kubeswift.invalid/none.img`, created at 07:23:47:

```text
07:23:47 image=Importing            job active=1 failed=<none> backoffLimit=6
07:29:50 Job FailureTarget=BackoffLimitExceeded; 07:29:51 Job Failed=BackoffLimitExceeded (failed=1)
07:29:51 image=Failed  ImportFailed "Job has reached the specified backoff limit"
```

- ✔ The image stayed `Importing` for the whole 6 min of retries, and turned
  `Failed` in the same second as the Job, with that message. Round 1 went
  `Failed` on the first failed pod.
- The Job's pods use `restartPolicy: OnFailure`, so every retry was a
  **container restart inside one pod**. The Job's `status.failed` stays empty
  until it gives up, then reads 1.
- The window the plan describes (`failed ≥ 1`, no `Failed` condition) therefore
  cannot occur with this Job shape.
- The time from the first container failure to the image's `Failed` could not
  be recovered, because the Job deleted its pod. From Job start to `Failed` was
  **6 m 04 s**.

## R9: D13, secure metrics. PASS, dev left secure

```text
helm upgrade kubeswift … --version 0.0.0-dev.7a1a76d --reuse-values \
  --set controllerManager.metrics.secure=true \
  --set controllerManager.metrics.readers[0].name=monitoring-kube-prometheus-prometheus \
  --set controllerManager.metrics.readers[0].namespace=monitoring        (revision 45, 08:05:04)
controller args: --leader-elect --webhook-enabled=true --metrics-secure=true; log "Serving metrics server" bindAddress=":8080" secure=true
ClusterRoleBinding kubeswift-metrics-reader -> ServiceAccount monitoring/monitoring-kube-prometheus-prometheus
ServiceMonitor endpoint: scheme https, tlsConfig.insecureSkipVerify, bearerTokenFile (Prometheus's own token)
```

From a `curlimages/curl` pod in `val-r2`, at 08:05:47:

```text
http://<pod>:8080/metrics                                   -> 400 (plain HTTP to the TLS server)
https, no token                                             -> 401
https, token of val-r2/val-r9-unbound (not a reader)        -> 403 Forbidden
https, token of monitoring/monitoring-kube-prometheus-prometheus -> 200, 130 kubeswift_ series
Prometheus /api/v1/targets: job=kubeswift-controller-manager-metrics url=https://<pod>:8080/metrics health=up lastError=""
up{job="kubeswift-controller-manager-metrics"} = 1;  count({__name__=~"kubeswift_.+"}) = 130
```

Final dev values: `controllerManager: {image: {tag: sha-7a1a76d}, metrics:
{readers: [{name: monitoring-kube-prometheus-prometheus, namespace: monitoring}],
secure: true}}`. The rest is as in `phase0.md`, with every tag `sha-7a1a76d`.
The probe pod and SA were deleted.

## R6: cancel racing completion. PASS on its criteria, with two new defects

**Setup.**
- Guest `r4g` (created after the upgrade) with a static 10 GiB of random tmpfs
  data and no rewriter; a transfer takes ~155 s.
- Every attempt ran from pod `r4g-mig-99c62d` (worker-1) to cp-1, and that pod
  had already had a cancelled send before each attempt:
  - a priming cancel `r6-prime` (graceful; 08:01:17 → Cancelled 08:01:20);
  - then each earlier attempt's own cancel.

**Trigger change.** "src migration complete" no longer appears as a
phaseDetail in this build; the phase goes from "transferring guest state"
straight to "cutover: completing". So the late cancels are timed from the start
of "transferring".

| Attempt | Cancel sent | Outcome | Survivor | Events |
|---|---|---|---|---|
| **t1** `r6-t1` (progress ≥ 95) | 08:30:25.7, **0.5 s into the transfer**. The first progress read was a **stale 95**, left on the source pod by its previous send's estimate | `Cancelled` 08:30:30 ("destination pod deleted after swiftletd cancel ack") | source `r4g-mig-99c62d`: sentinel ✔, uptime 3633 → 3664 s ✔ | CancelIssued → Cancelled; no CancelAckTimeout |
| **t2** `r6-t2` (+145 s) | 08:43:32.2, 145 s in, real progress 92 %, ~10 s before completion | `Cancelled` 08:43:36. Destination: `dispatch_migration_cancel` 08:43:32.934 → `cancel_killed` 08:43:35.997 → `sigterm_received` 08:43:36.721 (after) | source: sentinel ✔, uptime 4274 → 4451 s ✔ | CancelIssued → Cancelled; no CancelAckTimeout |
| **t3** `r6-t3` (+153 s) | 08:54:03.63, 153 s in, **raced completion** | `Completed` 08:54:08 | **destination** `r4g-mig-09d09b` (cp-1): sentinel ✔, uptime 4898 → 5087 s ✔; the source pod is gone | CancelIssued 08:54:03 → **CancelIgnored** "post-cutover; migration cannot be reversed" → CutoverStep1 → **SourceCompleteMissing** → Completed |

**t3 in detail. It shows the safety net working.**

```text
dst 08:54:04.421 dispatch_migration_receive_complete id=r6-t3:recv:1 state=Running elapsed_ms=154249
dst 08:54:04.478 w16_guest_running_reported guest=val-r2/r4g
dst 08:54:04.493 action_accept … MigrationCancel id=r6-t3:cancel:0
dst 08:54:04.512 dispatch_migration_cancel_refused id=r6-t3:cancel:0 reason=destination_running
                 "cancel refused: the destination guest is already running (the migration completed)"
src 08:54:07.015 dispatch_migration_send_complete id=r6-t3:send:1 elapsed_ms=155171
src 08:54:07.047 w23_terminal_write_signal_fired id=r6-t3:send:1 completed=true   (before `received`)
```

**Across R4–R6** (7 SwiftMigrations in `val-r2`):
- **0 `Failed`.**
- **0 `CancelAckTimeout`.**
- **`SourceCompleteMissing` on both completions (R5, t3).** In each case the
  source reported `complete` about 3 s *after* the controller cut over on the
  destination's `running`.
- **Exactly one VM survived** every attempt.

### New defect A: a cancelled send blocks the source for 600 s

After any cancel, the source Cloud Hypervisor's send fails within seconds (RST
from the stopped destination). But swiftletd notices only at its 600 s action
deadline: `migration_send_failed … "source guest still Running past the
migration deadline"`. Until then its main action slot is busy, and the source
pod accepts no new migration. Measured:

| Cancelled send | CH send error | swiftletd `failed` |
|---|---|---|
| `r4-cancel` | 07:39:17 (RST) | 07:48:59 |
| `r6-prime` | 08:01:21 (RST) | 08:11:02 |
| `r6-t1` | 08:30:30 (RST) | 08:40:36 |
| `r6-t2` | 08:43:38 (RST) | ~08:51:07 (idle at 08:51:14) |

**A new migration started in that window does not run, and looks as if it
does.**
- `r6-a1` (08:01:34), launched right after `r6-prime`'s cancel, got its
  send-action annotation on the source. The source never accepted it: its log
  shows no `action_accept` for `r6-a1` until 08:11:02.
- The SwiftMigration still showed "transferring guest state" with progress
  climbing to 95. That is the stale estimate, not data.
- Cancelling it then "worked" (08:03:31), though nothing had been sent.

### New defect B: the send-action of a cancelled migration is replayed later

When `r6-prime`'s deadline freed the slot at 08:11:02, the source picked up the
**still-pending send-action of `r6-a1`**. That migration had been cancelled
7.5 min earlier, and its destination deleted:

```text
08:11:02.953 w23_terminal_write_signal_fired id=r6-prime:send:1 completed=false
08:11:02.957 action_accept namespace=migration kind=MigrationSend id=r6-a1:send:1 slot=Main
08:11:02.977 dispatch_migration_send id=r6-a1:send:1 target=tcp:<r6-a1's deleted destination pod IP>:6789
             ss: SYN-SENT … -> <that IP>:6789        (no pod held the IP)
08:13:17     cloud-hypervisor: Migration failed: … Error connecting to TCP socket: Operation timed out (os error 110)
08:21:02     migration_send_failed id=r6-a1:send:1 … past the migration deadline   (the source blocked for another 10 min)
```

- The cancel path clears the destination but leaves the source's pending
  `migration-action` annotation in place.
- Here the connect simply timed out. But if that pod IP had been reused by
  another pod listening on 6789 (another migration's destination, say), a
  cancelled migration would have streamed this VM's memory into it.
- **Stale progress** is the same leftover-state class. A new SwiftMigration's
  `transferProgress` starts at the previous send's final
  `migration-progress-estimate` (95), which made t1's "progress ≥ 95" trigger
  fire at 0.5 s.

These two defects cost R6 about 50 minutes of waiting: each attempt had to wait
out the previous cancel's 600 s block.

## State left on dev

| What | Where | Note |
|---|---|---|
| Guest `r4g` Running on cp-1 (`r4g-mig-09d09b`), SwiftImage `r8-bad` (Failed), 7 SwiftMigrations | `val-r2` | kept for inspection; the events expire after ~1 h |
| Round-1 evidence | `val-d8` (`mig16` Stopped / Succeeded pod, `mig16b` Stopped), class `val-migratable-16g` | unchanged apart from stopping `mig16b` |
| Stuck `Terminating` | `val-d2`, `val-d3` | the pre-existing finalizer defect, untouched |
| Controller | chart `0.0.0-dev.7a1a76d`, rev 45, `--metrics-secure=true`, Prometheus bound as reader | per R9 |
| Unchanged | `gpu-cells/innercp` (uid `10bd28b8-…`, 0 restarts) | |

- `val-r2-rt`, `val-r2-d7a` and `val-r2-d7b` are deleted.
- No node is cordoned.
- `default` holds only the lab content.
