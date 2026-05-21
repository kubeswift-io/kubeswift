# Phase 3b PR 2 — Cluster Walkthrough

> Operator-driven cluster validation log for Phase 3b PR 2 (controller
> live-mode state-machine extensions — image-tag-match LBA-1 trip-wire,
> classifyFailureFromDetail extension for snake_case CH category
> tokens, and three controller-side failure-reason wirings:
> EligibilityMismatch, DstNeverReady, DstPodConflict). Per the per-PR
> walkthrough discipline established in Phase 3a + PR 1: implementation
> gates (build, unit tests) verify code correctness; walkthroughs
> verify operational correctness on a real cluster.
>
> Scaffolding: PR 2 reuses PR 1's
> [`tools/manual-demo/phase-3b-pr1/`](../../tools/manual-demo/phase-3b-pr1/)
> for T1/T8 disk-boot setup. T3/T4/T5/T6/T7 require minor scenario-
> specific adjustments captured inline.
>
> Design doc anchor:
> [`docs/design/live-migration-phase-3b.md`](../design/live-migration-phase-3b.md)
> §4.1 (image-tag-match LBA-1), §4.7 (failure-reason mapping),
> §8.2 (controller dispatch).
>
> Format: each test has **Expected** (from design + Commit C/D
> assertions) and **Observed** (filled in after running on cluster).
> Findings categorized at the end: LOW → tracked follow-up,
> MEDIUM → fix in-PR via additional commit, HIGH → block merge.
>
> **Walkthrough run:** `<DATE>`, deploy `sha-<PR2-image>`, branch
> `feat/phase-3b-pr2`.

---

## Pre-flight

| Item | Required value | Observed |
|---|---|---|
| Cluster | k0s 1.34+ on miles + boba + frida (CP) | |
| StorageClass | `longhorn-migratable` with `parameters.migratable: "true"` | |
| Webhook | controller-manager deployed with `--webhook-enabled=true` (TFU-16 fix: `make deploy-with-webhook`) | |
| Controller image | `sha-<PR2-commit>` deployed via `KUBESWIFT_LAUNCHER_IMAGE` | |
| `kubectl explain swiftmigration.status.failureReason` | shows all 12 codes (5 Phase 3a + 7 Phase 3b PR 2) | |
| `kubectl explain` shows `ImageTagMismatch`, `EligibilityMismatch`, `DstNeverReady`, `DstPodConflict`, `ReceiveDisconnect`, `RpcError`, `DstScheduleFailed` enum values | enum lists all 7 PR 2 codes | |

---

## T1 — CH auto-resume verification (LOAD-BEARING for Decision 1)

End-to-end auto-mode live migration on a 4Gi disk-boot RWX+Block
guest. **Critical observation: destination guest transitions to
running WITHOUT controller patching a `migration-action: resume`
annotation on the dst pod.**

**Setup:** PR 1's scaffolding produces a controller-less manual
demo; for PR 2 we need the controller-driven path. Use a
SwiftMigration CR instead:

```bash
# Bring up a SwiftGuest on miles (disk-boot, RWX+Block,
# longhorn-migratable). Reuse PR 1's launch-pods.sh's namespace +
# guest setup OR create equivalent SwiftGuest manually.

# Then trigger a controller-driven live migration:
kubectl -n phase-3b-pr2-demo apply -f - <<EOF
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata:
  name: t1-mig
spec:
  guestRef: {name: pr2-guest}
  target: {nodeName: boba}
  mode: auto
  allowIPChange: true
  timeout: 5m
EOF

kubectl -n phase-3b-pr2-demo get smig t1-mig -w
```

**Expected:**

| Item | Value |
|---|---|
| `status.mode` resolves to `live` (auto-selection per Phase 3a) | yes |
| Phase transitions: Pending → Validating → Preparing → StopAndCopy → Resuming → Completed | yes |
| `observedTransferDuration` ~38s (matches PR 1 spike Q2 baseline + Commit E.1 wiring) | yes |
| `observedDowntime` ~2s | yes |
| Destination guest reachable post-cutover (SSH or kubectl exec) | yes |
| **Critical:** `kubectl get pod <dst-pod> -o yaml \| grep migration-action` shows **no `resume` action** ever patched | yes |

