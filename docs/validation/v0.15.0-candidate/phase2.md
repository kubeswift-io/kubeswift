# Phase 2: dev cluster suites on `d232581`

Run 2026-09-25, 00:12–02:05 UTC, after Phase 1 passed on dev (`phase1.md`).
Nodes are generalised: `cp-1` (untainted, KVM), `worker-1`, `worker-2` (GPU),
as in `phase0.md`. Pod IPs are redacted. `192.168.99.x` is the documented
per-launcher guest bridge range.

## ⛔ STOPPED at D9: a guest left with no running launcher

**Phase 2 was stopped here, per the stop condition.** D9 attempt 3 and D13
were not run.

A **plain live migration, with no cancel sent**, of a guest whose source pod
had already seen a cancelled send (D8) and a failed send (D9 attempt 2), left
this state:

| Object | State at 02:00 UTC |
|---|---|
| Source pod `val-d8/mig16` (worker-1) | **Succeeded.** Its VM exited after the send completed at 01:51:37 (`vm_exited_post_send_migration`). Its annotation still reads `migration-status=sending`, `status-id=d9-a2b:send:1`. |
| Destination pod `mig16-mig-1db32d` (worker-2) | **Running**. The receive completed at 01:51:34 (`dispatch_migration_receive_complete … state=Running elapsed_ms=153201`, `w16_guest_running_reported`). **The VM is alive here.** SSH from this pod reports `sentinel=VAL-D8-1790299842 uptime=1831`, continuous from 1122 s before the migration. |
| SwiftMigration `d9-a2b` | **Stuck in `StopAndCopy` / "transferring guest state"** since 01:49:01, with no cutover. `cancelRequested` was never set. `timeout: 30m0s`, `timeoutStrategy: cancel`. |
| SwiftGuest `mig16` | **`phase: Stopped`**, `GuestRunning=False/LauncherExited` (01:51:39), `podRef` = the Succeeded source pod, no IP. The status says Stopped while the VM runs on the destination. |

**Likely cause: a stale terminal-write signal in the source swiftletd.** This
is inferred from its log. Every earlier send on this pod fired the W23 signal:

```text
01:42:00.256 w23_terminal_write_signal_fired id=d8-cancel:send:1     <- D8's cancelled send, failed at its 600 s deadline
01:47:37.367 w23_terminal_write_signal_fired id=d9-a2:send:1         <- D9 attempt 2, failed instantly
01:49:01.685 dispatch_migration_send id=d9-a2b:send:1 ...
01:51:37.064 vm_exited_post_send_migration; skipping VmStopped report (W22)
01:51:37.064 w23_terminal_write_signal_received; safe to exit        <- no "fired id=d9-a2b" before it
```

The completed `d9-a2b` send never fired its own signal, and the process exited
on a signal left over from an earlier send. Its `completed` status was
therefore never written. The controller logged nothing about `d9-a2b` at
default verbosity. Its last event is `DestinationPodReady … advancing to
StopAndCopy` at 01:49:01.

**What happens next** (not yet observed). The migration times out at ~02:18:46
with `timeoutStrategy: cancel`. D9 attempt 2 below showed the controller
deleting the destination pod after a pre-cutover failure
(`DestinationPodCleanedUp: deleted destination pod … after pre-cutover Failed`).
If the timeout takes that path, it deletes the only running VM, which is the
#653 failure class. I am watching without intervening, and will append the
outcome below.

### Update, 02:26 UTC: the timeout deleted the only running VM

I watched through the timeout without intervening:

```text
02:18:47 SwiftMigration d9-a2b  Failed  failureReason=Timeout  "spec.timeout=30m0s exceeded since StartedAt; migration did not complete in time"
02:18:47 event DestinationPodCleanedUp  deleted destination pod "mig16-mig-1db32d" after pre-cutover Failed
02:18:47 dst swiftletd: sigterm_received; requesting guest ACPI power-off -> 02:18:52 vm_stopped_gracefully
02:18:52 dst swiftletd: ERROR report_failed: ApiError: swiftguests.swift.kubeswift.io "mig16-mig-1db32d" not found
02:26:22 SwiftGuest mig16: runPolicy=Running, phase=Stopped, podRef=mig16 (the Succeeded source pod), no IP;
         pods for the guest: only mig16 (Succeeded). No launcher has been re-created.
```

