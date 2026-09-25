# Phase 2 r3: dev suites on `f260277`

Run 2026-09-25, 11:10–12:10 UTC, after Phase 1 r3 passed on dev (`phase1-r3.md`).

- Every guest was created after the upgrade, and every launcher checked ran
  `swiftletd:sha-f260277`.
- Node names are generalised: `cp-1`, `worker-1`, `worker-2`. Pod IPs are redacted.
- **Namespaces:** main `val-r3`. T2 used `val-r3-d7a*` / `val-r3-d7b*`, because
  the script deletes its own namespace. T1 used `default`.

**No guest was lost.** Every scenario passed, so `val-r3` and the D7b namespace
were deleted at 12:10:16.

## Results

| # | Verdict |
|---|---|
| T1 | **PASS** |
| T2 | **PASS on the re-run.** The first run hit a **script defect**: the guest landed on the third node (see T2) |
| T3 | **PASS**: the source goes `failed` 25.1 s after the cancel (round 2: 9 m 46 s) |
| T4 | **PASS on the second run.** The first run tripped on target-node CPU headroom (see T4, a finding) |
| T5 | **PASS**: all three attempts; source free ~27 s after each cancel; the `DestinationRunning` cancel gives `CancelIgnored` and completes |

## T2: the migration script

### First run (11:15–11:33): a script defect, not a product failure

```text
D7a (--mode offline --source <worker-1> --target <worker-2>): Guest running at IP=… on node=<cp-1> (pod e2e-guest)
                                                               guest landed on <cp-1>, expected <worker-1>   EXIT=1
D7b (--mode live …):                                           the same: guest landed on <cp-1>, expected <worker-1>   EXIT=1
```

- With `--source` and `--target` given, `migration-test.sh` cordons only the
  target and relies on the scheduler to pick the source.
- On a 3-node cluster, the third node can win the scheduling. Here worker-1
  carried `t3g`'s 16 Gi, and the scheduler chose cp-1 twice. Rounds 1 and 2
  happened to land on worker-1.
- No migration ran, and no cordon was left.
- **Suggested fix:** pin the guest with `spec.nodeName`, or cordon every node
  except the source.

### Re-run (11:51–12:09). PASS

- **Method:** I cordoned cp-1 myself for the duration (11:51:47 → 12:08:56),
  so the script's guest could only land on the source. The nodes before and
  after the run are all schedulable.

**D7a** (offline, worker-1 → worker-2, ns `val-r3-d7a2`): **All checks passed**, in 112 s.

```text
11:53:38 Preparing "waiting for source pod termination" → "waiting for volume detach" → 11:53:50 StopAndCopy "awaiting destination pod creation"
→ Resuming "awaiting destination launcher start" → 11:55:03 "awaiting GuestRunning=True on destination" → "awaiting primaryIP discovery" → 11:55:24 Completed
events: GuestClaimed, PodTerminating, Validated, PVCDetaching, VolumeDetached, PodScheduling, PodScheduled, Completed
observedDowntime=1m47.045913152s   observedTransferDuration=<empty>   (offline: no memory transfer)
```

**D7b** (live, `--guest-class small-migratable --no-cleanup`, ns `val-r3-d7b2`): **All checks passed**, in 42 s.

```text
PASS: disk sentinel survived; PASS: tmpfs sentinel survived and uptime kept counting (22s -> 70s); PASS: webhook refused the disabled guest
12:08:05 Preparing → 12:08:22 StopAndCopy "issuing receive on destination" → "issuing send on source" → 12:08:23.7 "transferring guest state"
progress: null → 26 → 52 → 79
12:08:42 condition DestinationRunning (observer saw it 12:08:43.254) → 12:08:43 CutoverStep1 → 12:08:43.6 "cutover: completing" → 12:08:44.6 Completed
observedTransferDuration = 19.83s        (round 2: <empty>; round 1: 19.821s)
observedDowntime         = 1.524568232s
events: DestinationPodCreated, Validated, DestinationPodReady, ReceiveIssued, SendIssued, CutoverStep1, Completed.  No SourceCompleteMissing.
after: pods "e2e-guest" not found; status.podRef = e2e-guest-mig-b6d910 (Running on worker-2, swiftletd:sha-f260277)
```

- **`DestinationRunning` → source `complete`:** the cutover followed
  `DestinationRunning` by ≤ ~1.6 s, with no `SourceCompleteMissing`.
