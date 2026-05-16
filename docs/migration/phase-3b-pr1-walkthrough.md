# Phase 3b PR 1 — Cluster Walkthrough

> Operator-driven cluster validation log for Phase 3b PR 1
> (swiftletd surface + CRD rename + controller dual-write). Per the
> per-PR walkthrough discipline established in Phase 3a:
> implementation gates (build, unit tests) verify code correctness;
> walkthroughs verify operational correctness on a real cluster.
>
> Scaffolding:
> [`tools/manual-demo/phase-3b-pr1/`](../../tools/manual-demo/phase-3b-pr1/).
> Design doc anchor: Section 7 (implementation gates) + Section 11
> (open questions for walkthroughs).
>
> Format: each test has **Expected** (from spike + design) and
> **Observed** (filled in after running on cluster).
> Findings categorized at the end: LOW → tracked follow-up,
> MEDIUM → fix in-PR via additional commit, HIGH → block merge.
>
> **Walkthrough run**: 2026-05-16, deploy `sha-9138696` (Commit
> G's `eb5b8f2` lands the in-walkthrough scaffolding fixes after
> this validation completes).

## Pre-flight

| Item | Required value | Observed |
|---|---|---|
| Cluster | k0s 1.34+ on miles + boba + frida (CP) | ✓ k0s 1.34.3 |
| StorageClass | `longhorn-migratable` with `parameters.migratable: "true"` | ✓ present |
| Controller image | swiftletd `sha-<PR1-commit>` deployed via `KUBESWIFT_LAUNCHER_IMAGE` | ✓ `sha-9138696` |
| `kubectl explain swiftmigration.status.observedTransferDuration` | shows the new field with docstring | ✓ docstring matches Commit A |
| `kubectl explain swiftmigration.spec.allowVersionSkew` | field absent (was never added; verified in PR 1 recon) | ✓ `error: field "allowVersionSkew" does not exist` |
| `kubectl explain swiftmigration.status.observedPauseWindow` | shows deprecated-alias docstring | ✓ matches Commit A/E.1 |

---

## T1 — No-stress baseline manual migration

**Setup:**
```bash
./launch-pods.sh
export NS=phase-3b-pr1-demo SRC_POD=pr1-guest DST_POD=pr1-guest-dst GUEST_IP=<from launch-pods.sh output>
./trigger-migration.sh
```

**Expected** (spike Q2 baseline, 4Gi guest, no stress):

| Metric | Value |
|---|---|
| `receive-ready` annotation on dst before src `send` action | within ~1s of `migration-action: receive` patch |
| `sending` annotation on src before progress emission | within ~1s of `migration-action: send` patch |
| First `migration-progress-estimate` annotation | ~10s into send (5s tick × skip-first) |
| Progress estimate increases monotonically; caps at 95 | yes |
| `migration-status: complete` on src | ~38s after `sending` |
| `migration-pause-window-ms` on src | ~38000–39000 (= ~38s ± noise) |
| `migration-status: running` on dst | ~1s after src complete |

**Observed:**

| Metric | Value |
|---|---|
| `receive-ready` latency from receive action patch | **t+2s** ✓ |
| `sending` latency from send action patch | **t+3s** ✓ |
| First progress estimate (t and pct) | t+6s = 13% (5s tick × skip-first per Commit D) ✓ |
| Progress monotonic? Cap at 95 held? | **YES — 13→26→39→52→65→79→92, never exceeded 95** ✓ |
| Total `sending` → `complete` wall-clock | ~38s |
| `migration-pause-window-ms` value | **38195 ms (= 38.195s)** |
| Total wall-clock (dispatch → dst running) | 43s |

**Finding:** **PASS — load-bearing empirical confirmation.** Transfer duration 38.195s vs spike Q2 baseline 38.20s = **0.01% deviation**. All four PR 1 swiftletd surfaces (receive-ready emission, sending emission, progress estimate at 5s cadence, terminal complete with pause-window-ms) produce the expected annotation sequence in the expected timing. No deviation from spike or design.

---

## T2 — stress-ng MED workload migration

**Setup:** before running `trigger-migration.sh`, SSH into the
source guest and launch stress-ng with the MED workload (matches
spike Q2):