**Observed:**

| Item | Value |
|---|---|
| status.mode | |
| Phase trajectory | |
| observedTransferDuration | |
| observedDowntime | |
| Dst guest reachable | |
| Resume action on dst pod (should be NONE) | |

**Pass criteria for Decision 1**:
- Dst guest comes up Running without a resume action → **PR 2 scope correct; ship.**
- Dst guest does NOT come up without resume → **STOP. Decision 1 wrong; PR 2 reopens with resume-action dispatch (~100-200 LOC in resuming_live.go).**
- Controller IS patching a resume action that PR 2 has no code for → **STOP. Phase 3a added resume after recon was written; PR 2 scope changes.**

**Finding:**

---

## T2 — Image-tag-match defensive trip-wire (LBA-1)

Cluster-empirical trigger of the trip-wire is intrusive (requires
deliberate launcher-image mismatch between controller default and a
running guest, e.g., via a one-shot controller deployment on a
different tag or in-place pod image edit). Unit-test coverage is
load-bearing:

- [validating_live_test.go: TestValidatingLive_ImageTagMatch_HappyPath](../../internal/controller/swiftmigration/validating_live_test.go)
- [validating_live_test.go: TestValidatingLive_ImageTagMatch_Mismatch_FailsWithImageTagMismatch](../../internal/controller/swiftmigration/validating_live_test.go)
- [validating_live_test.go: TestValidatingLive_ImageTagMatch_NoLauncherContainer_DefensiveSkip](../../internal/controller/swiftmigration/validating_live_test.go)

**If cheap cluster repro available:**

```bash
# Method: deploy a one-shot controller with KUBESWIFT_LAUNCHER_IMAGE
# pinned to a DIFFERENT tag than what the running SwiftGuest's
# launcher pod actually uses, then issue a SwiftMigration. Trip-wire
# fires at Validating-live, before mode-resolution side effects.
#
# Cleanup is operationally heavy (revert controller image, may
# require pod restart for env reload). Treat as one-shot probe;
# skip if walkthrough budget is tight.

# 1. Note current controller launcher image
kubectl -n kubeswift-system get deploy controller-manager -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KUBESWIFT_LAUNCHER_IMAGE")].value}'
# 2. Patch controller to a different launcher image (one-shot)
kubectl -n kubeswift-system set env deploy/controller-manager KUBESWIFT_LAUNCHER_IMAGE=ghcr.io/projectbeskar/kubeswift/swiftletd:fake-mismatch
kubectl -n kubeswift-system rollout status deploy/controller-manager
# 3. Trigger SwiftMigration on an existing running guest
kubectl -n phase-3b-pr2-demo apply -f t1-mig.yaml  # reuse T1's manifest
# 4. Observe Failed
kubectl -n phase-3b-pr2-demo get smig t1-mig -o jsonpath='{.status.failureReason} {.status.failureMessage}{"\n"}'
# 5. Revert controller image
kubectl -n kubeswift-system set env deploy/controller-manager KUBESWIFT_LAUNCHER_IMAGE=<original-value>
```

**Expected (if cluster repro performed):**

| Item | Value |
|---|---|
| `status.failureReason` | `ImageTagMismatch` |
| `status.failureMessage` contains both image strings | yes |
| `status.failureMessage` references `LBA-1` | yes |

**Observed:** ☐ unit-test-only validation accepted / ☐ cluster repro performed (fill below)

| Item | Value |
|---|---|
| failureReason | |
| failureMessage (truncated) | |

**Finding:**

---

## T3 — Failure classifier on src-side RPC error (Commit C)

Force a src-side migration failure by killing the dst pod mid-send.
Source observes peer drop; `vm.send-migration` returns
`Connect`/`Transport` error; sanitize_ch_error produces
`send_migration: connection_refused` or `send_migration: transport_error`;
classifier routes to **ReceiveDisconnect**.

