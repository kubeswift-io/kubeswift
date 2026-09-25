# Lab validation: v0.15.0 candidate

Written by the cloud session driving the release (it cannot reach the lab). It
is run by a Claude session on William's machine, where the kubeconfigs are.

**Channel:** this branch, `validation/v0.15.0-candidate`.
- Write one report per phase under this directory: `phase0.md`, `phase1.md`, …
- Commit only report files and push this branch. Pull before each push.
- Never push to `main`, and never open a PR.
- Stop after each phase and wait: the driving session reads the report and
  pushes the go-ahead for the next phase as a commit to this file (see
  **Go-ahead** at the bottom). `git pull` and check it before continuing.

**Rules:**
- Pass `--kubeconfig <path>` (or set `KUBECONFIG` per command) on every
  command, for the right cluster.
- Do not fix anything. On a failure, collect evidence (below), keep the failed
  resources for inspection, and carry on with the next independent scenario.
- Each scenario gets **PASS / FAIL / SKIPPED (why)**, the commands run, and the
  lines of output that prove the result.
- Evidence on failure:
  - `kubectl describe` of the objects involved, and events in the namespace;
  - controller-manager logs since the scenario started;
  - launcher logs for every guest involved, including `--previous`;
  - `kubectl get <kind> -o yaml` of the SwiftGuest, SwiftSnapshot,
    SwiftRestore or SwiftMigration.
- Use a namespace per scenario, named `val-<scenario>`, and delete it on PASS.

| Cluster | Kubeconfig | Notes |
|---|---|---|
| dev | `/home/wrkode/code/vmm-kubeswift/dev-tests/kubeswift/kubeswift-cluster.yaml` | primary lab, federation hub, GPU node |
| sov | `/home/wrkode/sovhetzner-kubeconfig.yaml` | Hetzner, controller only, no `/dev/kvm` |
| ntx | `/home/wrkode/code/vmm-kubeswift/ntx-cluster.kubeconfig` | OVN-Kubernetes primary CNI, CAPI guests |

What is under test (merged since v0.14.1):

| PR | Change |
|---|---|
| #649 | snapshot node dirs are `<ns>_<name>`, recorded in `status.memorySnapshot.handle`; digest-pinned base images |
| #652 | sharing-aware shared-base eviction (C23); TokenRequest VAP for launcher ServiceAccounts (S8) |
| #653 | swiftletd runs actions off its loop; a migration cancel cannot kill a completed migration |
| #657 | an in-place restore resumes the snapshot (it used to boot the guest cold) and keeps its address |
| #655 | G9: the UI console, sandbox shell and logs no longer use the user's `pods/exec`; `kubeswift-gateway-exec-gate` VAP |
| #656 | G15: `/metrics` is HTTPS and authorized by default (merging as this is written) |

## Phase 0: read-only reconnaissance (change nothing)

1. For each cluster:
   - Kubernetes version.
   - Nodes: which are KVM-capable, and which carry kubeswift labels (e.g. `kubeswift.io/basedisk-node`).
   - The kubeswift Helm release: name, namespace, chart version.
   - `helm get values` for it (user-supplied only; redact secrets).
   - The controller-manager image and readiness.
   - Whether each of these is installed: webhook, gateway, UI, cert-manager, Prometheus Operator, migration mTLS issuer.
   - `federation.role`.
   - ValidatingAdmissionPolicies present, by name.
   - Storage classes: which is the default, which are RWX, whether snapshot-capable (Longhorn?).
   - Installed CRD versions: does `swiftsnapshots` have `status.guestSpec`?
2. **Activity in progress:** SwiftGuests, SwiftSnapshots, SwiftRestores and SwiftMigrations in all namespaces (age, phase), and namespaces created in the last 6 hours. Flag anything that looks like another validation still running. Phase 1 must not start while one is.
3. **dev only:** can it run a live migration (two KVM nodes, an RWX class)? Which storage class suits the snapshot suites?

## Phase 1: upgrade to the candidate (only after the Go-ahead)

The go-ahead names the main commit `<sha>` (7 chars) whose dev build to use.
- Chart: `oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.<sha>`
- Images: every kubeswift image is tagged `sha-<sha>`.

For each cluster, in the order dev, ntx, sov:
1. **CRDs first.** `helm upgrade` does not update CRDs, and #657 adds `status.guestSpec.primaryIP`; a stale CRD silently drops it. `git checkout <sha>` in your checkout, then `kubectl apply --server-side --force-conflicts -f charts/kubeswift/crds/`.
2. `helm upgrade <release> oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.<sha> -n <ns> --reuse-values`, with `--set <component>.image.tag=sha-<sha>` for each image the values set:
   - controllerManager
   - swiftletd
   - sandboxMaterialize
   - snapshotORAS
   - migrationStunnel
   - gpuDiscovery
   - dra
   - gateway

   Leave `ui.image.tag` as it is.
3. `kubectl rollout status` for every kubeswift Deployment and DaemonSet.
4. Report the controller image, its `--metrics-secure` flag value (expect `false` under `--reuse-values`; that is by design), and the VAPs now present.

## Phase 2: dev cluster suites (only after the Go-ahead)

Run with `KUBECONFIG` pointing at dev, from the checkout at `<sha>`.