- I could not read the source's exact `complete` timestamp. My log follower
  timed out before the source pod started (D7b's setup took ~12 min), and the
  pod was deleted at cutover.
- T4 and T5 give the exact gap: 2.3 s and 2.5 s.
- "src migration complete; preparing cutover" did not appear. As the go-ahead
  says, that predates these fixes.

## T1: disk-boot smoke. PASS

`test/smoke/boot-test.sh --scenario disk-boot --no-cleanup`, 11:10–11:13:

```text
primaryIP=192.168.99.11   disk-boot: PASS   === All scenarios PASSED ===
launcher image=ghcr.io/kubeswift-io/kubeswift/swiftletd:sha-f260277   runtime.hypervisor=cloud-hypervisor GuestRunning=True
```

- The script's `WARN: GuestRunning=` / `hypervisor=` came back this time. They
  are read the instant phase turns Running; both were set moments later.
- Cleaned up by hand, keeping `default/ubuntu-noble`.

## T3: cancel mid-transfer. PASS

- **Guest:** `val-r3/t3g`, created after the upgrade (class
  `val-migratable-16g`), on worker-1, pod uid `2d649af1-…`, `swiftletd:sha-f260277`.
- **Load:** 10 GiB of random tmpfs data, rewritten in a loop.
- **Sentinel:** `VAL-T3-1790334959`, uptime 1077 s.
- **Action:** a live migration to worker-2, cancelled 15 s into "transferring guest state".

```text
11:33:16.103 created t3-cancel -> <worker-2>
11:33:55.981 StopAndCopy "issuing receive on destination" → 11:33:56.305 "issuing send on source"
11:33:57.709 "transferring guest state"          (source log: 11:33:57.524 action_accept … id=t3-cancel:send:1)
11:34:12.904 >>> cancelRequested=true (progress 9)
11:34:13.227 "waiting for cancel acknowledgment"
11:34:17.291 Cancelled "destination pod deleted after swiftletd cancel ack"
events: … SendIssued 11:33:56 → CancelIssued 11:34:12 → Cancelled 11:34:17; no CancelAckTimeout
progress over time: null → 3 → 6 → 9 (progress-estimate-id=t3-cancel:send:1 throughout)
```

| Criterion | Result |
|---|---|
| `Cancelled` on the graceful path; no `CancelAckTimeout` | ✔ (4.4 s from cancel to Cancelled) |
| The source has no `migration-action*` naming this migration right after `Cancelled` | ✔ the watcher shows `action-id` gone at **11:34:13.356**, 0.45 s after the cancel. The terminal dump at 11:34:17.3 lists no `migration-action`, `-action-id` or `-action-args` |
| The source's `migration-status` turns `failed` within 60 s of the cancel | ✔ **11:34:37.980, 25.1 s** after the cancel |
| `migration_send_failed id=t3-cancel:send:1` names the closed connection | ✔ `detail=the migration connection to <dst>:6789 closed, and the source guest is still running (the transfer failed; CH v53 auto-resumed the source)` |
| Sentinel, uptime, SSH | ✔ `VAL-T3-1790334959`, uptime 1077 → 1210 s over 134 s, the same pod uid, 0 restarts |
| **Timings** | cancel **11:34:12.904** → CH `Connection reset by peer (os error 104)` **11:34:17.432** → `migration_send_failed` **11:34:37.518** (20.1 s after the reset) |

## T4: a migration right after a cancel. PASS (second run)

### First run: invalid, because the target node lacked room for two destination pods

`t4-a` was created at 11:34:18.0, **0.7 s after T3's Cancelled**, to the same
worker-2:

```text
t4-a  Failed  "target node \"boba\" has insufficient CPU headroom: need 2, have 290m (allocatable 8, used 7710m)"
t4-b  created 11:34:18.9 → StopAndCopy "waiting for the source launcher to finish a previous send" (11:34:28.5) → cancelled while waiting → Cancelled 11:34:33
t4-c  created 11:34:34.3 → Failed, the same CPU-headroom message
```

- **Why:** a live migration's validation counts the previous destination pod
  (2 CPU) while it is still terminating. Worker-2 can hold only one.
  - A migration started within seconds of a cancelled one, to the same node,
    fails validation with `Failed` instead of waiting.
  - This is **a finding worth a look**: on a busy node, a quick retry fails
    outright.
- **Useful part:** `t4-b` showed the new "waiting for the source launcher…"
  state while the source was still busy with T3's send. The source never wrote
  or accepted a `t4-b` send (0 log lines).
- **Change for the second run:** I removed the failed T2 leftover guest from
  cp-1, and the second run targeted cp-1, which has room for two destination
  pods.

### Second run (`t4-a2`, `t4-b2`, `t4-c2` → cp-1)

- The rewriter ran for `t4-a2`, and was stopped in the background as `t4-a2`
  ended `Cancelled`; `rewriters_after_stop=0` was confirmed while `t4-b2` ran.

```text
t4-a2 11:36:32 created; 11:36:48.206 "transferring guest state"   (source 11:36:48.006 action_accept id=t4-a2:send:1)
      11:37:03.534 cancel (progress 9) → "waiting for cancel acknowledgment" → 11:37:07.358 Cancelled (graceful)
t4-b2 11:37:08.009 created (0.65 s after t4-a2's Cancelled)
      11:37:11.513 "waiting for the source launcher to finish a previous send" → 11:37:11.715 cancel → 11:37:16.635 Cancelled (graceful)
t4-c2 11:37:17.346 created; 11:37:20.932 "waiting for the source launcher to finish a previous send"
      source: 11:37:27.846 migration_send_failed id=t4-a2:send:1 ("migration connection … closed") → 11:37:27.868 action_accept id=t4-c2:send:1
      11:37:28.168 "transferring guest state"; progress 3 (11:37:33) → 6 → 9 … → 92 → 95
      11:40:00.925 condition DestinationRunning ("the destination reports the guest running; waiting briefly for the source's report")
      source: 11:40:03.051 vm_exited_post_send_migration → 11:40:03.257 dispatch_migration_send_complete elapsed_ms=155369
              → 11:40:03.290 w23_terminal_write_signal_fired id=t4-c2:send:1 completed=true → 11:40:03.290 w23_terminal_write_signal_received
      11:40:03 CutoverStep1 → "cutover: deleting source pod" → "cutover: completing" → 11:40:04.990 Completed
      observedDowntime=1.826605249s   observedTransferDuration=2m35.369s
events (t4-c2): DestinationPodCreated, Validated, DestinationPodReady, ReceiveIssued, SendIssued, CutoverStep1, Completed.  No SourceCompleteMissing.
```

| Criterion | Result |
|---|---|
| No "transferring guest state" before the source's `action_accept` for that send | ✔ `t4-a2`: accept 11:36:48.006 < transferring 11:36:48.206. `t4-c2`: accept 11:37:27.868 < transferring 11:37:28.168. `t4-b2` never transferred |
| Completing migration (`t4-c2`): the first `transferProgress` is real and low | ✔ first value **3** (11:37:33). The previous send's final estimate was 19–23 |
| `progress-estimate-id` names its send while transferring | ✔ `t4-c2:send:1` from 11:37:33.576 until the end |
| The source accepts its send within 60 s of `t4-a2`'s cancel | ✔ **24.3 s** (cancel 11:37:03.534 → accept 11:37:27.868). Round 2: ~10 min |
| `t4-b2` (cancelled while waiting): no action annotations after its `Cancelled`; never accepted | ✔ the terminal dump shows no `migration-action*`. The source log has **0** lines naming `t4-b2` for the whole life of that pod |
| R5's criteria: `Completed`, w23 `completed=true` before `received`, final `migration-status: complete` for its send, no `SourceCompleteMissing`, `observedTransferDuration` set | ✔ all. Watcher at 11:40:03.528: `status=complete status-id=t4-c2:send:1 status-detail=sent to tcp:<dst>:6789 (155369ms)`, then the pod was deleted at 11:40:04.898 |
| **`DestinationRunning` → source `complete`** | 11:40:00.925 → 11:40:03.257. The controller waited **~2.3 s** and cut over after the source's report |
| Sentinel and uptime in the survivor | ✔ on `t3g-mig-7e60ce` (cp-1): `VAL-T3-1790334959`, uptime 1273 → 1531 s |

## T5: cancel racing completion, three attempts back to back. PASS

**Setup.**
- The T4 guest `t3g`, with the rewriter stopped and a static 10 GiB of random
  tmpfs data.
- The source launcher is `t3g-mig-7e60ce` on cp-1, which is T4's destination
  pod, on `swiftletd:sha-f260277`. The target is worker-1.
- Each attempt starts as soon as the previous one has ended and the source no
  longer reads `sending`.

| | t1 (`transferProgress` ≥ 90) | t2 (~10 s before completion) | t3 (on `DestinationRunning`) |
|---|---|---|---|
| Start → "transferring guest state" | 11:41:40.7 → 11:41:55.2 | 11:44:43.7 → 11:44:58.9 | 11:47:52.6 → 11:48:07.9 |
| Source `action_accept` for the send | 11:41:55.061 | 11:44:58.687 | 11:48:07.616 |
| Cancel sent | **11:44:15.637** at a real **92 %** (the first reading ≥ 90) | **11:47:24.088** at 145 s, progress 95 | **11:50:40.876**, 0.17 s after `DestinationRunning` appeared (11:50:40.702) |
| Outcome | `Cancelled` 11:44:20.294 ("destination pod deleted after swiftletd cancel ack") | `Cancelled` 11:47:29.227 (the same) | **`CancelIgnored`** 11:50:41 ("post-cutover; migration cannot be reversed"), cutover 11:50:43, `Completed` 11:50:45.561 |
| Source CH reset → `migration_send_failed` | 11:44:22.410 → **11:44:42.595** (20.2 s after the reset, **27.0 s** after the cancel) | 11:47:31.340 → **11:47:51.494** (20.2 s / **27.4 s**) | n/a. The source logged `send_complete` at 11:50:43.166, then `w23 … id=t5-t3:send:1 completed=true` → `received` |
| Survivor | source `t3g-mig-7e60ce`: sentinel ✔, uptime 1581 → 1744 s ✔ | source: sentinel ✔, uptime 1933 s ✔ | **destination** `t3g-mig-fabf57` (worker-1): sentinel ✔, uptime 2129 s ✔ |
| `observedTransferDuration` / `observedDowntime` | – | – | **2m35.515s** / 2.272 s |
| `CancelAckTimeout` / `SourceCompleteMissing` | none / – | none / – | – / **none** |
| Gap to the next attempt | 23.1 s (Cancelled → source idle → the next create) | 23.0 s | – |

**t3 in detail.** `DestinationRunning` gave a window of about 2.6 s, and the
cancel landed inside it:

```text
dst 11:50:40.583 dispatch_migration_receive_complete id=t5-t3:recv:1 state=Running elapsed_ms=154511
dst 11:50:40.622 w16_guest_running_reported guest=val-r3/t3g
    11:50:40.702 condition DestinationRunning ("the destination reports the guest running; waiting briefly for the source's report")
    11:50:40.876 >>> cancelRequested=true
    11:50:41.1   condition CancelIgnored=True/PastCutover   (event CancelIgnored; no CancelIssued, and no MigrationCancel reached the destination)
src 11:50:43.166 dispatch_migration_send_complete id=t5-t3:send:1 elapsed_ms=155515
src 11:50:43.191 w23_terminal_write_signal_fired id=t5-t3:send:1 completed=true → w23_terminal_write_signal_received
    11:50:43 CutoverStep1 → "cutover: completing" → Resuming "waiting for guest health on destination" → 11:50:45.561 Completed
```

- **An improvement over round 2's R6-t3:** a post-commit cancel is now
  refused by the controller itself, without writing a cancel onto the
  destination. In round 2 the destination received the cancel and refused it
  with `destination_running`.
- **Every attempt:**
  - exactly one VM survived;
  - no SwiftMigration ended `Failed`;
  - the sentinel `VAL-T3-1790334959` survived with continuous uptime;
  - every cancel after a `Cancelled` attempt freed the source in about 27 s.
- **Source annotations:**
  - status ran `sending` → `failed` for t1 and t2, and `sending` → `complete`
    for t3;
  - `progress-estimate-id` named each attempt's own send;
  - the SwiftMigration's `transferProgress` read `null`, then 3, 6, 9… on
    every attempt. The previous send's final estimate (92–95) was never shown,
    unlike round 2.

## Findings to carry forward

1. **The migration script can put its guest on the wrong node** (T2, first run).
   With three nodes it cordons only the target, so the guest can land on the
   third node and the run fails. It has passed before only by luck of scheduling.
2. **A migration started right after a cancelled one, to the same node, fails
   validation.** This is T4's first run.
   - Validation counts the previous destination pod while it terminates, so a
     node with room for one 2-CPU destination rejects the second.
   - The error is `insufficient CPU headroom: need 2, have 290m`, and the
     migration ends `Failed` at once instead of waiting a few seconds.
   - Worker-2 hosts `innercp`, so it hit this. cp-1 did not.
3. **The script's `WARN: GuestRunning=` / `hypervisor=` are timing artefacts**
   (T1, seen in every round). They are read the instant the phase turns
   `Running`, just before both are set.

## State left on dev

- **Controller:** chart `0.0.0-dev.f260277` (rev 46), `--metrics-secure=true`,
  with the Prometheus SA bound as a reader.
- **Namespaces:** `val-r3`, `val-r3-d7a2` and `val-r3-d7b2` are deleted, and
  `val-r3-d7a` was deleted by the script. `val-r3-d7b` (the failed first T2 run)
  was deleted at 11:36 to free cp-1 for T4; its guest YAML and events were
  saved locally.
- **Unchanged:**
  - `gpu-cells/innercp` (uid `10bd28b8-…`, 0 restarts), the only SwiftGuest left;
  - the cluster-scoped class `val-migratable-16g`;
  - `val-d2` / `val-d3` (still Terminating, waiting on William).
- **Nodes:** none cordoned.
- **`default`:** holds only the lab content.