**Setup:**

```bash
# Apply a new SwiftMigration on the same guest.
kubectl -n phase-3b-pr2-demo apply -f t3-mig.yaml  # mode=live

# Wait until phase=StopAndCopy and phaseDetail=transferring; then
# delete the dst pod with --grace-period=0 to simulate hard failure.
kubectl -n phase-3b-pr2-demo get smig t3-mig -w &
# When status hits StopAndCopy:
DST_POD=$(kubectl -n phase-3b-pr2-demo get smig t3-mig -o jsonpath='{.status.destinationPodRef.name}')
kubectl -n phase-3b-pr2-demo delete pod "$DST_POD" --grace-period=0 --force
```

**Expected:**

| Item | Value |
|---|---|
| Phase reaches Failed | yes |
| `status.failureReason` | `ReceiveDisconnect` or `RpcError` |
| `status.failureMessage` contains `send_migration:` prefix + a sanitize_ch_error category token | yes |
| Source SwiftGuest restored to runPolicy=Running on miles | yes |

**Observed:**

| Item | Value |
|---|---|
| failureReason | |
| failureMessage (truncated) | |
| Source guest restored | |

**Finding:**

---

## T4 — Failure classifier on dst-side never-ready (Commit C) + DstNeverReady semantic refinement

Force the PreparingLive 60s budget timeout by deleting the dst pod
immediately after creation, before swiftletd-dst emits
`migration-status: receive-ready`. Controller observes the absence
of receive-ready within budget; stamps **DstNeverReady**.

**Setup:**

```bash
kubectl -n phase-3b-pr2-demo apply -f t4-mig.yaml
# As soon as status.destinationPodRef populates, force-delete:
DST_POD=$(kubectl -n phase-3b-pr2-demo get smig t4-mig -o jsonpath='{.status.destinationPodRef.name}')
kubectl -n phase-3b-pr2-demo delete pod "$DST_POD" --grace-period=0 --force
# Repeat the delete in a loop for ~60s if the controller recreates;
# alternative: scale down the target node briefly to prevent
# scheduling, then scale back to verify the budget-timeout path.
```

**Expected:**

| Item | Value |
|---|---|
| Phase reaches Failed within ~60s | yes |
| `status.failureReason` | **`DstNeverReady`** (NOT `PodTerminated` — see semantic-refinement note below) |
| `status.failureMessage` contains `never reached Ready within 60s budget` | yes |

**Observed:**

| Item | Value |
|---|---|
| failureReason | |
| Time to Failed | |
| failureMessage (truncated) | |

**Semantic-refinement note for operators upgrading from Phase 3a:**

Phase 3a reported `FailureReason=PodTerminated` for both:
1. dst pod genuinely terminated mid-migration (drain, OOM)
2. dst pod alive-but-stuck within PreparingLive budget

Phase 3b PR 2 splits these:
1. dst pod genuinely terminated → still `PodTerminated`
2. dst pod alive-but-stuck → **`DstNeverReady` (NEW)**

**Operators with dashboards filtering on `PodTerminated` will no
longer match scenario (2).** Update dashboard queries to match
`PodTerminated OR DstNeverReady` for the union, or split the
panels per the new taxonomy.

A future PR will additionally split `DstScheduleFailed` from
`DstNeverReady` (scheduler-rejected vs alive-but-stuck); see
PR 2 close-out tracked follow-up.

**Finding:**

---

## T5 — Cancel pre-receive-ready (Phase 3a regression check)

Issue a SwiftMigration, then immediately patch
`spec.cancelRequested=true` before the dst pod reaches
receive-ready. Pre-cutover cancel path drives migration to Cancelled.

**Setup:**

```bash
kubectl -n phase-3b-pr2-demo apply -f t5-mig.yaml
# As soon as status.phase==Validating (before Preparing's dst pod
# creation completes), patch cancel:
kubectl -n phase-3b-pr2-demo patch smig t5-mig --type=merge -p '{"spec":{"cancelRequested":true}}'
```