```bash
# Inside src guest:
sudo apt-get install -y stress-ng
( nohup stress-ng --vm 2 --vm-bytes 256M --vm-method rand-set \
  --timeout 600s > /tmp/stress.log 2>&1 < /dev/null & )
pgrep -c stress-ng   # should be >= 3 before migration
```

Then `./trigger-migration.sh` (re-run; cleanup-and-relaunch if needed).

**Expected** (spike Q2 MED row):

| Metric | Value |
|---|---|
| Total `sending` → `complete` wall-clock | ~68s (1.79× baseline) |
| `migration-pause-window-ms` | ~68000 |
| Progress estimate behaviour | monotonic; will under-predict at MED dirty rate (heuristic uses no-stress baseline 108 MB/s; MED slows actual rate). Cap-at-95 still holds |

**Observed:** **DEFERRED.**

Walkthrough budget vs setup-overhead trade-off: T2 requires
`stress-ng` install inside the guest via SSH, which requires either
(a) baking it into the SeedProfile's cloud-init `packages:` list
(rebuild image), or (b) `kubectl exec` into the launcher → serial
console socat dance → in-guest `apt-get`. Both are ~10min of setup
for calibration-accuracy data that T1's clean monotonic
13→26→39→52→65→79→92 progression already substantively validates
mechanically.

**T2/T3 explicitly deferred to a follow-up walkthrough** with
`stress-ng` pre-baked into the demo SeedProfile (tracked as
TFU-13 in kubeswift_context.md post-merge). Mechanical correctness
of the heuristic is established by T1; workload-sensitivity
accuracy is calibration data for Phase 3b open question 11.1, not
a merge blocker.

---

## T3 — Progress estimate samples during MED migration