- **The guest now has no running VM and no launcher.** The migration completed
  at the VM level at 01:51:34, and the controller then destroyed the migrated VM
  at the timeout.
- The ACPI power-off was graceful, so the RWX disk should be consistent, but
  the guest's in-memory state is gone. The tmpfs sentinel is lost.
- **Separate small defect:** the destination swiftletd reports to a SwiftGuest
  named after its *pod* (`mig16-mig-1db32d`), not after the guest (`mig16`).

**Everything is left in place for inspection.** Namespace `val-d8` holds guests
`mig16` and `mig16b`, SwiftMigrations `d8-cancel`, `d9-a1`, `d9-a2` and
`d9-a2b`, and the cluster-scoped SwiftGuestClass `val-migratable-16g`.
Evidence was collected locally: YAML, describe, events, controller log, and
both launcher logs with `--previous`.

## Results

| # | Scenario | Verdict |
|---|---|---|
| D1 | Boot smoke | disk-boot, kernel-boot, qemu-boot, multi-nic **PASS**. gpu-alloc **FAIL**: a stale test, not the candidate (see D1) |
| D2 | In-place restore (#657) | **PASS** |
| D3 | Clone restore (Tier B) | **FAIL**: dev CPU capacity, plus a known non-OVN-K limitation (see D3) |
| D4 | Snapshot dirs (#649) | **PASS** |
| D5 | Tier A CSI snapshot | `snapshot-test` **PASS**. `clonestrategy-test` **FAIL** on its 3× speed criterion only: Longhorn clones by full copy; the functional path works |
| D6 | Cross-node TCP | **PASS** |
| D7a | Offline migration (fixed script) | **PASS** |
| D7b | Live migration (fixed script) | **PASS** |
| D8 | Cancel mid-transfer (#653) | **PASS** on the plan's criteria, **with two defects**: the cancel falls straight through to force-deleting the destination, and the source stays stuck in its send (see D8) |
| D9 | Cancel racing completion (#653) | Attempt 1 Cancelled, OK. Attempt 2 blocked by D8's stuck send. The attempt-2 re-run then **left the guest with no running launcher** (above). **STOPPED** |
| D10 | TokenRequest gate (#652) | **PASS** |
| D11 | Gateway exec gate (#655) | **PASS** |
| D12 | Console through the UI (#655) | My half is done: **0 Access-editor roles exist**, so the migration changed nothing. The browser half is William's (steps below) |
| D13 | Secure metrics (#656) | **NOT RUN**: Phase 2 stopped first. Prepared: Prometheus runs as `monitoring/monitoring-kube-prometheus-prometheus` |

## Observation requested by William: guests share the same `primaryIP`

Every nat-mode launcher runs its own `br0` (`192.168.99.1/24`) and dnsmasq,
over the same `192.168.99.10–20` range. So `status.network.primaryIP` is only
unique inside one launcher pod, and different guests routinely report the
**same** address. Seen during this run:

- **dev, at the same moment:** `gpu-cells/innercp` = `192.168.99.20` and
  `val-d8/mig16` = `192.168.99.20` (both Running, 01:39).
- **dev, concurrently:** D1's `multi-nic-test` and D2's `snapshot-local-source`
  were both `192.168.99.16` (00:22).
- **Across clusters:** D1's `faas-test` and ntx's CAPI control-plane guest were
  both `192.168.99.10`.
- **Across a migration:** a guest keeps its address. D7b stayed `192.168.99.11`,
  and D9's `mig16` stayed `192.168.99.20` (`d3_guest_ip_propagated`), so a
  collision follows the guest to its new node.

The address is isolated in each pod's network namespace, so it does no harm on
the wire. It is still bad practice for an object's *status*. Anything that
keys on `primaryIP` conflates guests: the UI, `swiftctl` by address,
monitoring labels, and scripts. Operators reading `kubectl get swiftguest -A`
see duplicates with no hint that the address is pod-local. At minimum the
status should say so. Better: allocate distinct addresses per node or per
cluster, or report a routable address next to the pod-local one.

---

## D1: boot smoke

`make smoke-test` (`boot-test.sh --no-cleanup`), 00:12:48–~00:34.

- **It ran in `default`.** `NAMESPACE=val-d1` fails immediately
  (`the namespace from the provided object "default" does not match the
  namespace "val-d1"`): the samples hard-code `namespace: default`. This is a
  script limitation.
- The existing lab SwiftImage `default/ubuntu-noble` matched the sample exactly
  (`kubectl diff` empty), so the test reused it.
- **Cleanup ran by hand, not with `make smoke-test-cleanup`.** It deleted
  every test object but kept `default/ubuntu-noble`, because the make target
  would delete that pre-existing lab image.

```text
disk-boot       PASS   (sample, primaryIP=192.168.99.15)
kernel-boot     PASS   (faas-test, 192.168.99.10)
qemu-boot       PASS   (qemu-test, hypervisor qemu, 192.168.99.18)
gpu-alloc       FAIL   GPUAllocated=False reason=NoCapacity "no SwiftGPUNode has sufficient free GPUs matching the profile"
multi-nic       PASS   (multi-nic-test, 192.168.99.16)
```

- **gpu-alloc is a test defect that predates the candidate.** Since v0.14.1
  (`7fbe097`, "allocate only on nodes the workload can run on"), allocation
  passes through `nodeUsable`, which requires `status.vfioReady` and an
  existing, uncordoned Node. The scenario's `mock-gpu-node` SwiftGPUNode is
  `Ready`, has 1 free `A100-PCIe` matching the profile, but has no `vfioReady`
  and no Node object, so it is refused by design.
- **`WARN: GuestRunning=` / `hypervisor=`.** The script reads these the instant
  `phase=Running` lands. A minute later `sample` showed `GuestRunning=True/VmRunning`
  and `runtime.hypervisor=cloud-hypervisor`. This is a timing race in the check.

## D2: in-place restore of a running guest (#657). PASS

- **Command:** `local-roundtrip-test.sh --namespace val-d2 --no-cleanup`, with
  `KUBESWIFT_TEST_IDENTITY` set to an ephemeral key.
- **Timeline:** a 2 s poller of guest, snapshot and restore.

```text
Captured on node <cp-1> (pause window 3610ms)
OK: launcher pod is in restore-receive mode, no stager (in-place fast path)
OK: sentinel survived: kubeswift-roundtrip-1790295735-32149
=== Tier B round-trip e2e PASS ===
```

| Check | Value |
|---|---|
| Guest `primaryIP` before the snapshot | `192.168.99.16` |
| Snapshot `status.guestSpec.primaryIP` | `192.168.99.16` |
| Guest `primaryIP` after the restore | `192.168.99.16`, reported by the restore launcher at 00:22:32, before Ready; the script's post-restore SSH used it |
| SwiftRestore duration | `startedAt` 00:22:26 → `completedAt` 00:22:37 = **11 s** (> ~5 s) |
| Restore phases | Restoring → Resuming → Ready |

**Side observation.** Before its SwiftImage existed, the guest showed
`phase: Failed` (00:18:34–00:20:13), then recovered by itself. A missing image
reads as Failed, not as waiting.

## D3: clone restore (Tier B). FAIL (environment and a known limitation)

- **Command:** `local-clone-identity-test.sh --namespace val-d3 --no-cleanup`.
- **Clone A:** `GuestRunning=True` after 126 s, and its SwiftRestore was Ready
  after 128 s.
- **Clone B:** `FAIL: snapshot-local-clone-b never reached GuestRunning=True`.
  Its pod stayed Pending: `0/3 nodes are available: 1 Insufficient cpu, 2
  node(s) didn't match Pod's node affinity/selector`.
- **Why there was no room.** The clones are pinned to the capture node, which
  holds the node-local memory image. That node already had 6390m of 8000m CPU
  requested. The source and clone A request 2 CPUs each, so there was no room
  for a third. No dev node can hold three 2-CPU guests on top of its base load.
- **Clone A has no IP.** It is Running with no `status.network`. This is the
  documented non-OVN-K limitation: a resumed clone keeps the source's address
  in RAM and never re-DHCPs. Only OVN-K derives it from `k8s.ovn.org/pod-networks`.
  So even with capacity, the script's SSH-based identity checks cannot run on dev.
- **Manual check, by SSH from clone A's own launcher to its in-RAM address:**
  clone A's machine-id, ed25519 host-key fingerprint and hostname are
  **identical** to the source's, and `/var/lib/kubeswift/.clone-regenerated` is
  **absent** after ~390 s uptime.
  - This matches the sample seed's own note: its regeneration bootcmd "does
    NOT fire on snapshot resume", and there is no `spec.guestAgent`.
  - Identity regeneration on a resumed clone therefore needs a reboot or the
    vsock guest agent, and this test configures neither.
- **Script defect.** `identity_of` reads `/sys/class/net/eth0/address`, but
  Ubuntu Noble names the NIC `ens3`, so the MAC field is always `missing`.
- **Suggestion:** run D3 on ntx (OVN-K) with smaller guests.
- Evidence (describe, YAML, events, controller and launcher logs) was
  collected locally before deleting `val-d3`. The namespace is now stuck; see
  the snapshot finalizer below.

## D4: snapshot dirs (#649). PASS

- D2's SwiftSnapshot: `status.memorySnapshot.handle` =
  `/var/lib/kubeswift/snapshots/val-d2_snapshot-local-mem`. The ntx run gave
  `…/val-n2r_snapshot-local-mem`.
- A local SwiftSnapshot with `spec.backend.local.hostPath: /var/lib/kubeswift/snapshots/elsewhere`:

```text
--dry-run=server and real create:
Error from server (Forbidden): admission webhook "vswiftsnapshot.snapshot.kubeswift.io" denied the request:
spec.backend.local.hostPath must be omitted or be /var/lib/kubeswift/snapshots/val-d2_val-d4-bad-hostpath,
the directory derived from the snapshot's namespace and name (got "/var/lib/kubeswift/snapshots/elsewhere")
control (the derived path, dry-run): swiftsnapshot … created (server dry run)
```

### Defect found while cleaning up (predates the candidate): a namespace holding a local snapshot never finishes deleting

- `val-d2` has been `Terminating` since 00:50:49, and `val-d3` since 01:05:46.
- Each is held by its SwiftSnapshot's `kubeswift.io/snapshot-hostpath-cleanup`
  finalizer. The finalizer creates the cleanup pod **in the snapshot's own
  namespace**, and the API server refuses that in a terminating namespace:

```text
E ... "Reconciler error" "error"="create cleanup pod: pods \"swift-snap-cleanup-snapshot-local-mem\" is forbidden:
unable to create new content in namespace val-d3 because it is being terminated"   (retried indefinitely)
```

- **It predates the candidate:** v0.14.1's `cleanup.go` also creates the pod in
  `snap.Namespace`, and the finalizer dates from `234ea3e`.
- The snapshot directories remain on their nodes. Both namespaces are left
  stuck for inspection; nothing was removed by hand.

## D5: Tier A CSI snapshot

- **`snapshot-test.sh --vsclass longhorn-snapshot-vsc` (ns `val-d5`): PASS.**
  The VolumeSnapshot `swift-snap-snapshot-e2e-snap` was readyToUse with a
  matching handle, the restore was Ready, and the restored guest ran.
- **`clonestrategy-test.sh --vsclass longhorn-snapshot-vsc` (ns `val-d5b`): FAIL
  on the speed criterion.**

  ```text
  copy avg 78s (78 79)   snapshot avg 110s (76 144)   speedup 7/10 x (min 30/10 x)
  ```

  The functional path is correct: `swiftguest-root-cs-snap-{1,2}` were
  provisioned from `dataSource: VolumeSnapshot cs-source-snap-clone-seed`
  (readyToUse, `longhorn-snapshot-vsc`). The script itself notes that Longhorn
  implements snapshot+dataSource as a full copy.
- The `ceph-block` + `ceph-block-vsc` pair would exercise a CoW clone, but Ceph
  is `HEALTH_WARN` (see `phase0.md`).

## D6: cross-node TCP. PASS

`make b0-cross-node-tcp-test`, then `-cleanup`. The clean run started at 00:54:27:

```text
br0 IP: 192.168.99.1/24   PASS — within 192.168.99.0/24 default
br0 /24 prefix 192.168.99.0/24 ≠ eth0 /24 prefix   PASS — distinct /24 prefixes
launcher pod on worker-2 listens; probe pod on worker-1 connects → probe pod Succeeded — TCP connection established
==> ALL CHECKS PASSED
```

- The first attempt failed with `OutOfcpu`: the script pins its guest to
  worker-2, where D1's guests still held CPU.
- I freed D1's guests and re-ran D6, but the first attempt was still polling and
  checked the new guest at the same time.
- The clean run above was a third, isolated run.

## D7a: offline migration (fixed script). PASS

- **Command:** `migration-test.sh --mode offline --source <worker-1> --target
  <worker-2>`, with an ephemeral key and `swiftctl` built from `d232581`.

```text
Guest running at IP=192.168.99.14 on node=<worker-1>
Migration completed in 112s (resolved mode: offline)
PASS: disk sentinel survived the migration
PASS: webhook rejected migration of guest with migration.enabled=false
All checks passed.
```

- **Phase sequence (0.5 s poller):**
  - 01:21:12 Preparing "waiting for source pod termination";
  - 01:21:17 "waiting for volume detach";
  - 01:21:23 StopAndCopy "patching SwiftGuest to start on destination node";
  - 01:21:23 Resuming "awaiting destination launcher start";
  - 01:22:36 "awaiting primaryIP discovery";
  - 01:22:58 **Completed** (`guest running on <worker-2> with IP 192.168.99.13`).
- **Nodes before and after:** all three schedulable. No cordon was left, and no
  foreign cordon existed to lift.

## D7b: live migration (fixed script). PASS

- **Command:** `migration-test.sh --mode live --guest-class small-migratable
  --source <worker-1> --target <worker-2> --no-cleanup`.

```text
Guest uptime before: 24s
Migration completed in 38s (resolved mode: live)
Post-migration pod e2e-guest-mig-84f47a on <worker-2>
PASS: disk sentinel survived the migration
PASS: tmpfs sentinel survived and uptime kept counting (24s -> 67s): the running VM moved
PASS: webhook rejected migration of guest with migration.enabled=false
All checks passed.
```

- **Phases:**
  - 01:26:43 Preparing;
  - 01:26:58 StopAndCopy "transferring guest state" (26 % → 52 % → 79 %);
  - 01:27:19 "cutover: completing";
  - 01:27:20 **Completed**.
- **SwiftMigration status:** `mode: live`, downtime **1.81 s**, transfer 19.8 s.
- **Before cleanup:** `pods "e2e-guest" not found` (the source is gone).
  `status.podRef` = `e2e-guest-mig-84f47a` (Running on worker-2).
- No cordon was left. `migration-e2e` was deleted afterwards.

## D8: cancel mid-transfer (#653). PASS on the criteria, with two defects

- **Setup:**
  - class `val-migratable-16g` (`small-migratable` with 16Gi);
  - guest `val-d8/mig16` on worker-1;
  - a 10 GiB tmpfs filled from `/dev/urandom` and rewritten in a loop;
  - the tmpfs sentinel `VAL-D8-1790299842`, uptime 41 s.
- **Action:** a live migration to worker-2, with `spec.cancelRequested: true`
  set 15 s into "transferring guest state".

```text
01:31:43.0 Preparing "preparing destination launcher pod"
01:31:43.4 Preparing "waiting for destination pod ready"   dst=mig16-mig-c6ed64
01:31:59.3 StopAndCopy "transferring guest state"   (3% … 6%)
01:32:14.8 >>> cancelRequested=true
01:32:15.3 Cancelled  "destination pod was never created; cancel completes without swiftletd ack"
```

**Pass criteria, all met:**
- Cancelled.
- The source pod has the same UID (`a4934cac-…`) and 0 restarts, and stays
  `podRef`.
- The sentinel is present, and uptime is continuous (41 s at 01:30:45, 148 s at
  01:32:30).
- SSH works.
- The destination pod is gone.

**Defect 1: the 30 s cancel-ack budget is never honoured, and the message is
wrong.** Events, all within one second:

```text
01:32:14 CancelIssued      wrote cancel annotation on destination pod "mig16-mig-c6ed64" (id=d8-cancel:cancel:0); awaiting swiftletd ack
01:32:14 CancelAckTimeout  swiftletd cancel ack not observed within 30s; force-deleting destination pod "mig16-mig-c6ed64"
01:32:14 Cancelled         migration cancelled: destination pod force-deleted; swiftletd cancel ack timed out (30s budget)
01:32:14 Cancelled         migration cancelled: destination pod was never created; cancel completes without swiftletd ack   <- final status
```

- The destination pod did exist and was receiving.
- D9 attempt 1 repeated the same pattern at 95 %. There the destination's
  swiftletd log was captured, and it **never receives a cancel action**. It is
  SIGTERMed (`sigterm_received` at 01:46:33.875) 0.4 s after the cancel
  annotation was written.
- So swiftletd's graceful cancel path is never used; the fallback fires at once.
- D8's destination launcher log could not be captured: my streamer attached
  while the pod was still initialising, and the pod was deleted before I retried.

**Defect 2: the source stays stuck in its send, and the guest cannot be
migrated again.**
- The force-deleted destination never sends a RST. The source Cloud Hypervisor
  kept an **ESTABLISHED** TCP connection to the vanished destination IP on port
  6789, with about 3.8 MB in its send queue.
- The source swiftletd reported `migration-status=sending` for **600 s**, then:
  `migration_send_failed … source guest still Running past the migration
  deadline (v53 auto-resumes the source on a failed transfer)` (01:42:00).
- The CH send itself ended only at 01:48:11: `Migration failed: … Operation
  timed out (os error 110)`. That is TCP give-up, **~16 min after the cancel**.
- Until then a new migration of this guest fails (D9 attempt 2).
- The VM kept running throughout.

## D9: cancel racing completion (#653)

Each guest had a static 10 GiB of random data in tmpfs with no rewriter, so the
transfer converges in about 2.5 min.

**Attempt 1: `mig16b`, cp-1 → worker-2, cancel at `transferProgress ≥ 95`.**

```text
01:44:07 StopAndCopy "transferring guest state" … 3% every ~5 s …
01:46:33.0 >>> cancelRequested=true (95%)
01:46:33.4 Cancelled  "destination pod force-deleted; swiftletd cancel ack timed out (30s budget)"
```

- Exactly one VM survives: the source, with the same UID (`89c264e3-…`), 0
  restarts, sentinel `VAL-D9-1790300574` intact, uptime continuous (104 → 289 s)
  and SSH working.
- The destination pod is gone. Its swiftletd log shows no cancel action before
  `sigterm_received` (Defect 1).
- The cancel came at 95 %, a few seconds before completion; it did not race
  cutover itself.

**Attempt 2: `mig16`, worker-1 → worker-2, cancel on "src migration complete".**

```text
01:47:36.8 StopAndCopy "transferring guest state" progress=95   <- stale: D8's progress-estimate on the source pod
01:47:37.7 Failed  RpcError "source reported migration failure: send_migration: internal_server_error"
```

- CH refused the send because D8's send was still hung (Defect 2).
- The destination pod was cleaned up (`DestinationPodCleanedUp … after
  pre-cutover Failed`), and the source VM was unaffected.
- A new SwiftMigration inherited the old progress value, 95.

**Attempt 2 re-run: `mig16`, after the CH send died at 01:48:11, same trigger.**

This produced the stop at the top of this report. The trigger never fired,
because the phase never reached "src migration complete". The migration
completed at the VM level, the source exited, and the controller never cut over.

**Not run:** attempt 3, and D13.

## D10: TokenRequest gate (#652). PASS

```text
kubectl create ns val-tok; kubectl create sa kubeswift-launcher -n val-tok
kubectl create rolebinding val-tok-admin --clusterrole=admin --user=val-tok-user -n val-tok
kubectl auth can-i create serviceaccounts --subresource=token -n val-tok --as=val-tok-user   -> yes
kubectl create token default -n val-tok --as=val-tok-user                                    -> eyJhbGciOiJSUzI1NiIs… (control: allowed)
kubectl create token kubeswift-launcher -n val-tok --as=val-tok-user
  error: failed to create token: serviceaccounts "kubeswift-launcher" is forbidden: ValidatingAdmissionPolicy
  'kubeswift-launcher-sa-tokenrequest-gate' with binding 'kubeswift-launcher-sa-tokenrequest-gate' denied request:
  tokens for the KubeSwift launcher ServiceAccounts are reserved: …
```

- The SA was created by hand. The controller creates it only with a first guest.
- A TokenRequest for an SA that does not exist gets `NotFound` before admission
  runs (checked with `kubeswift-sandbox-launcher`).
- `val-tok` was deleted.

## D11: gateway exec gate (#655), policy. PASS

- **Setup:** as `--as=system:serviceaccount:kubeswift-system:kubeswift-gateway`,
  against D1's `sample` guest. The launcher is `status.podRef`, container
  `launcher`.
- **The command:** `consoleBridge("default","sample")`, taken verbatim from
  `internal/gateway/exec_bridge.go` @ `d232581`.

```text
sh -c id  ->  The pods "sample" is invalid: : ValidatingAdmissionPolicy 'kubeswift-gateway-exec-gate' … denied request:
              the kubeswift gateway's credential may exec only the console, sandbox shell and sandbox log bridges, in a launcher container
bridge + "; id" appended  ->  same denial
bridge, with a TTY as the gateway uses  ->  admitted; serial console output:
              Ubuntu 24.04.4 LTS kubeswift-guest ttyS0 / kubeswift-guest login:
              (held open until my 12 s timeout interrupted it)
```

- **Without a TTY, the bridge is admitted, but socat exits:**
  `tcgetattr … Inappropriate ioctl for device`. That comes from `raw,echo=0`.
  The gateway always execs with TTY.
- **The "default container" control was not a valid test.** kubectl fills in
  `container=launcher` from the default-container annotation, so that request
  was admitted.

## D12: console through the UI (#655)

**My half.** The CHANGELOG `jq` migration was run as a dry run over
`clusterroles -l kubeswift.io/role=true`, then verbatim:

```text
roles labelled kubeswift.io/role=true: (none)      dev, and also sov and ntx
roles iterated: 0          -> no role changed
```

- None of the predefined roles (`kubeswift-viewer/operator/admin`) exists yet:
  the gateway creates them the first time one is assigned.
- There is therefore no existing editor-created role to inspect.
- From the code at `d232581`, the Console capability maps to
  `create swiftguests/console`, `create swiftsandboxes/exec` and
  `get swiftsandboxes/log`, and not to `pods/exec`
  (`internal/gateway/access_capabilities.go`).

**William's half (browser).**
1. In the UI, **Access**, create a custom role **with** the Console capability
   (for example `val-console`), and one **without** it (for example
   `val-noconsole`). Bind each to a test identity or group on the local cluster.
2. As a cluster admin, confirm the saved role carries the new rule:
   `kubectl get clusterrole val-console -o jsonpath='{.rules}'`
   It should show `swiftguests/console` `create`, and **no** `pods/exec`.
3. Signed in as the identity with Console, open any running guest, and choose
   **Console**. The serial console should connect.
4. Signed in as the identity without Console, do the same. It should get **403**
   `cannot create swiftguests/console`.
5. Optional: repeat step 3 for a sandbox (Shell, Logs).

## D13: secure metrics (#656). NOT RUN (Phase 2 stopped)

Prepared for when it is resumed:
- Prometheus runs as `ServiceAccount monitoring/monitoring-kube-prometheus-prometheus`.
- The ServiceMonitor `kubeswift-controller-manager` currently scrapes `http` on
  port `metrics` (8080).
- The controller still runs `--metrics-secure=false`, and dev's values are
  unchanged by this phase.

## State left on dev

| What | Where | Why |
|---|---|---|
| The stuck migration and its guests | `val-d8` (`mig16`, `mig16b`, 4 SwiftMigrations), cluster-scoped class `val-migratable-16g` | stop condition, for inspection |
| Two namespaces stuck `Terminating` | `val-d2`, `val-d3` | the local-snapshot finalizer defect (D4) |
| Unchanged | `gpu-cells/innercp` (uid `10bd28b8-…`, 0 restarts) | not touched by any scenario |

- All other test namespaces are deleted.
- No node is cordoned.
- `default` holds only the lab's original content.