**Expected:**

| Item | Value |
|---|---|
| Final phase | `Cancelled` |
| `status.failureReason` | `Cancelled` |
| Source SwiftGuest restored to runPolicy=Running on miles | yes |
| Destination pod deleted | yes |

**Observed:**

| Item | Value |
|---|---|
| Final phase | |
| failureReason | |
| Source guest restored | |
| Dst pod deleted | |

**Finding:**

---

## T6 — Cancel mid-send-active (TFU-14 verification)

Issue a SwiftMigration, wait until StopAndCopy substateSendPending
(swiftletd-src in the middle of `vm.send-migration` blocking RPC),
then patch `spec.cancelRequested=true`. Per TFU-14: source-side
cancel-during-send is a NO-OP until the RPC returns (swiftletd's
action loop is on `current_thread` tokio runtime, starved by the
blocking sync HTTP call).

**Expected timeline:**

| t (from cancel patch) | Event |
|---|---|
| t+0 | spec.cancelRequested=true patched |
| t+0..~30s | src `migration-status: sending` UNCHANGED (action loop blocked); progress estimate continues emitting |
| t+0..~5s | dst processes cancel via its action loop (NOT blocked) — may transition to failed=cancelled |
| t+~38s | src `vm.send-migration` returns; controller observes the late cancel signal |
| Final | SwiftMigration → Cancelled (or Failed with classifier-mapped reason if src observed dst's cancel-side abort as a transport error) |

**Setup:**

```bash
kubectl -n phase-3b-pr2-demo apply -f t6-mig.yaml
# Wait until status.phaseDetail contains "transferring":
while [ "$(kubectl -n phase-3b-pr2-demo get smig t6-mig -o jsonpath='{.status.phaseDetail}')" != *transferring* ]; do sleep 2; done
# Patch cancel mid-send:
kubectl -n phase-3b-pr2-demo patch smig t6-mig --type=merge -p '{"spec":{"cancelRequested":true}}'
# Record annotation timeline:
echo "cancel_patched_at: $(date -Is)"
for i in $(seq 1 60); do
  src_status=$(kubectl -n phase-3b-pr2-demo get pod <src-pod> -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status}')
  dst_status=$(kubectl -n phase-3b-pr2-demo get pod <dst-pod> -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status}' 2>/dev/null)
  echo "t+${i}s src=${src_status} dst=${dst_status}"
  sleep 1
done
```

**Observed timeline:**

| t (from cancel patch) | src status | dst status |
|---|---|---|
| t+0 | | |
| t+5 | | |
| t+15 | | |
| t+30 | | |
| t+38 (approx send return) | | |
| Final | | |

**Expected:** src `migration-status` stays at `sending` until `vm.send-migration` returns, demonstrating TFU-14's "cancel-takes-effect-only-after-RPC" semantic.

**Finding:**

---

## T7 — W21 CancelIgnored gate (TFU-15 verification)

Issue a SwiftMigration, wait until the StopAndCopy substate reaches
substateSrcCompleted (W1 gate passed; src reported migration-status=complete),
then patch `spec.cancelRequested=true`. Per Phase 3a W21
implementation: post-cutover (PodRefSwapped=True after cutoverStep1)
cancel is IGNORED; migration completes normally with a
CancelIgnored condition.

PR 2 must preserve W21 enforcement — same code path as Phase 3a.
This test is a regression guard.

**Setup:**

```bash
kubectl -n phase-3b-pr2-demo apply -f t7-mig.yaml
# Wait until status.phaseDetail contains "preparing cutover"
# (substateSrcCompleted has fired; cutoverStep1 imminent):
while [ "$(kubectl -n phase-3b-pr2-demo get smig t7-mig -o jsonpath='{.status.phaseDetail}')" != *preparing\ cutover* ]; do sleep 1; done
# Patch cancel:
kubectl -n phase-3b-pr2-demo patch smig t7-mig --type=merge -p '{"spec":{"cancelRequested":true}}'
# Continue observing — expect Completed, not Cancelled.
kubectl -n phase-3b-pr2-demo get smig t7-mig -w
```

**Expected:**

| Item | Value |
|---|---|
| Final phase | `Completed` (NOT Cancelled) |
| `status.conditions[?(@.type=='CancelIgnored')]` | `True`, reason=`PastCutover` |
| Migration completes successfully end-to-end | yes |
| Dst guest reachable post-completion | yes |

**Observed:**

| Item | Value |
|---|---|
| Final phase | |
| CancelIgnored condition | |
| Dst guest reachable | |

**Finding:**

---

## T8 — Phase 3a offline regression

One round-trip with `spec.preferredMode=offline` (or `spec.mode=offline`)
to verify PR 2's live-mode-targeted changes have not regressed the
Phase 1 offline path.

**Setup:**

```bash
kubectl -n phase-3b-pr2-demo apply -f - <<EOF
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata:
  name: t8-mig
spec:
  guestRef: {name: pr2-guest}
  target: {nodeName: miles}   # back to miles for the round-trip
  mode: offline
  allowIPChange: true
  timeout: 10m
EOF
```

**Expected:**

| Item | Value |
|---|---|
| `status.mode` | `offline` |
| Final phase | `Completed` |
| `status.observedDowntime` | populated (~70s Longhorn) |
| `status.observedTransferDuration` | empty/nil (PR 1 Commit E.1 — only populated for live mode) |
| `status.failureReason` | empty/nil (Phase 1 offline mode does not populate this field per types docstring) |

**Observed:**

| Item | Value |
|---|---|
| status.mode | |
| Final phase | |
| observedDowntime | |
| observedTransferDuration | |
| failureReason | |

**Finding:**

---

## Pass criteria mapped to PR 2 commits

| Test | Commit | Decision/Property guarded |
|---|---|---|
| T1 | (no commit; load-bearing for entire PR scope) | Decision 1: CH auto-resume; resume action NOT in PR 2 |
| T2 | Commit B | Image-tag-match LBA-1 trip-wire |
| T3 | Commit C classifier extension | `send_migration:` / `receive_migration:` prefix routing |
| T4 | Commit C wiring + DstNeverReady semantic refinement | Budget-timeout reports DstNeverReady, not PodTerminated |
| T5 | Phase 3a regression | Pre-cutover cancel path |
| T6 | TFU-14 verification (no PR 2 commit; Phase 2 PR-B limitation) | Source cancel-during-send is no-op until RPC returns |
| T7 | TFU-15 verification (Phase 3a W21 gate) | Post-cutover cancel is IGNORED |
| T8 | Non-regression | Offline-mode round-trip unaffected by PR 2 live-mode changes |

---

## Findings summary

### LOW findings (filed as tracked follow-ups in `kubeswift_context.md` post-merge; do not block PR 2)

*(Fill in after walkthrough run.)*

### MEDIUM findings (fixed in-PR via additional commit before merge)

*(Fill in after walkthrough run. Expected: none if T1-T8 all PASS.)*

### HIGH findings (block merge)

*(Fill in after walkthrough run. Expected: none; T1 STOP-and-report
clauses captured above for the two ways T1 can produce a HIGH —
controller patching resume action that PR 2 has no code for, OR
dst not coming up without resume.)*

---

## Sign-off

- [ ] T1 confirms CH auto-resume (Decision 1 validated on cluster)
- [ ] T2 unit-test-validated OR cluster repro performed
- [ ] T3 / T4 confirm Commit C classifier + DstNeverReady refinement
- [ ] T5 / T6 / T7 confirm Phase 3a behaviors preserved
- [ ] T8 confirms offline-mode non-regression
- [ ] No HIGH findings outstanding
- [ ] LOW / MEDIUM findings categorized and dispositioned
- [ ] Walkthrough run metadata filled in (date, image SHA, branch
      commit hash)

**Walkthrough run:** `<DATE>`, deploy `sha-<PR2-image>`, branch
`feat/phase-3b-pr2` at `<commit-hash>`.