| # | Scenario | How | Pass |
|---|---|---|---|
| D1 | Boot smoke | `make smoke-test` (then `make smoke-test-cleanup`) | every scenario the cluster supports PASS |
| D2 | In-place restore of a running guest (#657) | `make local-roundtrip-test`; key via `KUBESWIFT_TEST_IDENTITY` (see the e2e workflow's "Generate ephemeral SSH keypair" step) | sentinel survives; the post-restore SSH address equals the captured one; SwiftRestore took more than ~5 s (it used to be Ready in 3 s) |
| D3 | Clone restore (Tier B) | `make local-clone-identity-test` | passes (on OVN-K the clone's IP comes from `k8s.ovn.org/pod-networks`) |
| D4 | Snapshot dirs (#649) | inspect D2's SwiftSnapshot before cleanup, or create one: `status.memorySnapshot.handle` = `/var/lib/kubeswift/snapshots/<ns>_<name>`. Create a local SwiftSnapshot with `spec.backend.local.hostPath: /var/lib/kubeswift/snapshots/elsewhere`. | handle as expected; the bad hostPath is refused (admission if the webhook is on, else the snapshot goes Failed before capturing) |
| D5 | Tier A CSI snapshot | `make snapshot-test` and `make clonestrategy-test` | pass |
| D6 | Cross-node TCP | `make b0-cross-node-tcp-test`, then `make b0-cross-node-tcp-test-cleanup` | pass |
| D7 | Live migration (#653) | `test/migration/migration-test.sh` (read its header for arguments) | migration Succeeded; guest reachable; source pod gone |
| D8 | Migration cancel mid-transfer (#653) | give a guest enough memory to make the transfer take several seconds, start a live migration, and cancel while phase is transferring (docs/migration/phase-3a.md, "Cancelling a migration") | SwiftMigration Cancelled; the guest keeps running on the source and is reachable; the destination pod is gone; no VM killed |
| D9 | Cancel racing completion (#653) | repeat D8 but cancel just as the migration completes | either Cancelled with the source running, or Succeeded with the destination running; never neither (no guest left without a running VM) |
| D10 | TokenRequest gate (#652) | as a user bound to `admin` in `val-tok`, try `kubectl create token kubeswift-launcher -n val-tok` (via `--as`) | refused by `kubeswift-launcher-sa-tokenrequest-gate` |
| D11 | Gateway exec gate (#655), policy | the gateway credential is `system:serviceaccount:<ns>:kubeswift-gateway`. With a running guest: `kubectl exec --as=<that SA> <launcher-pod> -c launcher -- sh -c id` | refused by `kubeswift-gateway-exec-gate`; the same exec with the console bridge command from `internal/gateway/exec_bridge.go` (`consoleBridge(ns, guest)`) is admitted (connects; interrupt it) |
| D12 | Console through the UI (#655) | a user with the Console capability opens a guest console; a user without it tries | the first works; the second gets 403 `cannot create swiftguests/console`. Also run the CHANGELOG `jq` role migration and report which Access-editor roles it changed |
| D13 | Secure metrics (#656), dev only | `helm upgrade ... --reuse-values --set controllerManager.metrics.secure=true`, then from a debug pod: plain `http://<pod-ip>:8080/metrics`; `https` with an unbound SA token; `https` after binding that SA via `controllerManager.metrics.readers` | plain HTTP fails; unbound token gets 403; bound token gets 200 with `kubeswift_` series; a Prometheus ServiceMonitor, if present, shows the target up once its SA is a reader |

## Phase 3: ntx and sov (only after the Go-ahead)

| # | Cluster | Scenario | Pass |
|---|---|---|---|
| N1 | ntx | `make smoke-test` (disk-boot) on the OVN-K primary network | pass; guest reachable at its OVN address |
| N2 | ntx | `make local-roundtrip-test` | sentinel survives; the guest keeps its OVN address after the restore |
| N3 | ntx | a CAPI-managed guest still reconciles after the upgrade (no restart, no status churn) | unchanged |
| S1 | sov | controller Ready; webhooks answering (`kubectl apply --dry-run=server` of a sample SwiftGuest); VAPs present | pass |

## Go-ahead

Phase 0: **DONE** (`phase0.md`). Thanks — the heads-ups are all taken below.

**Candidate: main @ `2d146eb`** (#649, #652, #653, #655, #656, #657). William has
confirmed the v0.14.1 validation session is finished.

Phase 1: **GO**.
Phase 2: **GO** once Phase 1 has succeeded on dev.
Phase 3: **GO** per cluster, once Phase 1 has succeeded on that cluster.
Push `phase1.md`, `phase2.md` and `phase3.md` as each finishes; do not wait for
another go-ahead between them.

**Stop conditions:**
- If the upgrade or rollout fails on a cluster, collect the evidence, run
  nothing more on that cluster, and continue with the others.
- If a scenario leaves a guest without a running VM (lost, killed, or stuck with
  no launcher), stop Phase 2 there and report at once.

### Amendments from Phase 0 (these override the tables above)

**Phase 1:**
- **Image published?** Before step 1, check the build exists:
  - `helm show chart oci://ghcr.io/kubeswift-io/charts/kubeswift --version 0.0.0-dev.2d146eb`
  - the image `ghcr.io/kubeswift-io/kubeswift/controller-manager:sha-2d146eb`

  The Release Dev run for `2d146eb` was still publishing at 22:58 UTC. If either
  is missing, re-check every 5 minutes for up to 45 minutes, then report and stop.
- **Also set `snapshotS3.image.tag=sha-2d146eb`** (heads-up 2): all nine
  pinned tags, `ui` excepted.
- **CRDs first** on every cluster, from a checkout at `2d146eb` (heads-up 3).
  Then confirm `swiftsnapshots` has `status.guestSpec.primaryIP`.
- **After each cluster's upgrade, record:**
  - the Helm revision;
  - every kubeswift image;
  - the controller's `--metrics-secure` value;
  - the VAPs present. Expected: `kubeswift-launcher-sa-tokenrequest-gate`
    everywhere; `kubeswift-gateway-exec-gate` on dev and sov, not ntx;
  - on dev and sov: that ClusterRole `kubeswift-gateway-console` exists, and that
    `kubeswift-vm-reader` (sov) no longer grants `pods/exec`.
- **Running guests are not disturbed.** Record the launcher pod UID and
  restart count of every running guest before and after:
  - dev: `gpu-cells/innercp` (`10bd28b8-…`);
  - ntx: `capi-udn/ks-udn-cp-54klw` (`a5420720-…`).

  The controller upgrade must not recreate them.

**Phase 2 (dev):**
- **D5.** Run the scripts directly with `--vsclass longhorn-snapshot-vsc`
  (`test/snapshot/snapshot-test.sh --vsclass …`, and the same for
  `test/clonestrategy/clonestrategy-test.sh` if it accepts the flag; if it does
  not, SKIPPED with the reason).
- **D7 is replaced.** Do not run `test/migration/migration-test.sh`: it is an
  offline test with the defects you listed, and they will be fixed separately.
  - Instead, create a live SwiftMigration by hand for a guest of class
    `small-migratable` (`longhorn-migratable`, RWX Block), from worker-1 to
    worker-2 (`spec.mode: live`; see `docs/migration/phase-3a.md` for the spec).
  - **Before:** plant a tmpfs sentinel in the guest (as the round-trip test does)
    and record `/proc/uptime`.
  - **Pass:** phase **`Completed`** (not `Succeeded`); the sentinel is still
    there; uptime kept counting (no reboot); the guest is reachable; the source
    pod is gone; `status.podRef` names the destination pod.
- **D8, cancel mid-transfer.**
  - Setup: a cluster-scoped SwiftGuestClass `val-migratable-16g`, a copy of
    `small-migratable` with 16Gi memory (delete it afterwards). Make memory busy
    in the guest, e.g. fill a tmpfs with random data and keep rewriting it, so
    the transfer lasts long enough to cancel.
  - Start a live migration, and while it is transferring set
    `spec.cancelRequested: true` (phase-3a, "Cancelling a migration").
  - **Pass:** the SwiftMigration ends Cancelled; on the source, the sentinel is
    present, uptime is continuous, and the guest is reachable; the destination
    pod is gone.
  - Also report the phase and phaseDetail sequence you observed, and the
    destination launcher's swiftletd log lines around the cancel.
- **D9, cancel racing completion.** Same setup; set `cancelRequested` as close
  as you can to the source reporting complete.
  - **Pass:** exactly one VM survives: either Cancelled with the source running,
    or Completed with the destination running. The sentinel is present and
    uptime continuous in whichever runs.
  - Try it three times, and report the timing of each attempt.
- **D11.** Take the bridge command verbatim from `consoleBridge()` in
  `internal/gateway/exec_bridge.go` @ `2d146eb`, with the guest's namespace and
  name substituted. The gateway credential on dev is
  `system:serviceaccount:kubeswift-system:kubeswift-gateway`.
- **D12 is split.**
  - The browser half is William's: an OIDC login with and without the Console
    capability. List the exact steps for him in the report.
  - Your half:
    - run the CHANGELOG `jq` role migration as a **dry run** first, printing each
      role that would change and its before/after rules;
    - then apply it on dev;
    - report which roles changed, and that a new role saved in the Access editor
      carries `swiftguests/console` rather than `pods/exec` (check an existing
      editor-created role's rules via kubectl if you can't use the UI).
- **D13 on dev.**
  - Find the ServiceAccount the kube-prometheus-stack Prometheus runs as, in the
    `monitoring` release.
  - Then `helm upgrade … --reuse-values --set controllerManager.metrics.secure=true`
    with `controllerManager.metrics.readers` set to that SA.
  - Check:
    - plain HTTP fails;
    - an unbound SA token gets 403;
    - a bound one gets 200 with `kubeswift_` series;
    - the Prometheus target `kubeswift-controller-manager` is `up` (via its
      `/api/v1/targets`).
  - Leave dev in this state (it is the new default); report the final values.

**Phase 3:**
- **ntx:**
  - The default class is `longhorn-r1`, and worker-1's Longhorn has an attach
    problem that predates everything here (heads-up 4).
  - If N1 or N2 fails on a Longhorn attach (`FailedMount`, volume `attaching`),
    mark it **BLOCKED-ENV** with the evidence. Retry once with the guest pinned
    to worker-2 (`spec.nodeName`).
  - N3 uses the baseline above: same launcher UID, 0 restarts.
- **sov** (no KVM, no webhook):
  - S1 is controller Ready; the VAPs as expected; the sov-side console grant and
    exec gate present; `kubeswift-vm-reader` without `pods/exec`.
  - Its webhook is off, so skip the `--dry-run=server` check and say so.

## Go-ahead, re-issued: candidate `d232581`

This section supersedes the candidate above. Phase 1 had not started on
`2d146eb`, so nothing is to be undone.

**Candidate: main @ `d232581`.** It is `2d146eb` plus #662, the fix to
`test/migration/migration-test.sh`. The product code is identical.
- **Wherever this plan says `2d146eb`, read `d232581`:** the chart
  `0.0.0-dev.d232581`, every image tag `sha-d232581` (all nine, `ui` excepted),
  and the checkout for CRDs, scripts and `exec_bridge.go`.
- **Build check:** the Release Dev run for `d232581` started at about 23:50
  UTC. The "Image published?" check and its 45-minute wait apply as written.
- **Later commits:** a commit on main after `d232581` that touches only docs does
  not change the candidate; stay on `d232581`.

Phase 1: **GO**. Phase 2: **GO** once Phase 1 has succeeded on dev.
Phase 3: **GO** per cluster, once Phase 1 has succeeded on that cluster. Push
`phase1.md`, `phase2.md` and `phase3.md` as each finishes. The stop conditions
and every amendment above still hold, except for D7.

**D7 is restored, run from the fixed script** (this replaces the "D7 is
replaced" amendment).

Setup, from the checkout at `d232581`:
- an ephemeral key: `ssh-keygen -t ed25519 -N '' -f /tmp/val-mig-key`, then
  `export KUBESWIFT_TEST_IDENTITY=/tmp/val-mig-key`;
- `swiftctl`: the script builds `./bin/swiftctl` with Go if it is missing. If
  the host has no Go, set `SWIFTCTL=` to a `swiftctl` built from `d232581`.

The runs, one after the other:
- **D7a, offline:**
  `test/migration/migration-test.sh --mode offline --source <worker-1> --target <worker-2>`,
  using the two KVM workers named in `phase0.md`.
  - **Pass:** "All checks passed". That covers the disk sentinel, the guest on
    the target, and the webhook refusing a migration with `migration.enabled=false`.
- **D7b, live:**
  `test/migration/migration-test.sh --mode live --guest-class small-migratable --source <worker-1> --target <worker-2> --no-cleanup`.
  - **Pass:**
    - "All checks passed" (the disk and tmpfs sentinels, continuous uptime,
      `status.mode` = `live`);
    - then, before cleaning up, the source launcher pod is gone and
      `status.podRef` names the `<guest>-mig-<uid>` pod on the target.
  - **Cleanup:** `kubectl delete ns migration-e2e`. `small-migratable` is the
    cluster's own class, so the script leaves it.
- **Record for each run:**
  - the script's full output;
  - the migration's `phase`/`phaseDetail` sequence;
  - the time to Completed;
  - `kubectl get nodes` before and after. The script must leave no cordon
    behind, and must not lift a cordon it did not set.
- **Script defect vs product failure:** if the script itself misbehaves (as
  opposed to the migration failing), record it as a defect of the script with
  the evidence. Then fall back to the hand-made live migration from the
  earlier D7 amendment, so D7 still gets a verdict.

## Round 2: candidate `7a1a76d`

Round 1 stopped at D9 (`phase2.md`): a completed live migration timed out and
the controller deleted the destination, the only running copy. The root causes
are fixed and merged:

| PR | Fix | Found by |
|---|---|---|
| #663 | The source launcher now waits for its own send's `complete` write; a stale signal from an earlier cancelled or failed send released it early. The controller also takes the destination's `migration-status: running` as the commit point, so it cuts over instead of timing out and deleting the destination. | D9 |
| #664 | The 30 s cancel-ack budget runs from the cancel (`kubeswift.io/migration-cancel-issued-at` on the destination pod), not from the destination pod's creation, so swiftletd gets to stop the receive gracefully. | D8, defect 1 (and defect 2, the 16-minute source hang) |
| #665 | A migrated launcher reports GuestRunning to its guest (`KUBESWIFT_GUEST_NAME`), not to `<guest>-mig-<uid>`. | D9, `report_failed … not found` |
| #666 | SwiftImage import and SwiftKernel pull fail only when their Job gives up, not on its first failed pod. | N1, the image `Failed` while its Job still retried |

**Candidate: main @ `7a1a76d`.** Everything in this plan still holds with
`7a1a76d` in place of `d232581`: the chart `0.0.0-dev.7a1a76d`, all nine image
tags `sha-7a1a76d` (`ui` excepted), the checkout, CRDs first, the stop
conditions, and the Phase 0 amendments.
- **Build check:** Release Dev run 751 for `7a1a76d` started at 06:58 UTC,
  alongside three runs for the intermediate commits. Apply the same check and
  45-minute wait as before.
- **Reports:** write `phase1-r2.md`, `phase2-r2.md` and `phase3-r2.md`. Leave
  the round-1 reports as they are.

### Before Phase 1: dev housekeeping
- **`val-d8`:** keep it for inspection, but free its node: set
  `val-d8/mig16b` to `spec.runPolicy: Stopped` and wait for its launcher pod to
  go. Leave `mig16`, the SwiftMigrations and the namespace as they are.
- **`val-d2` and `val-d3`:** they stay stuck in Terminating (a known,
  pre-existing finalizer defect). Don't touch them.
- **ntx leftovers** from round 1: leave them.

### Phase 1 r2: GO
Phase 1 as before, on all three clusters, with `7a1a76d`. Record the same items
as round 1, including the running-guest baselines: dev `gpu-cells/innercp`
and ntx `capi-udn/ks-udn-cp-54klw`, same UID and 0 restarts.

### Phase 2 r2 (dev): GO once Phase 1 r2 has succeeded on dev

The launcher-side fixes (#663's swiftletd half and #665) reach only launchers
created on the new image. **Every guest used below must be created after the
upgrade.** Before migrating it, check that its launcher runs
`swiftletd:sha-7a1a76d`. Use a new namespace, `val-r2`.

| # | Scenario | Pass |
|---|---|---|
| R1 | D1 boot smoke, `disk-boot` only | PASS |
| R2 | D2, `local-roundtrip-test.sh` | PASS, as in round 1 (the address is kept; the restore takes more than ~5 s) |
| R3 | D7a and D7b, the migration script, as in round 1 | "All checks passed" for both; the D7b checks as in round 1 |
| R4 | **D8 again, cancel mid-transfer**, on a new `val-migratable-16g` guest with memory being rewritten, as in round 1. Cancel 15 s into "transferring guest state". | See **R4** below |
| R5 | **The D9 failure sequence.** Right after R4, a plain live migration (no cancel) of the same guest back to its first node. | See **R5** below |
| R6 | **D9, cancel racing completion.** Three attempts, cancelling at progress ≥ 95 or on "src migration complete", each on a guest that has already had a cancelled send. | See **R6** below |
| R7 | **A migrated guest reports its stop.** After D7b (run with `--no-cleanup`), stop the migrated guest from inside (`sudo poweroff` over SSH). | See **R7** below |
| R8 | **An import failure only when the Job gives up.** A SwiftImage whose `source.http.url` is `https://kubeswift.invalid/none.img`. | See **R8** below |
| R9 | D13, secure metrics (never ran in round 1), exactly as in the Phase 0 amendments | As amended |

D3–D6 and D10–D12 are not re-run: nothing merged touches them, and the
round-1 results stand. D12's browser half is still William's.

**R4 passes if all of these hold:**
- The SwiftMigration ends `Cancelled`.
- Its events show `CancelIssued`, then the graceful path: the final message
  is "destination pod deleted after swiftletd cancel ack". There is no
  `CancelAckTimeout` within 30 s of the cancel.
- The destination launcher's log shows the cancel action dispatched, before any
  SIGTERM.
- On the source:
  - the sentinel is present, uptime is continuous, and SSH works;
  - its `migration-status` turns to `failed` within about a minute of the
    cancel (round 1 took about 16 minutes);
  - `ss -tn` in the source launcher shows no ESTABLISHED connection to the
    old destination's port 6789 after that.

  Record the timings.

**R5 passes if all of these hold:**
- The migration reaches `Completed`.
- The sentinel and uptime survive.
- The source launcher's log has `w23_terminal_write_signal_fired id=<this
  send> completed=true` before `w23_terminal_write_signal_received`.
- The source pod ends with `migration-status: complete` for this send.
- No `SourceCompleteMissing` event, which should be rare. Report it if it
  appears.

This is the exact sequence that lost the guest in round 1.

**R6 passes if, in every attempt:**
- Exactly one VM survives: either Cancelled with the source running, or
  Completed with the destination running.
- No SwiftMigration ends `Failed` after the destination reported `running`.
- The sentinel and uptime are intact in the survivor.

Report each attempt's timing, its phase and phaseDetail sequence, and any
`SourceCompleteMissing` or `CancelAckTimeout` event.

**R7 passes if all of these hold:**
- The destination launcher's container env has `KUBESWIFT_GUEST_NAME=<guest>`.
- After the poweroff, the SwiftGuest shows `GuestRunning=False` with reason
  `VmStopped`.
- The launcher log has no `report_failed … not found`.

**R8 passes if all of these hold:**
- While the import Job retries (`status.failed` ≥ 1, no `Failed` condition),
  the SwiftImage stays `Importing`.
- It turns `Failed` only once the Job reports `Failed`
  (`BackoffLimitExceeded`), with that message.
- Report the time between the first failed pod and the image's `Failed`. It
  takes about 10 minutes, so run it in parallel with the rest.

**Stop condition.** If a guest is ever left without a running VM (lost,
killed, or stuck with no launcher), stop Phase 2 at once. Leave everything in
place and report.

### Phase 3 r2: GO per cluster, once Phase 1 r2 has succeeded on it
- **ntx:**
  - N3 as before: same launcher UID, 0 restarts, no status churn.
  - N1 and N2 are not re-run: the fixes do not change them, and a worker-1
    Longhorn problem would block them anyway.
- **sov:** S1 as before.

## Round 3: candidate `f260277`

Round 2 (`phase2-r2.md`) lost no guest and passed its criteria, but found
defects in live-migration cancel and cutover. They are fixed and merged:

| PR | Fix | Found by |
|---|---|---|
| #667 | A cancelled or failed live migration takes its send (`kubeswift.io/migration-action*` naming `<mig>:send:*`) off the source pod, so the source launcher cannot run it later. StopAndCopy says "transferring guest state" only once the source launcher has taken this send up. While the launcher still runs an earlier send, the phaseDetail is "waiting for the source launcher to finish a previous send". Progress is read only from this send's estimate. | R6, defect B and the stale 95 |
| #668 | Once the destination reports `running`, the controller waits up to 30 s for a live source's own `complete` before cutting over (a new condition, `DestinationRunning`). This brings back `observedTransferDuration`, and makes `SourceCompleteMissing` the rare case again. A source pod that is gone, finished or being deleted is not waited for, and the migration stays committed through the wait. | R3/D7b (`observedTransferDuration` empty); R5 and R6-t3 (`SourceCompleteMissing`) |
| #669 | swiftletd fails a send about 20 s after its connection to the destination closes while the source guest still runs (or if the connection never opens within 60 s), not at the 600 s deadline. The progress estimate now carries its send's id (`kubeswift.io/migration-progress-estimate-id`). | R4 (9 m 46 s); R6, defect A |

**Candidate: main @ `f260277`.** Everything in round 2 holds with `f260277`
in place of `7a1a76d`: the chart `0.0.0-dev.f260277`, all nine image tags
`sha-f260277` (`ui` excepted), the checkout, CRDs first, the stop conditions,
and the Phase 0 amendments.
- **Build check:** Release Dev run 754 for `f260277` started at 10:50 UTC,
  alongside runs 752 and 753 for the intermediate commits. Apply the same check
  and 45-minute wait as before.
- **Reports:** write `phase1-r3.md`, `phase2-r3.md` and `phase3-r3.md`. Leave
  the earlier reports as they are.

### Before Phase 1: dev housekeeping
- **`val-r2`:** delete the namespace. Its guest `r4g` runs the round-2
  launcher, so it cannot test #669, and round 2's evidence is in
  `phase2-r2.md`. It holds no snapshots, so it should finish deleting. If it
  sticks in Terminating, leave it and report it.
- **`val-d8`:** delete the namespace too (round-1 evidence, already reported).
  Keep the cluster-scoped class `val-migratable-16g`: round 3 uses it.
- **`val-d2` and `val-d3`:** still don't touch them (they wait on William).
- **Secure metrics:** dev's stay on. The upgrade reuses values.

### Phase 1 r3: GO
Phase 1 as before, on all three clusters, with `f260277`. Record the same items
as round 2, including:
- the running-guest baselines: dev `gpu-cells/innercp` and ntx
  `capi-udn/ks-udn-cp-54klw`, same UID and 0 restarts;
- on dev, that the controller still runs with `--metrics-secure=true`.

### Phase 2 r3 (dev): GO once Phase 1 r3 has succeeded on dev

#669 is in swiftletd, so it reaches only launchers created on the new image.
**Every guest used below must be created after the upgrade.** Before migrating
it, check that its launcher runs `swiftletd:sha-f260277`. Use a new namespace,
`val-r3`.

For every live SwiftMigration below, record:
- its phase and phaseDetail sequence, with timestamps;
- its events;
- `status.transferProgress` over time;
- once it ends: `observedDowntime`, `observedTransferDuration` and the
  conditions.

| # | Scenario | Pass |
|---|---|---|
| T1 | R1 boot smoke, `disk-boot` only | PASS |
| T2 | R3 again: D7a and D7b, the migration script | See **T2** below |
| T3 | R4 again: cancel mid-transfer | See **T3** below |
| T4 | A migration right after a cancel (defects A and B, and R5's sequence) | See **T4** below |
| T5 | R6 again: cancel racing completion, three attempts back to back | See **T5** below |

R2, R7, R8 and R9 are not re-run: nothing merged touches them, and round 2's
results stand. D12's browser half is still William's.

**T2 passes if all of these hold:**
- "All checks passed" for D7a and D7b.
- D7b's SwiftMigration has `observedTransferDuration` and `observedDowntime`
  set. `observedTransferDuration` was empty in round 2 and 19.8 s in round 1.
- There is no `SourceCompleteMissing` event.
- Record the `DestinationRunning` condition, if present, and the gap between
  it and the source's `complete`.
- The phaseDetail "src migration complete; preparing cutover" is defined but
  never set. That predates these fixes, so its absence is not a failure: the
  phase goes from "transferring guest state" to "cutover: completing", as in
  round 2.

**T3: setup.** As in R4:
- a new guest `val-r3/t3g` of class `val-migratable-16g`;
- a 10 GiB tmpfs of random data, rewritten in a loop;
- a sentinel;
- a live migration to another node, cancelled 15 s into "transferring guest
  state".

**T3 passes if all of these hold:**
- The SwiftMigration ends `Cancelled` on the graceful path: "destination pod
  deleted after swiftletd cancel ack", and no `CancelAckTimeout`.
- Right after `Cancelled`, the source pod has no `kubeswift.io/migration-action`,
  `-action-id` or `-action-args` naming this migration.
- **The source's `migration-status` turns `failed` within 60 s of the
  cancel.** The expected time is about 20 s after Cloud Hypervisor's
  connection reset.
- The source launcher's log has `migration_send_failed id=<this send>` with a
  detail naming the closed migration connection, not "past the migration
  deadline".
- On the source guest, the sentinel is present, uptime is continuous, and SSH
  works.
- Record the cancel time, the time of Cloud Hypervisor's send error (the reset),
  and the time of `migration_send_failed`.

**T4: steps.** On the same guest, right after T3:
1. Start `t4-a`, a live migration to T3's target node, and cancel it 15 s into
   "transferring guest state".
2. Once `t4-a` is `Cancelled`, stop the memory rewriter so the next migration
   can converge. Within 5 s of the `Cancelled`, create `t4-b` to the same node.
3. If `t4-b` shows "waiting for the source launcher to finish a previous
   send", cancel it while it still shows that, then create `t4-c` to the same
   node and let it complete. If `t4-b` never shows it, record that (the source
   was free before `t4-b` needed it) and let `t4-b` complete.

**T4 passes if all of these hold:**
- No migration shows "transferring guest state" before the source's log has
  `action_accept … id=<that migration>:send:<n>`.
- The completing migration (`t4-b` or `t4-c`):
  - its first `transferProgress` is a real, low value, not the previous
    send's final estimate;
  - while it transfers, the source pod's
    `kubeswift.io/migration-progress-estimate-id` names its send;
  - the source accepts its send within 60 s of `t4-a`'s cancel, not after
    about 10 minutes as in round 2.
- If `t4-b` was cancelled while waiting:
  - its action annotations are gone from the source pod right after its
    `Cancelled`;
  - for the rest of Phase 2, the source log never shows `action_accept` or
    `dispatch_migration_send` for `t4-b`.
- The completing migration meets R5's criteria:
  - it reaches `Completed`;
  - the source launcher logs `w23_terminal_write_signal_fired id=<its send>
    completed=true` before `w23_terminal_write_signal_received`;
  - the source pod ends with `migration-status: complete` for its send;
  - there is no `SourceCompleteMissing` event;
  - `observedTransferDuration` is set.
- In the survivor, the sentinel is present and uptime is continuous.

**T5: setup.**
- The T4 guest, with its memory rewriter stopped and a static 10 GiB of random
  tmpfs data. A transfer takes about 155 s.
- Its source launcher is T4's destination pod, created on the new image.
- Run the attempts back to back. Start each one as soon as the previous one
  has ended and the source's `migration-status` no longer reads `sending`.
  This should take at most 60 s after a cancel; round 2 needed 600 s.

The three attempts:
- **t1:** cancel once `transferProgress` ≥ 90 (the value is real now).
- **t2:** cancel about 10 s before the expected completion (about 145 s in).
- **t3:** cancel as soon as the `DestinationRunning` condition appears. This
  is the window #668 added: the destination runs and the controller waits for
  the source. Poll it at about 0.2 s; the window is usually a few seconds.
  - If you miss it, cancel at once anyway. Record when you cancelled relative
    to the destination's `migration-status: running` and the source's
    `complete`.

**T5 passes if, in every attempt, all of these hold:**
- Exactly one VM survives: either `Cancelled` with the source running, or
  `Completed` with the destination running.
- No SwiftMigration ends `Failed` after the destination reported `running`.
- In the survivor, the sentinel is present and uptime is continuous.
- A cancel sent after `DestinationRunning` gives `CancelIgnored`, and the
  migration completes.
- Each `Completed` attempt has `observedTransferDuration` set and no
  `SourceCompleteMissing` event.
- After each `Cancelled` attempt, the source's `migration-status` turns
  `failed` within 60 s.
- Report each attempt's timing and the gap before the next attempt.

**Stop condition.** As in round 2: if a guest is ever left without a running VM
(lost, killed, or stuck with no launcher), stop Phase 2 at once. Leave
everything in place and report. If every scenario passes, delete `val-r3`;
otherwise keep it for inspection.

### Phase 3 r3: GO per cluster, once Phase 1 r3 has succeeded on it
- **ntx:** N3 as before: same launcher UID, 0 restarts, no status churn. N1
  and N2 are still not re-run.
- **sov:** S1 as before.

## Round 4: candidate `9604992`

Round 3 (`phase2-r3.md`) passed with no guest lost. Since then, round 3's
findings and the other items open before v0.15.0 were fixed and merged:

| PR | Change | What round 4 checks |
|---|---|---|
| #671 | The committed-cutover wait has its own phaseDetail, `destination running; waiting for the source's report`. The never-set "src migration complete" is gone from the docs. | V5 |
| #673 | A migration whose target fits only once pods already terminating there are gone waits in Validating (phaseDetail `waiting for terminating pods on the target node to release resources`, up to 2 min), instead of failing. | V6 |
| #674 | Warm GPU pools boot `gpu-sandbox` (#659). A sandbox or pool whose SwiftKernel is missing or not Ready gets no pod and reports `KernelNotFound`/`KernelNotReady` (#660). | V9 |
| #675 | Snapshot cleanup pods run in the controller's namespace, so a namespace holding a snapshot finishes deleting. The three stuck namespaces should clear after the upgrade. | Phase 1, V8 |
| #676 | SwiftGuest and SwiftSandbox report `status.network.podIP` and `status.network.primaryIPScope`. The columns are `Guest IP` and `Pod IP`, plus `IP Scope` in `-o wide`. | V2 |
| #677 | SwiftKernel pull Jobs are named for the current directory, so every kernel re-pulls once after the upgrade (#658). A kernel-boot guest waits for a pulling kernel instead of failing. | Phase 1, V10 |
| #678 | `spec.runPolicy: Stopped` stops a running guest, and waits while a migration, restore or capture owns it. | Phase 1 pre-check, V3 |
| #679 | Test scripts: `migration-test.sh` places its guest on `--source`; smoke runs in `$NAMESPACE` and cleans up only what it created; the `GuestRunning=`/`hypervisor=` warnings are fixed; `identity_of` reads the default-route MAC; clonestrategy's speedup is informational; `gpu-alloc` can pass. | V1, V4, V7 |
| #680 | A live cutover moves a pinned guest's `spec.nodeName` to the target. | V4 |

**Candidate: main @ `9604992`.** Everything in round 3 holds with `9604992` in
place of `f260277`:
- the chart `0.0.0-dev.9604992`;
- all nine image tags `sha-9604992` (`ui` excepted);
- the checkout, CRDs first, the stop conditions, and the Phase 0 amendments.

**CRDs changed:** `swiftguests` and `swiftsandboxes` gained `podIP` and
`primaryIPScope`, and their printer columns changed. Apply the CRDs before the
Helm upgrade, as always.
- **Build check:** Release Dev run for `9604992`. Apply the same check and
  45-minute wait as before.
- **Reports:** write `phase1-r4.md`, `phase2-r4.md` and `phase3-r4.md`.

### Before Phase 1, on EVERY cluster: the runPolicy pre-check (#678)
From this version on, a guest whose spec says `runPolicy: Stopped` but that
still runs is shut down by the controller.

1. Before upgrading a cluster, list those guests:
   ```sh
   kubectl get swiftguests -A -o json | jq -r '.items[]
     | select(.spec.runPolicy == "Stopped" and .status.phase == "Running")
     | "\(.metadata.namespace)/\(.metadata.name)"'
   ```
2. Record the output in `phase1-r4.md`.
3. **If it lists any guest, do not upgrade that cluster.** Report the list
   and wait for William. Upgrading would stop those guests, and on ntx that
   may include CAPI-managed guests.
4. Also record, per cluster:
   - the pod-security labels of the controller namespace:
     `kubectl get ns kubeswift-system --show-labels`. Snapshot cleanup pods now
     run there and need a namespace that admits hostPath pods (#675).
   - `kubectl get swiftkernels -A` and `kubectl get jobs -A -l
     app.kubernetes.io/component=swiftkernel-pull` (expect none with that
     label yet), plus the names of the existing `swiftkernel-pull-*` Jobs.

### Phase 1 r4: GO per cluster, once its pre-check is clean
Phase 1 as before, on all three clusters, with `9604992`. Record the same
items as round 3, and also:
- **Baselines.** Dev `gpu-cells/innercp` and ntx `capi-udn/ks-udn-cp-54klw`:
  same UID, 0 restarts, still `Running`. Record whether either is a
  kernel-boot guest.
- **Kernel re-pull (#677).** Within about 5 minutes of the upgrade:
  - every SwiftKernel goes `Pulling`, then back to `Ready`;
  - new Jobs `swiftkernel-pull-<kernel>-<node>-<8 hex>` appear with label
    `kubeswift.io/swiftkernel=<kernel>`;
  - the old unhashed Jobs are deleted;
  - no running guest changes phase meanwhile.

  Record the timeline.
- **Stuck namespaces (#675).** `val-d2` and `val-d3` on dev, and `val-n2r` on
  ntx, should finish deleting within a few minutes of the upgrade.
  - Record when each namespace goes.
  - Record the cleanup pods seen in `kubeswift-system`: label
    `snapshot.kubeswift.io/role=hostpath-cleanup`, name
    `swift-snap-cleanup-<snapshot>-<hash>`.
  - If one is still `Terminating` after 10 minutes, collect:
    - the controller log lines naming it;
    - `kubectl get pods -n kubeswift-system -l snapshot.kubeswift.io/role=hostpath-cleanup -o wide`;
    - the events in `kubeswift-system`.
- On dev, `--metrics-secure=true` survives the upgrade.

### Phase 2 r4 (dev): GO once Phase 1 r4 has succeeded on dev

Use new namespaces `val-r4-*`. Every guest used must be created after the
upgrade: check that its launcher runs `swiftletd:sha-9604992`.

| # | Scenario | Pass |
|---|---|---|
| V1 | **Smoke, in a namespace:** `NAMESPACE=val-r4-smoke make smoke-test` (all scenarios, `gpu-alloc` included), then `make smoke-test-cleanup` with the same `NAMESPACE`. | Every scenario PASS or an explained SKIP. There is no `WARN: GuestRunning=`/`hypervisor=`, cleanup removes only the run's objects, and the shared `default/ubuntu-noble` (if present) is untouched. |
| V2 | **Guest and pod IPs (#676).** List `kubectl get swiftguest -A` and `-o wide` with at least two nat guests running (the smoke guests will do). | The columns read `Guest IP`/`Pod IP` (and `IP Scope` in wide). Two nat guests that share a Guest IP have different Pod IPs, and `status.network.primaryIPScope` is `Pod`. `status.network.podIP` equals the launcher pod's IP. |
| V3 | **runPolicy Stopped (#678).** (a) Patch a running test guest to `runPolicy: Stopped` with kubectl. (b) Start a live migration of another guest and, while it is in StopAndCopy, patch that guest to `Stopped`. | (a) Within about 30 s the launcher is deleted, a `Stopping` event is recorded, the guest powers off (no hard kill in the launcher log) and ends `Stopped`. (b) A `StopDeferred` event names the SwiftMigration, the migration completes, and only then does the guest stop. |
| V4 | **Migration script and pinned repin (#679, #680).** (a) `migration-test.sh --mode offline` and `--mode live` with `--source`/`--target` naming two workers, on the 3-node cluster. (b) Live-migrate a new guest created with `spec.nodeName: <source>`; after it completes, delete its launcher pod. | (a) "All checks passed" for both, with the guest landing on `--source` both times, and D7b's `observedTransferDuration` set. (b) After the migration `spec.nodeName` names the target, and the relaunched pod runs on the target. |
| V5 | **The DestinationRunning wait (#671).** During a live migration, watch the phaseDetail at about 0.2 s. | Between "transferring guest state" and "cutover: completing" the phaseDetail reads `destination running; waiting for the source's report` whenever `DestinationRunning` appears. There is no `SourceCompleteMissing`. |
| V6 | **A migration right after a cancel, to a busy node (#673).** Repeat round 3's T4 first run: cancel a migration to a node with room for only one destination (worker-2, where `innercp` runs), then create the next one to the same node within 1 s. | The new migration does not fail. It shows `waiting for terminating pods on the target node to release resources` with `Compatible=Unknown` (reason `AwaitingTerminatingPods`), then proceeds once the old destination pod is gone, and completes. Record the wait. |
| V7 | **clonestrategy on Longhorn (#679):** `clonestrategy-test.sh --vsclass longhorn-snapshot-vsc` in `val-r4-cs`. | PASS. The speedup is printed as informational, and the new clone-seed and `dataSource` checks pass. |
| V8 | **Snapshot namespace deletion (#675).** In `val-r4-snap`, run a local memory snapshot of a running guest (`local-roundtrip-test.sh --no-cleanup`), then `kubectl delete ns val-r4-snap`. | The namespace is gone within about 2 minutes. The cleanup pod ran in `kubeswift-system` and was deleted after it succeeded. The snapshot directory `/var/lib/kubeswift/snapshots/val-r4-snap_<name>` is gone from the node. |
| V9 | **Sandbox kernel checks (#674).** (a) Create a SwiftSandbox in a new namespace `val-r4-nok` that has no SwiftKernel named `sandbox`. (b) If a GPU pool can be created without disturbing `innercp` (a free GPU), create a warm pool with `gpuProfileRef` and no `kernelProfileRef`; otherwise SKIP with the reason. | (a) The sandbox stays `Pending` with `Resolved=False`, reason `KernelNotFound`, and no launcher pod. Creating the SwiftKernel there lets it start once the kernel is Ready. (b) The slot pod mounts `/var/lib/kubeswift/kernels/<ns>/gpu-sandbox`. |
| V10 | **Kernel-boot after the re-pull (#677).** The smoke `kernel-boot` scenario from V1, plus one SwiftSandbox. | Both boot. No `Cannot open initramfs file` appears in any launcher log. |
| V11 | **Regression: R2 in-place restore, T3 cancel mid-transfer, T5 cancel racing completion** (three attempts back to back), exactly as in round 3. | As in round 3. |

R7, R8 and R9 are not re-run; nothing merged touches them. D12's browser half
is still William's.

**Stop condition.** As before: if a guest is ever left without a running VM,
stop Phase 2 at once, leave everything in place, and report. If every scenario
passes, delete the `val-r4-*` namespaces. Their deletion is itself a check of
#675: report any namespace that does not finish deleting.

### Phase 3 r4: GO per cluster, once Phase 1 r4 has succeeded on it
- **ntx:**
  - N3 as before;
  - `val-n2r` finishes deleting (Phase 1);
  - the ntx SwiftKernels re-pull and return to `Ready`.
- **sov:** S1 as before.

## Round 5: preparation (GO now), then a focused re-run (candidate to follow)

Round 4 (`phase2-r4.md`) passed everything it could run. Three scenarios were
blocked by the lab's storage: V4a live, T3 and T5. Four findings are being
fixed. **v0.15.0 is not tagged until round 5 passes.**

The round 5 candidate (the fixes for findings 1, 2, 4 and 5) follows in a
separate go-ahead, with its SHA. Start with the preparation below now: it
does not depend on the candidate.

### Preparation: free Longhorn space on dev's cp-1 (GO now)

- **Target:** cp-1's Longhorn disk must be comfortably above
  `storage-minimal-available-percentage=25`: at least 35% free (about 70 GiB of
  195 GiB). New volumes then get all 3 replicas and can be live-migrated.
  - T5 needs a 16 GiB guest, T3 the same class, and V4a a small guest, each
    with a healthy volume.
- **William has authorised you to delete guests and images to get there.**
  Work in this order and stop as soon as the target is met.
  1. **Round 4 leftovers.** Delete the namespaces `val-r4-mig`, `val-r4-d7b2`,
     `val-r4-rt`, `val-r4-nok` and `val-r4-smoke`, with their guests,
     SwiftMigrations, snapshots, images and PVCs.
     - Save anything you still need from them first.
     - Their deletion also re-checks #675: report any namespace that does not
       finish deleting.
  2. **Other validation leftovers.** Any remaining `val-*` namespace, and the
     `val-migratable-16g` class only if nothing uses it (round 5 recreates what
     it needs).
  3. **Unused lab images.** SwiftImages that no SwiftGuest, SwiftGuestPool or
     SwiftSandbox references, in any namespace except those listed under
     "never" below.
     - Check with `kubectl get swiftguests,swiftguestpools,swiftsandboxes -A -o yaml`
       before deleting each one.
     - `default/ubuntu-noble` may be deleted if needed; the test scripts
       re-import it.
     - Record each image you delete, its namespace and its size.
  4. **Orphaned Longhorn volumes and snapshots** that no PVC uses (detached,
     no workload). Delete these only with Longhorn's own UI or CLI state
     showing them unused, and record each one.
- **Never delete or modify:**
  - `gpu-cells/innercp` and its image, PVCs and namespace;
  - anything in `field-testing`, `capi-udn` or `kube-system`;
  - the kubeswift install in `kubeswift-system`;
  - any guest you did not create for validation, unless William names it.
- **Do not lower `storage-minimal-available-percentage` or change other
  Longhorn settings.** Freeing space is the fix.
- **Report** in `phase0-r5.md`:
  - cp-1's free space before and after;
  - everything deleted;
  - whether a fresh test volume now gets 3 healthy replicas: create a 1 GiB
    `longhorn-migratable` PVC, check its robustness is `healthy`, and delete
    it.
- **If you cannot reach 35% within those limits,** stop and report what is
  left on cp-1's disk (the largest volumes and their owners). William decides
  from there.

## Round 5 go-ahead: candidate `08d0165`

**Where round 4 ended.** Round 4 finished at 22:44 with T3 and T5 passing,
once William authorised freeing space. Two items are still open:
- V4a live, blocked by storage;
- V9b, skipped because there was no free GPU.

**Merged since `9604992`,** fixing round 4's findings 1, 2, 4 and 5 (finding 3
is IPAM, #672; finding 6 is not changed):

| PR | Change | What round 5 checks |
|---|---|---|
| #681 | A guest whose SwiftImage (root or data disk) is still importing waits. It stays `Pending`, or keeps its phase if it is past `Pending`, with `Resolved=False` "SwiftImage not Ready". It goes `Failed` only if the image is `Failed`. (Finding 2.) | R5-B |
| #682 | `DstNeverReady` says why. After `within 1m0s budget:` it gives the destination pod's state and its two latest Warning events, such as `FailedAttachVolume`, and it records a `DestinationPodNeverReady` event. An offline `Timeout` while the guest's pod is not Ready gets the same cause. The controller ClusterRole gains `list` on `events`. (Finding 1.) | Phase 1, R5-A |
| #683 | A sandbox's phase never goes back (finding 4). A failed or succeeded warm slot is deleted, with a `SlotEnded` Warning event on its pool, and it frees its GPU (finding 5). A checked-out sandbox whose slot pod ends fails with `SlotEnded`; one whose slot pod is gone fails with `SlotLost`, naming the pod. | Phase 1 (`ft-gpu-pool`), R5-C, V9b |
| #684 | `migration-test.sh --mode live` waits for the guest's Longhorn volumes to be `healthy` before it migrates, for up to `LONGHORN_HEALTHY_WAIT_MIN` minutes (default 15). | V4a live |

**Candidate: main @ `08d0165`.** Everything in round 4 holds, with `08d0165` in
place of `9604992`:
- the chart `0.0.0-dev.08d0165`;
- all nine image tags `sha-08d0165` (`ui` excepted);
- the checkout, CRDs first, the stop conditions, and the Phase 0 amendments.

**No CRD changed since `9604992`.** Apply them first anyway, as always.
- **RBAC changed:** the controller ClusterRole adds `list` on `events`. The
  Helm upgrade applies it.
- **Swiftletd:** no Rust changed since `9604992`, so launchers still on
  `sha-9604992` interoperate with `sha-08d0165` ones.
- **Build check:** use the Release Dev run for `08d0165`, with the same check and
  45-minute wait as before.
- **Reports:**
  - `phase0-r5.md` for the preparation;
  - `phase1-r5.md`, `phase2-r5.md` and `phase3-r5.md` for the phases.

### Order on dev (after `phase0-r5.md`)

The preparation stopped at 30.9% free, with cp-1 `Schedulable=True`, above
Longhorn's 25%. Step 4, deleting the 7 orphaned replicas (58.7 GiB), waits
for William's explicit OK.
1. **Phase 1 r5: GO now.** It does not need the extra space.
2. **Step 4, once William confirms it:**
   - delete the 7 cp-1 Orphan objects listed in `phase0-r5.md`;
   - re-measure cp-1;
   - run the 1 GiB `longhorn-migratable` fresh-volume check;
   - update `phase0-r5.md`.

   Leave the worker-2 orphans alone unless William includes them.
3. **Phase 2 r5: run V4a live first,** while there is the most room.
   - **If step 4 is not confirmed yet,** run V4a live at the current 30.9%,
     provided the fresh-volume check passes there: a new 1 GiB
     `longhorn-migratable` PVC gets 3 `healthy` replicas.
   - **Then the rest,** in the table's order.
   - **Keep cp-1 above 25%:** delete each scenario's guests and images as soon
     as it has passed and its evidence is recorded. Keep a failed scenario's
     objects.
   - **If cp-1 reads `Schedulable=False` again,** stop creating volumes and
     report. Do not delete anything beyond the preparation's list.

### Before Phase 1, on EVERY cluster: the runPolicy pre-check
Run the same pre-check as round 4:
- List the guests with `runPolicy: Stopped` and phase `Running`.
- Record the list in `phase1-r5.md`.
- If it lists any guest, do not upgrade that cluster.

(The clusters already run #678, so the list should be empty.)

### Phase 1 r5: GO per cluster, once its pre-check is clean
Phase 1 as before, on all three clusters, with `08d0165`. Record the same items
as round 4, and also:
- **Baselines.** Dev `gpu-cells/innercp` and ntx `capi-udn/ks-udn-cp-54klw`
  keep the same UID, 0 restarts, and stay `Running`.
- **Events RBAC (#682).**
  `kubectl auth can-i list events --as=system:serviceaccount:kubeswift-system:controller-manager`
  prints `yes`. Use the controller's namespace if it is not
  `kubeswift-system`.
- **`field-testing/ft-gpu-pool` on dev (#683).** Only observe it: do not
  change the pool.
  - Within a few minutes of the upgrade, the `Failed` slot
    `ft-gpu-pool-slot-dfjtk` should be deleted.
  - The pool should get a `SlotEnded` Warning event that names the slot and
    its failure message. Record the event text.
  - The GPU should no longer be allocated to that slot. Check it the same way
    round 4 did.
  - The pool may warm a replacement (its image resolves). If so, record
    whether the replacement runs.
  - If the replacement ends too, count the `SlotEnded` events over 10
    minutes. A delete-and-recreate loop is a finding.
- On dev, `--metrics-secure=true` survives the upgrade.

### Phase 2 r5 (dev): GO once Phase 1 r5 has succeeded on dev

- Use new namespaces `val-r5-*`.
- Every guest used must be created after the upgrade, with a launcher running
  `swiftletd:sha-08d0165`.

| # | Scenario | Pass |
|---|---|---|
| R5-A | **`DstNeverReady` names its cause (#682).** A volume that is kept degraded on purpose: (1) Create a temporary StorageClass `val-r5-degraded`, a copy of `longhorn-migratable` with `numberOfReplicas: "4"`. With 3 nodes and hard anti-affinity, the fourth replica never schedules, so every volume in it stays `degraded`. This is a new StorageClass, not a change to a Longhorn setting. (2) Create a SwiftGuestClass `val-r5-degraded` using it (RWX Block, 2 CPU, 2Gi, 10Gi root) and a guest in `val-r5-dnr`. Once the guest is `Running`, confirm its volume reads `degraded`: `kubectl -n longhorn-system get volumes.longhorn.io <PV volumeHandle>`. (3) Live-migrate it: `swiftctl -n val-r5-dnr migrate <guest> --to <other node> --preferred-mode live --allow-ip-change --name r5a-mig`. (4) Delete the guest, the class and the StorageClass afterwards. If the lab's permission check refuses the StorageClass, ask William. If he declines, SKIP with the reason; the unit tests cover the message. | The SwiftMigration fails `DstNeverReady` after about 60 s. Its `status.failureMessage` goes on after `within 1m0s budget:` to name the cause, including a `Warning FailedAttachVolume: …` event. A `DestinationPodNeverReady` Warning event is on the SwiftMigration. The guest is still `Running` on its source, with the same pod UID. No destination pod is left. Paste the full message. |
| R5-B | **A guest waits for its importing image (#681).** (a) In `val-r5-img`, apply a SwiftImage for the Noble cloud image and, in the same `kubectl apply`, a SwiftGuest that uses it. Watch `status.phase` and the `Resolved` condition every 2 s until the guest is `Running`. (b) Apply a SwiftImage whose URL returns 404, with a guest that uses it. | (a) While the image imports, the guest is `Pending` with `Resolved=False` "SwiftImage not Ready". It is never `Failed` and gets no launcher pod. The controller logs `waiting to resolve`. The guest boots once the image is `Ready`. Record how long it waited. (b) The guest stays `Pending` while the import Job retries, which takes about 10 minutes with the Job's default 6 retries. Once the image reads `Failed`, the guest goes `Failed` with `Resolved=False`. Record both times. |
| R5-C | **Sandbox phase and ended slots (#683).** (a) Run three one-shot SwiftSandboxes, two exiting 0 and one exiting 3, and poll `status.phase` every 0.5 s. (b) Create a non-GPU SwiftSandboxPool with one slot in `val-r5-sbx`. Check out a SwiftSandbox from it (`spec.poolRef`, command `sleep 600`). Once it is `Running`, force-delete its claimed slot pod: `kubectl delete pod <slot> --grace-period=0 --force`. | (a) The phase sequence never goes back: no `Materializing` after `Running`. The exit-0 sandboxes end `Completed`, and the exit-3 one ends `Failed` with exit code 3. (b) Within about 10 s the sandbox is `Failed` with reason `SlotLost`, and its message names the slot pod. The pool warms a new slot. |
| V4a live | **Run first.** Run `migration-test.sh --mode live --source <worker> --target <worker> --storage-class longhorn-migratable` from the candidate checkout. | The script logs `Longhorn volume … is healthy` before it migrates, then "All checks passed". The guest starts on `--source`, and D7b's `observedTransferDuration` is set. While it runs, watch the phaseDetail as V5 did: `destination running; waiting for the source's report` appears whenever `DestinationRunning` does. Record how long the volume wait took. |
| V9b | **Only if Phase 1 showed the dev GPU freed** and `ft-gpu-pool` is not holding it with a replacement. Create round 4's V9b GPU pool (`gpuProfileRef`, no `kernelProfileRef`) in `val-r5-gpu`, check the pass, then delete the pool at once so the GPU is free again. Otherwise SKIP with the reason. | As in round 4: the slot pod mounts `/var/lib/kubeswift/kernels/<ns>/gpu-sandbox`. After the pool is deleted, the GPU is no longer allocated. |
| V1 | **Regression: smoke.** `NAMESPACE=val-r5-smoke make smoke-test` (all scenarios), then `make smoke-test-cleanup` with the same `NAMESPACE`. | As in round 4. |
| V11-R2 | **Regression:** the R2 in-place restore in `val-r5-rt`, exactly as in round 4. | As in round 4. |

R7, R8, R9, V2 to V8, V10, T3 and T5 are not re-run: nothing merged since
round 4 touches them. D12's browser half is still William's.

**Stop condition:** as before. If a guest is ever left without a running VM,
stop Phase 2 at once, leave everything in place, and report.
- **If every scenario passes,** delete the `val-r5-*` namespaces and any
  `val-r4-*` still left, and report any that does not finish deleting.

### Phase 3 r5: GO per cluster, once Phase 1 r5 has succeeded on it
- **ntx:** N3 as before, plus the events RBAC check.
- **sov:** S1 as before, plus the events RBAC check.

**v0.15.0 is tagged only after round 5 passes and William gives the word.**
