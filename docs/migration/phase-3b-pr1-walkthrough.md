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
> **Observed** (operator fills in after running on cluster).
> Findings categorized at the end: LOW → tracked follow-up,
> MEDIUM → fix in-PR via additional commit, HIGH → block merge.

## Pre-flight

| Item | Required value | Observed |
|---|---|---|
| Cluster | k0s 1.34+ on miles + boba + frida (CP) | |
| StorageClass | `longhorn-migratable` with `parameters.migratable: "true"` | |
| Controller image | swiftletd `sha-<PR1-commit>` deployed via `KUBESWIFT_LAUNCHER_IMAGE` | |
| `kubectl explain swiftmigration.status.observedTransferDuration` | shows the new field with docstring | |
| `kubectl explain swiftmigration.spec.allowVersionSkew` | field absent (was never added; verified in PR 1 recon) | |
| `kubectl explain swiftmigration.status.observedPauseWindow` | shows deprecated-alias docstring | |

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
| `receive-ready` latency from receive action patch | |
| `sending` latency from send action patch | |
| First progress estimate (t and pct) | |
| Progress monotonic? Cap at 95 held? | |
| Total `sending` → `complete` wall-clock | |
| `migration-pause-window-ms` value | |
| Total wall-clock (dispatch → dst running) | |

**Finding (if any):** _(none expected; deviation > 20% from spike Q2 baseline is a finding worth investigating)_

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

**Observed:**

| Metric | Value |
|---|---|
| Total wall-clock | |
| `migration-pause-window-ms` value | |
| Progress estimate accuracy (e.g., at t=30s, expected=68s, raw=44%, observed=?) | |

**Finding (if any):** _(Phase 3b open question 11.1: progress
estimate accuracy under realistic workloads. Drift > 20% is a
calibration question to surface, not a bug per se.)_

---

## T3 — Progress estimate samples during MED migration

Captured via `trigger-migration.sh`'s step 6/8 output during T2.
Tabulate the samples here for the walkthrough log:

| t (s) | `migration-progress-estimate` value |
|---|---|
| | |
| | |
| | |
| | |
| | |
| | |

**Validation checks:**

- [ ] Monotonically non-decreasing across samples
- [ ] Cap held at 95 after raw exceeds it
- [ ] First sample at ~t+5–10s (5s interval × skip-first-tick per
  Commit D)
- [ ] Sample cadence ~5s ± apiserver latency
- [ ] No annotation patches between `complete` observation and
  cleanup (progress task aborts on `send_migration` return per
  Commit D drop guard)

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

**Observed:**

- [ ] dst pod's `migration-status` final value: ______
- [ ] dst pod's `migration-status-detail` final value: ______
- [ ] dst pod still Running after cancel? (cancel is SIGKILL of CH
  process inside the pod; the pod itself stays Running until K8s
  reaps it; subsequent receive actions on the same pod would fail)
- [ ] Source guest unaffected (`swiftctl describe pr1-guest`
  shows Running): ______

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

**Observed:**

- [ ] src `migration-status` final value + detail: ______
- [ ] dst `migration-status` final value + detail: ______
- [ ] Last `migration-progress-estimate` value before cancel: ______
- [ ] Progress estimate stops emitting after cancel (drop guard
  effectiveness): ______
- [ ] Time from cancel patch → src `migration-status: failed`:
  ______ (Phase 2 PR-B cancel handler — Phase 3a action handler
  drains; ~1–2s expected)

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

**Observed:**

- [ ] Phase reached Completed: ______ (yes/no + time)
- [ ] `status.observedDowntime`: ______
- [ ] `status.observedTransferDuration`: ______ (must be nil)
- [ ] `status.observedPauseWindow`: ______ (must be nil)
- [ ] `swiftctl migration describe pr1-offline-mig` output sensible
  (downtime present; no broken nil-Duration formatting): ______

**Finding (if any):** _(any non-nil observedTransferDuration on
offline mode is a HIGH finding — contradicts the Commit E.1
docstring and would block merge)_

---

## T7 — Status field semantic audit

Per design doc §11.4 + Phase 3b implementation gate 6 (W27 commit D
lesson — field docstrings must match what the implementation
writes).

**For each new/changed status field, verify:**

| Field | Docstring claim | Implementation writes | Match? |
|---|---|---|---|
| `status.observedTransferDuration` | "full vm.send-migration RPC duration ... NOT vCPU-paused ... not populated for status.mode=offline" | Stamped by `stampTransferDuration` at `substateSrcCompleted` from `migration-pause-window-ms` annotation on src pod | |
| `status.observedPauseWindow` (deprecated alias) | "deprecated alias for ObservedTransferDuration ... Phase 3b PR 1 dual-writes both fields ... will be removed in Phase 3b+1" | Same source as canonical field via dual-write | |
| `status.observedDowntime` (existing W27a) | "cutoverStep2DispatchedAt → GuestRunning observation" | Unchanged in PR 1 | (existing — verified in W27 audit) |

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

**Observed:**

- [ ] `kubectl explain` for canonical field: paste first 5 lines
  ______
- [ ] `kubectl explain` for deprecated alias: paste first 5 lines
  ______
- [ ] `kubectl explain` for allowVersionSkew: error message text
  ______
- [ ] `kubectl get smig -o wide` columns: ______
  (expected: Guest, From, To, Mode, Phase, Downtime, Age — no
  Transfer column until PR 3)

---

## Findings summary

Fill in after running all 8 tests.

### LOW findings (file as tracked follow-up; do not block PR 1 merge)

_(none observed / list below)_

### MEDIUM findings (fix in-PR via additional commit before merge)

_(none observed / list below)_

### HIGH findings (block merge; reset and rework)

_(none observed / list below)_

---

## Sign-off

- [ ] All 8 tests passed (LOW findings filed as tracked follow-ups,
  MEDIUM findings fixed in-PR, no HIGH findings outstanding)
- [ ] Walkthrough log committed alongside the PR
- [ ] Phase 3b PR 2 implementation prompt can begin

**Walkthrough operator:** ____________  **Date:** ____________
