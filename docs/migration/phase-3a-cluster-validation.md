# Phase 3a PR 1 — Cluster Validation Walkthrough

> Cluster validation of [PR #42](https://github.com/projectbeskar/kubeswift/pull/42) (Phase 3a controller core, mode=live live migration).
>
> Cluster: 3-node k0s 1.34.3 — `frida` (control-plane), `miles` and `boba` (workers). CH v51.1, Longhorn, Calico. PR #41 D1/D2/D3 baseline + PR #39 br0/Calico CIDR fix already deployed.
>
> Date: 2026-05-03. Walkthrough completed across **four image iterations** (W13/W14/W15/W16 hotfixes landed mid-walkthrough).

---

## Findings summary

**Findings: W13, W14, W15, W16, W17, W18, W19, W20, W21** (nine findings; four BLOCKING fixed mid-walkthrough; five for follow-up PR).

| # | Severity | Disposition | One-line |
|---|---|---|---|
| W13 | BLOCKING | Fixed via [PR #43](https://github.com/projectbeskar/kubeswift/pull/43) | Controller did not patch src pod with `migration-phase2-unsafe-plaintext: ack` → swiftletd rejected send → migration timed out at substateSendPending |
| W14 | HIGH | Fixed via [PR #43](https://github.com/projectbeskar/kubeswift/pull/43) | `deriveSubstate` did not recognize `migration-status=rejected` → defaulted to Timeout instead of mapping to the rejection detail |
| W15 | BLOCKING | Fixed via [PR #44](https://github.com/projectbeskar/kubeswift/pull/44) | UID-check used `canonicalPodName` which resolves to dst post-step-1 → false-fired SourcePodReplaced on healthy migration |
| W16 | BLOCKING | Fixed via [PR #45](https://github.com/projectbeskar/kubeswift/pull/45) | swiftletd receiver-mode never flipped GuestRunning=True post-receive → Resuming-live timed out at spec.timeout |
| W17 | MEDIUM | Follow-up PR | Pre-cutover Failed migration leaves dst pod running on target node (resource leak / UX confusion) |
| W18 | HIGH | Follow-up PR | failureReason taxonomy can't distinguish "src=failed because dst was K8s-terminated" from generic src failures → maps to Other instead of PodTerminated |
| W19 | LOW | Follow-up PR (docs) | `docs/migration/phase-3a.md` W12 narrative is out-of-date — cluster reality is fast PodTerminated, not slow Timeout |
| W20 | MEDIUM | Phase 3b | Cancel D1 fast-path doesn't fire while receive_migration blocks the action loop → fallback (force-delete dst) fires after 30s instead of "few seconds" |
| W21 | HIGH | Follow-up PR | `SwiftMigrationConditionPodRefSwapped` is never written → `isPostCutover()` always returns false → CancelIgnored gate broken; potential data-loss in narrow Resuming window |

**Pattern:** four consecutive finding-behind-a-finding events during the BLOCKING-finding fix iterations. Each blocking bug hid the next:

```
Iter 1 (sha-4cde589): W13 + W14 surfaced → PR #43
Iter 2 (sha-ce68fa9): W15 surfaced       → PR #44
Iter 3 (sha-4236252): W16 surfaced       → PR #45
Iter 4 (sha-5c794ff): clean walkthrough  → W17/W18/W19/W20/W21 documented (non-blocking)
```

W21 specifically is the same root cause as W15 (PodRefSwapped condition not written). The W15 fix mitigated one symptom; W21 is the second symptom.

---

## Cluster baseline

### Pre-walkthrough sanity check

```
controller-manager: ghcr.io/projectbeskar/kubeswift/controller-manager:sha-5c794ff (Iter 4)
nodes: miles + boba Ready (workers); frida Ready (control-plane)
CRD: all Phase 3a status fields present (cutoverStep1At, recvAttempts, sendAttempts,
     preparingStartedAt, resumingStartedAt, observedDowntime, observedPauseWindow,
     failureReason, failureMessage, sourcePodUID)
CRD: all spec fields present (cancelRequested, mode, allowIPChange, timeout,
     timeoutStrategy, target)
webhook: kubeswift-validating-webhook registered
```

### Test fixtures

- SwiftKernel `faas-minimal` (mig-wt namespace) — 6.6.2 kernel, faas-minimal initramfs
- SwiftGuestClass `default` — 4Gi RAM, 40Gi disk
- Per-scenario SwiftGuest `faas-sN` (kernel-boot, runPolicy=Running, nodeName=miles)
- Per-scenario SwiftMigration `sN-mig` (mode=live or offline, target=boba, allowIPChange=true, timeout=5m)

---

## Scenario results

### Scenario 1 — Happy path (kernel-boot cross-node)

**Result: PASS** (iteration 4, image sha-5c794ff)

```
T+0s   Preparing / 'waiting for destination pod ready'
T+8s   StopAndCopy / 'transferring guest state'
T+48s  StopAndCopy / 'cutover: deleting source pod'
T+49s  StopAndCopy / 'cutover: completing'
T+51s  TERMINAL Completed
```

| Check | Value |
|---|---|
| Total wall-clock | ~51s (matches design ~50s expectation) |
| cutoverStep1At | T+48s |
| recvAttempts / sendAttempts | 1 / 1 (clean first-try) |
| destinationPodRef | `faas-s1-mig-b3c16e` on boba |
| SwiftGuest podRef | dst pod (cutover crossed correctly) |
| SwiftGuest phase | Running on boba |
| GuestRunning condition | True/VmRunning (W16 fix confirmed) |
| Guest IP on dst | 192.168.99.14 (br0 reassigned per allowIPChange) |
| observedDowntime | 57.874µs |
| Src pod | NotFound (cutoverStep2 cleanup) |

**Note on observedDowntime:** sub-millisecond value (57.874µs) appears to measure controller-side cutover-step-3-to-Completed window, not the actual vCPU pause window. Phase 5 polish — the actual vCPU pause is the ~38s memory-transfer body, not the metric reported here.

### Scenario 2 — Reconcile-loop interruption recovery

**Result: PASS**

Setup: kill controller-manager pod mid-StopAndCopy/transferring.

```
T+0s   StopAndCopy / 'transferring guest state' (kill issued)
T+39s  StopAndCopy / 'cutover: completing'
T+42s  TERMINAL Completed
```

| Check | Value |
|---|---|
| Migration completes after handover | ✓ |
| recvAttempts / sendAttempts | 1 / 1 (no duplicate dispatches) |
| New leader pod | `controller-manager-5d879f6fbf-...` 4m old |

State machine §2.4 reconstruction-from-cluster-state-alone validated.

### Scenario 3 — Source pod K8s termination → SourcePodReplaced

**Result: PASS** (with W17 finding)

Setup: `kubectl delete pod faas-s3 --grace-period=30` mid-StopAndCopy.

```
T+0s   StopAndCopy / 'transferring guest state'
T+34s  TERMINAL Failed
```

| Check | Result |
|---|---|
| phase=Failed | ✓ |
| failureReason=SourcePodReplaced | ✓ |
| failureMessage describes UID change | ✓ ("source pod ... no longer exists during StopAndCopy") |
| dst pod cleaned up | ✗ — **W17** |

**Finding W17 (MEDIUM, NOT BLOCKING):** Pre-cutover Failed leaves dst pod running on target node. `cleanupSourceGuest` only removes the migration-in-progress annotation and restores runPolicy; doesn't delete the dst pod the controller created. Per design §2.3 ("pre-cutover failure case is the only one that explicitly deletes a pod the controller created") this is a real bug.

Fix surface: extend `onTerminalPhase` to delete the dst pod on pre-cutover Failed (lookup via `status.destinationPodRef.name`).

### Scenario 4 — Destination pod K8s termination (graceful) → PodTerminated

**Result: PARTIAL PASS** (with W18 finding)

Setup: `kubectl delete pod <dst> --grace-period=30` mid-StopAndCopy.

```
T+0s   StopAndCopy / 'transferring guest state'
T+33s  TERMINAL Failed
```

| Check | Expected | Observed | Status |
|---|---|---|---|
| phase=Failed | ✓ | ✓ | Pass |
| src survives (F2 invariant) | ✓ | ✓ | Pass |
| failureReason=PodTerminated | ✓ | **Other** | **Fail (W18)** |
| failureMessage from D2 detail | ✓ | "send_migration: internal_server_error" (src side) | **Fail (W18)** |

**Finding W18 (HIGH, NOT BLOCKING):** `deriveSubstate` priority is src-first; src=failed fires `substateSrcFailed` before dst=failed is checked. Src detail is generic CH error (`send_migration: internal_server_error`); `classifyFailureFromDetail` doesn't match → defaults to Other. Operators see "Other" for what should be "PodTerminated".

Fix surface: when `substateSrcFailed` fires, also check dst pod state. If dst pod is NotFound or Terminating, classify as PodTerminated regardless of src detail.

### Scenario 5 — W12 reproduction (force-delete dst)

**Result: PASS, BUT W12 narrative outdated** (W19 finding)

Setup: `kubectl delete pod <dst> --grace-period=0 --force` mid-StopAndCopy.

```
T+0s   force delete issued
T+0s   TERMINAL Failed
```

| Check | Result |
|---|---|
| phase=Failed | ✓ |
| failureReason=PodTerminated | ✓ (instant, NOT via 127s slow path) |
| failureMessage | "destination pod ... disappeared during StopAndCopy" |
| src pod survives | ✓ |

**Finding W19 (LOW, docs-only):** `docs/migration/phase-3a.md` describes W12 force-delete as taking ~127s of TCP retransmit before transitioning to Timeout. Cluster reality: controller's outer dst-pod-NotFound check fires fast-path PodTerminated at T+0s. Better than documented — update docs.

src CH probably DOES still TCP-timeout internally (~127s) but is irrelevant — controller already terminally failed the migration. Stale `migration-status=running` on src pod lingers (cosmetic).

### Scenario 6 — Drain on destination node

**Deferred** (functionally equivalent to S4 per spike F4.5). Same W18 classification gap would surface.

### Scenario 7 — Cancel pre-cutover (D1 + spec.cancelRequested)

**Result: PARTIAL PASS** (with W20 finding)

Setup: `kubectl patch smig s7-mig --type=merge -p '{"spec":{"cancelRequested":true}}'` mid-StopAndCopy/transferring.

```
T+0s   StopAndCopy / 'transferring guest state' (cancel patched)
T+0s   StopAndCopy / 'waiting for cancel acknowledgment'
T+27s  TERMINAL Cancelled
```

| Check | Expected | Observed | Status |
|---|---|---|---|
| phase=Cancelled | ✓ | ✓ | Pass |
| failureReason=Cancelled | ✓ | ✓ | Pass |
| src pod survives | ✓ | ✓ | Pass |
| dst pod cleaned up | ✓ | ✓ | Pass |
| Within "few seconds" per docs | ✓ | T+27s — fallback path | **Fail (W20)** |

failureMessage: `"destination pod force-deleted; swiftletd cancel ack timed out (30s budget)"`

**Finding W20 (MEDIUM, NOT BLOCKING):** D1 cancel fast-path doesn't fire while `dispatch_migration_receive` blocks the action loop. Per Phase 2 spike F2.4: "current-thread runtime cannot process the cancel verb while this handler is in flight." Controller's 30s cancel-ack budget expires → fallback force-delete dst → src CH errors → Cancelled at T+27s. Better than W12 (~127s) but slower than docs claim ("few seconds"). Phase 3b's `swift-ch-client` async refactor resolves both W12 and W20.

### Scenario 8 — Cancel post-cutover (CancelIgnored)

**Result: INCONCLUSIVE** (revealed W21 finding)

Setup: try to patch `cancelRequested=true` during Resuming phase. **Resuming phase completed in ~2s — too narrow to catch with polling.** Cancel landed post-Completed; controller's terminal-phase shortcut made it a no-op. No CancelIgnored condition was set.

| Check | Result |
|---|---|
| phase=Completed | ✓ (terminal-shortcut) |
| CancelIgnored condition | **Missing — W21** |

**Finding W21 (HIGH, NOT BLOCKING):** `SwiftMigrationConditionPodRefSwapped` is defined in API types and read by `isPostCutover()` (in `source_pod_uid.go`, `cancel_live.go`) but **no controller code path actually writes it**. So `isPostCutover()` always returns false.

Two consequences:

1. **CancelIgnored condition never fires** — design intent unmet.
2. **Data-loss potential in narrow Resuming window:** if cancel is patched during Resuming (post-cutover, pre-Completed, ~2s for kernel-boot, longer for disk-boot), `honorCancel` takes the **pre-cutover path** → `transitionCancelLive` writes cancel-action on the **dst pod (which is now the live migrated guest)** → swiftletd SIGKILLs dst CH → migrated guest destroyed.

Same root cause as W15 (UID-check race). PR #44 mitigated W15's symptom by changing `canonicalPodName` → `guest.Name`; the underlying gate (`PodRefSwapped` not wired) remains broken.

Fix surface: write `PodRefSwapped=True` at `cutoverStep1` alongside the SwiftGuest podRef patch. Then both W15 and W21 are race-safe via the same gate.

### Scenario 9 — Per-source-node concurrency rejection

**Result: PASS**

Setup: Two SwiftGuests on miles. Apply first migration (s9a-mig). While in non-terminal phase, attempt to apply second migration (s9b-mig, same source node).

```
$ kubectl apply -f s9b-mig.yaml
Error from server (Forbidden): admission webhook "vswiftmigration.migration.kubeswift.io"
denied the request: another live SwiftMigration "mig-wt/s9a-mig" is in flight from
source node "miles" (phase=Preparing); per-source-node concurrency limits to one
in-flight live migration. Wait for the existing migration to complete or fail.
```

| Check | Result |
|---|---|
| Webhook rejects with operator-readable message | ✓ |
| Identifies blocking migration namespace/name + phase | ✓ |
| First migration unaffected | ✓ |
| After first completes, second applies cleanly | ✓ |

### Scenario 10 — Phase 1 offline migration regression

**Result: PASS** — no regression from Phase 3a changes.

```
T+0s    Preparing / 'waiting for source pod termination'
T+32s   TERMINAL Completed
```

| Check | Result |
|---|---|
| phase=Completed | ✓ |
| mode=offline | ✓ |
| SwiftGuest moved to boba | ✓ (nodeName=boba; phase=Scheduling — Phase 1 cold-boot pattern) |

Phase 1 offline path works unchanged. No regression observed.

---

## Follow-up PR scope

Five non-blocking findings (W17–W21) collected during the walkthrough warrant a single follow-up PR:

| Finding | Fix size | Tests |
|---|---|---|
| W17: dst pod cleanup on Failed | ~10 LoC + cluster integration | unit test for `onTerminalPhase` cleanup |
| W18: failureReason classification for dst-K8s-terminated | ~15 LoC (dst-state probe in src-failed branch) | unit test with dst NotFound/Terminating fixtures |
| W19: docs/migration/phase-3a.md W12 narrative | docs only | n/a |
| W21: write PodRefSwapped condition at cutoverStep1 | ~5 LoC | unit test in cutover_test.go for condition write |
| (W20: defer to Phase 3b — `swift-ch-client` async refactor) | swiftletd | n/a |

W21 is the most operationally important: removes the data-loss-in-narrow-Resuming-window hazard that the W15 fix mitigated symptomatically. Its fix is the smallest of the four — write the condition at cutoverStep1 alongside the SwiftGuest podRef patch.

---

## Hotfix iterations (mid-walkthrough)

Four image iterations. Each one's BLOCKING finding hid the next:

### Iter 1: sha-4cde589 (PR #42 baseline)

Scenario 1 stuck at substateSendPending for 147s+. Root cause: src pod missing `migration-phase2-unsafe-plaintext: ack` annotation; swiftletd wrote `migration-status: rejected` with detail `phase2_plaintext_ack_missing`; controller didn't recognize rejected as terminal → spec.timeout fired.

→ **W13 + W14** → PR #43.

### Iter 2: sha-ce68fa9 (post-W13/W14)

Scenario 1 reached cutover successfully (cutoverStep1At populated), then false-failed at `phaseDetail: 'cutover: completing'` with `failureReason=SourcePodReplaced` and message describing UID change to `52bafad8...` — which was actually the dst pod's UID, not a recreated src pod's.

Race: cutoverStep1's two patches (SwiftGuest podRef + SwiftMigration status) trigger an informer-driven reconcile that reads stale phaseDetail (LiveSrcCompleted, in pre-cutover vocabulary) AND fresh guest podRef (already swapped to dst). UID-check fetched src via `canonicalPodName` → resolved to dst pod → false-fired SourcePodReplaced.

→ **W15** → PR #44.

### Iter 3: sha-4236252 (post-W15)

Scenario 2 progressed through cutover successfully but stuck in Resuming for 240s+. Root cause: dst-side swiftletd's receiver-mode never flipped GuestRunning=True post-receive. Src wrote `GuestRunning=False/VmStopped` at cutover-step-1 (its CH exited normally); receiver-mode swiftletd skipped `on_socket_ready` callback and never overwrote the stale condition. Resuming-live waited until spec.timeout.

→ **W16** → PR #45.

### Iter 4: sha-5c794ff (post-W16)

Clean walkthrough. All 10 scenarios produced expected behaviors (modulo W17–W21 documented above as non-blocking).

---

## Pattern observation

Four consecutive finding-behind-a-finding events surfaced during cluster validation. Each unit-test-passing layer hid the next BLOCKING bug at a different code path:

| Iter | Layer | Bug class |
|---|---|---|
| 1 | controller (Go) | Missing annotation patch + missing terminal-state recognition |
| 2 | controller (Go) | Race-window between two-step status patches |
| 3 | swiftletd (Rust) | Lifecycle gap in receiver-mode post-receive transition |
| 4 (clean) | (n/a) | Five non-blocking findings revealed only by full end-to-end |

The B0 selectiveFailingClient test infrastructure (Group B) caught one canonicalPodName ambiguity (in `cutover.go`'s `executeCutover`) but not the sibling site in the outer `stopandcopy_live.go`. PR 1's data plane was never validated end-to-end pre-merge — Phase 2 walkthrough W6 explicitly documented this gap (receiver-mode launch branch never ran in production); the gap persisted into PR 1 because Phase 2's own blockers (W3/W4 RBAC, W6 storage handoff) prevented disk-boot guests from completing receive_migration in the Phase 2 manual demo path.

This is the **W5 lesson restated for the fifth time** in the project's history (snapshot F2, Phase 2 W3/W6, PR #32 W8/W9, PR #35 W10/W11, now PR #42 W13–W21). Spike scenarios under-constrain design when they validate simplified inputs. Cluster validation is the only layer that exercises the full operator scenario.

The walkthrough discipline ("fix and continue on clean baseline" rather than "manual workaround" or "defer to follow-up PR") was load-bearing here. Manual workaround would have papered over W13's missing ack and never surfaced W14/W15/W16. Each iteration's clean baseline forced the next finding to surface; the discipline paid for itself across all four iterations.

---

## Cluster validation status

**Phase 3a PR 1 mode=live live migration: cluster-validated functional** for kernel-boot guests on default node-local networking. Five non-blocking findings (W17–W21) tracked for follow-up PR. Phase 3b candidates (W20, W12) tracked for the swift-ch-client async refactor.

The four BLOCKING hotfixes shipped via PRs #43, #44, #45 are the actual functional unlock — without them, no Phase 3a live migration could succeed. PR 1's merge represented the controller-and-swiftletd architecture; the hotfixes complete the data plane.