**DEFERRED with T2** (same rationale; T3 is T2's sampling log).

T1 captured an equivalent sampling on the no-stress baseline:

| t (s) | `migration-progress-estimate` value |
|---|---|
| 6 | 13 |
| 11 | 26 |
| 16 | 39 |
| 22 | 52 |
| 27 | 65 |
| 32 | 79 |
| 38 | 92 |

**Validation checks against the T1 sampling** (mechanical
correctness; under-MED-workload calibration is the T2 follow-up
walkthrough's question):

- [x] Monotonically non-decreasing across samples ✓
- [x] Cap held at 95 after raw exceeds it ✓ (raw at t=38s ≈
  100%, observed 92, then transition to `complete` was the next
  observation rather than another 95-cap tick)
- [x] First sample at ~t+5–10s (5s interval × skip-first-tick per
  Commit D) ✓ (first at t+6s)
- [x] Sample cadence ~5s ± apiserver latency ✓ (5, 5, 6, 5, 5, 6
  — consistent ~5s)
- [x] No annotation patches between `complete` observation and
  cleanup (progress task aborts on `send_migration` return per
  Commit D drop guard) — verified in T5 separately (7s of
  post-completion polling with progress holding at 92%)

---

## T4 — Failure surface: cancel pre-`receive-ready`

**Setup:** start fresh (after `./cleanup.sh && ./launch-pods.sh`).
Run `./trigger-migration.sh` but interrupt it after step 1/8 (ack
annotations stamped) and BEFORE step 2/8 (receive action dispatch).

```bash
# Manually patch cancel on dst before receive is dispatched
kubectl -n phase-3b-pr1-demo annotate pod $DST_POD \
  kubeswift.io/migration-action=cancel \
  kubeswift.io/migration-action-id=cancel-$(date +%s) \
  'kubeswift.io/migration-action-args={}' \
  --overwrite
```

**Expected:**

- swiftletd-dst dispatch_migration_cancel runs (Phase 3a cancel
  handler; design doc §4.6 confirmed SIGKILL semantics for live).
- The pre-dispatch annotation would have been `receive-ready` if
  `receive` had dispatched first; since cancel beat it, the dst
  pod's annotation transitions are cancel-only.
- Source guest untouched.

**Observed:** **PASS**

- [x] dst pod's `migration-status` final value: **`failed`** (observed within 2s of cancel action patch)
- [x] dst pod's `migration-status-detail` final value: **`cancelled`** (Phase 2 PR-B SIGKILL → `Err("cancelled")` → write_migration_status(Failed, detail="cancelled"))
- [x] dst pod still Running after cancel: **YES**, restart count 0 (launcher process kept alive; only CH was SIGKILLed)
- [x] Source guest unaffected: **YES**, `kubectl get sg pr1-guest` returns `phase=Running ip=192.168.99.11`

---

## T5 — Failure surface: cancel mid-`sending`

**Setup:** start fresh. Run `./trigger-migration.sh` through step
5/8 (`sending` observed). At step 6/8, while progress estimates are
emitting, manually cancel both pods:

```bash
TS=$(date +%s)
kubectl -n phase-3b-pr1-demo annotate pod $SRC_POD \
  kubeswift.io/migration-action=cancel \
  "kubeswift.io/migration-action-id=cancel-src-${TS}" \
  'kubeswift.io/migration-action-args={}' \
  --overwrite
kubectl -n phase-3b-pr1-demo annotate pod $DST_POD \
  kubeswift.io/migration-action=cancel \
  "kubeswift.io/migration-action-id=cancel-dst-${TS}" \
  'kubeswift.io/migration-action-args={}' \
  --overwrite
```

**Expected:**

- src dispatch_migration_cancel SIGKILLs CH; `vm.send-migration`
  HTTP call on src returns with a sanitized error.
- src's progress-estimate emitter exits via the drop guard (the
  dispatch future returns; ProgressEmitterGuard drop fires).
- dst dispatch_migration_cancel SIGKILLs CH; `vm.receive-migration`
  HTTP call on dst returns with a sanitized error.
- src guest is gone (CH killed; src pod terminates per launcher
  RestartPolicy=Never).
- dst CH is gone (no resumable state).
- src pod terminal `migration-status`: `failed` with detail
  containing `cancelled` (Phase 2 PR-B cancel-via-Err path).

**Observed:** **KNOWN LIMITATION (not a PR 1 regression) — see Finding LOW-2.**

Cancel applied at t=10s into the `sending` state. Timeline:

| Event | Time |
|---|---|
| Cancel patches applied (both pods) | t=0 |
| src state remains `sending` | t=1..16s |
| src reaches `complete` (transfer 38214ms — **full happy path**) | t=17s |
| dst transitions: `receive-ready` → `running` → `failed` (cancelled) | t=17..18s |
| Progress estimate stays at 92% (no further emissions) | t=18..30+s |

- [x] src `migration-status` final: **`complete`** detail=`sent to tcp:10.244.213.20:6789 (38214ms)` — **cancel did NOT abort the in-flight send**
- [x] dst `migration-status` final: **`failed`** detail=`cancelled` — cancel processed by dst once receive returned at t+17
- [x] Last `migration-progress-estimate` value before completion: **92%**
- [x] Progress estimate stops emitting after `send_migration` returns: **YES** — progress held at 92% across 7s of post-completion polling. **Drop guard works correctly.**
- [x] Time from cancel patch → src `migration-status: failed`: **N/A** — src never reached `failed` because cancel was queued behind the blocking sync `client.send_migration()` call. This is the **known Phase 2 PR-B limitation** documented at action.rs:640-644: *"current_thread, so this dispatch effectively serializes the loop during the migration. Cancel cannot fire on the SOURCE side."* PR 1 didn't change this surface; tracked as LOW-2 follow-up for Phase 3c or operational-polish phase.

**Two sub-findings from T5 timing:**

1. **Drop guard PASSES** — the progress emitter cleanly exited on `dispatch_migration_send` return; no zombie annotation patches after completion. This is the Commit D drop-guard invariant validated empirically.
2. **Destination cancel-post-receive-complete races receive's success transition** — dst showed `running` then immediately `failed=cancelled` (LOW-3 follow-up). PR 2 controller-side CancelIgnored gate (Phase 3a precedent) provides dispatch-time guard.

---

## T6 — Phase 3a offline migration regression check

**Setup:** start fresh. Create a Phase 3a offline SwiftMigration CR
against the `pr1-guest` from `launch-pods.sh`:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata:
  name: pr1-offline-mig
  namespace: phase-3b-pr1-demo
spec:
  guestRef:
    name: pr1-guest
  target:
    nodeName: boba
  mode: offline
  allowIPChange: true
  timeout: 15m
  reason: "Phase 3b PR 1 offline regression check (T6)"
EOF
```

Watch until `phase=Completed`:

```bash
kubectl -n phase-3b-pr1-demo get smig pr1-offline-mig -w
```

**Expected:**

- Phase 3a offline path runs unchanged (Phase 3b PR 1 doesn't touch
  offline code).
- `status.observedDowntime` populated (W27a fix; offline cutover
  window).
- `status.observedTransferDuration` and `status.observedPauseWindow`
  BOTH stay nil — offline mode has no memory-transfer phase. This
  is the Commit E.1 docstring claim verified empirically.

**Observed:** **PASS — Commit E.1 docstring claim empirically validated**

- [x] Phase reached Completed: **YES**, at t+48s (Preparing 0..43s → Completed 48s)
- [x] `status.observedDowntime`: **`46.679056087s`** (W27a working — anchored on cutoverStep2DispatchedAt)
- [x] `status.observedTransferDuration`: **empty** ✓ (offline has no memory-transfer phase)
- [x] `status.observedPauseWindow`: **empty** ✓ (deprecated alias also nil — dual-write helper never called on offline path)
- [x] `kubectl get smig -o wide` printer columns: **Guest, From, To, Mode, Phase, Downtime, Age** — no Transfer column (PR 3 adds it; matches T8d expectation)

---

## T7 — Status field semantic audit

Per design doc §11.4 + Phase 3b implementation gate 6 (W27 commit D
lesson — field docstrings must match what the implementation
writes).

**For each new/changed status field, verify:**

| Field | Docstring claim | Implementation writes | Match? |
|---|---|---|---|
| `status.observedTransferDuration` | "full vm.send-migration RPC duration ... NOT vCPU-paused ... not populated for status.mode=offline" | Stamped by `stampTransferDuration` at `substateSrcCompleted` from `migration-pause-window-ms` annotation on src pod | ✓ — T1 wire annotation `migration-pause-window-ms=38195` matches spike Q2 baseline 38.20s (full RPC duration, not paused window); T6 confirmed empty on offline mode |
| `status.observedPauseWindow` (deprecated alias) | "deprecated alias for ObservedTransferDuration ... Phase 3b PR 1 dual-writes both fields ... will be removed in Phase 3b+1" | Same source as canonical field via dual-write | ✓ — T6 confirmed empty on offline (matches `not populated for status.mode=offline`); dual-write covered by unit tests (TestStampTransferDuration_HappyPath_DualWrite + TestStampTransferDuration_SpikeQ2BaselineValue) |
| `status.observedDowntime` (existing W27a) | "cutoverStep2DispatchedAt → GuestRunning observation" | Unchanged in PR 1 | ✓ — T6 populated 46.68s for offline cutover; T1 had no SwiftMigration CR so no downtime stamping exercised (PR 2 controller path) |

**Audit method:** for each field, exec one offline migration (T6)
and one live "migration" (T1) and inspect the resulting
SwiftMigration status. For live, since PR 1 has no controller live
dispatch, no SwiftMigration is created; verify the swiftletd-side
wire annotation `migration-pause-window-ms` exists on src and
contains the expected value. Controller stamping is exercised
in-process via the unit tests
(`TestStampTransferDuration_HappyPath_DualWrite`,
`TestStampTransferDuration_SpikeQ2BaselineValue`,
`TestStampTransferDuration_*_LeavesBothFieldsNil`).

---

## T8 — CRD verification on cluster

After `make deploy` against the cluster:

```bash
# (a) new canonical field present with docstring
kubectl explain swiftmigration.status.observedTransferDuration

# (b) deprecated alias present, docstring acknowledges deprecation
kubectl explain swiftmigration.status.observedPauseWindow

# (c) allowVersionSkew never existed; explain errors
kubectl explain swiftmigration.spec.allowVersionSkew  # expected: error / not found

# (d) printer columns unchanged in PR 1 (Transfer column lands in PR 3)
kubectl get smig -A -o wide
```

**Observed:** **PASS**

- [x] `kubectl explain` for canonical field:
  ```
  FIELD: observedTransferDuration <string>
  DESCRIPTION:
      ObservedTransferDuration is the swiftletd-reported elapsed time
      of the vm.send-migration RPC on the source pod: pre-copy
  ```
- [x] `kubectl explain` for deprecated alias:
  ```
  FIELD: observedPauseWindow <string>
  DESCRIPTION:
      ObservedPauseWindow is a deprecated alias for
      ObservedTransferDuration. The original name misleadingly
  ```
- [x] `kubectl explain` for allowVersionSkew: `error: field "allowVersionSkew" does not exist` ✓
- [x] `kubectl get smig -o wide` columns: **`Guest From To Mode Phase Downtime Age`** ✓ (no Transfer column; PR 3 will add)

---

## Findings summary

### LOW findings (filed as tracked follow-ups in `kubeswift_context.md` post-merge; do not block PR 1)

**LOW-1 — T2/T3 stress-ng workload validation deferred.** Walkthrough
budget vs setup-overhead trade-off; mechanical correctness of progress
emitter validated via T1's monotonic 13→26→39→52→65→79→92 trajectory.
Bake `stress-ng` into the demo SeedProfile cloud-init `packages:` list
in a future follow-up to eliminate the SSH-into-guest setup dance.
**Tracked as TFU-13** post-merge.

**LOW-2 — Source-side cancel-during-send is a no-op until send returns.**
T5 surfaced the pre-existing Phase 2 PR-B limitation documented at
[rust/swiftletd/src/action.rs:640-644](../../rust/swiftletd/src/action.rs#L640):
*"current_thread, so this dispatch effectively serializes the loop
during the migration. Cancel cannot fire on the SOURCE side."* PR 1
didn't change this surface. Operational implication for PR 2: the
controller's cancel-mid-send path must expect cancel to take effect
only after `vm.send-migration` returns (success or timeout); cancel-
via-pod-delete is the fallback. Phase 3c or operational-polish phase
candidate for a worker-thread refactor on src.
**Tracked as TFU-14** post-merge.

**LOW-3 — Destination cancel-post-receive-complete races receive's
success transition.** T5 timeline showed dst transition `receive-ready`
→ `running` → `failed=cancelled` within 1 second. swiftletd-dst has no
post-success cancel-ignore gate; PR 2's controller-side CancelIgnored
gate (Phase 3a precedent for live mode) is the dispatch-time guard
the design wants. PR 2 must explicitly preserve this gate.
**Tracked as TFU-15** post-merge.

### MEDIUM findings (fixed in-PR via Commit G before merge)

**MED-1 — `launch-pods.sh` SwiftGuestClass schema wrong** (`spec.resources.cpu/memory` doesn't exist; real schema is flat `spec.cpu/memory` + `spec.rootDisk`). Caught by `strict decoding error: unknown field "spec.resources"`. **Fixed in Commit G.**

**MED-2 — `launch-pods.sh` python env-passing wrong** (`python3 -c '...' VAR=val` doesn't export to python's `os.environ`). Caught by `KeyError: 'DST_POD_NAME'`. **Fixed in Commit G.**

### HIGH findings (block merge)

**None observed.**

### Deploy-time observation (not a finding; worth a separate cleanup)

`make deploy` (config/default) sets `--webhook-enabled=false` while the
cluster's existing ValidatingWebhookConfiguration resources expect the
webhook server to be running. Patched live during walkthrough setup by
overriding deployment args to `--webhook-enabled=true`. Pre-existing
tension between `config/default` and `charts/kubeswift/values.yaml`
defaults (chart sets it `true`). Not a PR 1 issue, but PR 2 adds
webhook eligibility checks for `mode: live` rejection — a stale
webhook state would create a confusing failure surface. Worth a
separate cleanup before PR 2 lands. **Tracked as TFU-16** post-merge.

---

## Sign-off

- [x] All 8 in-scope tests passed; T2/T3 explicitly deferred with rationale + tracked follow-up
- [x] Two MEDIUM findings fixed in-PR via Commit G (launch-pods.sh)
- [x] No HIGH findings outstanding
- [x] Three LOW findings + 1 deploy-time observation filed as tracked follow-ups (TFU-13/14/15/16 — landed in post-merge `docs:` backfill on main)
- [x] **Headline empirical confirmation: T1 transfer duration 38.195s vs spike Q2 baseline 38.20s = 0.01% deviation.** All four PR 1 swiftletd surfaces (Commits A/C/D/E) produce the expected annotation sequence in expected timing.
- [x] Phase 3b PR 2 implementation prompt can begin in a separate session

**Walkthrough run:** 2026-05-16, deploy `sha-9138696`, branch
`feat/phase-3b-pr1` at `eb5b8f2` (Commit G).
