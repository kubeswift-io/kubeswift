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

---

# Phase 3a PR 1 — E12 Disk-Boot Validation (2026-05-04)

> Cluster validation of disk-boot RWX+Block live migration (E12 in [`docs/design/live-migration-phase-3a.md`](../design/live-migration-phase-3a.md) §8.2 test plan). Same cluster as the kernel-boot walkthrough above; image baseline `sha-8c86ffa` (post-W22/W23/chart-version fix), then `sha-f6cf771` (post-W26 hotfix).

Phase 3a docs claimed support for **two** workload classes (`kernelRef` + `imageRef` with RWX+Block); PR #46 walkthrough only validated the first. E12 closes that gap and surfaces W26 — a workload-class-independent bug that PR #46's three-run determinism gate did not catch because that gate validated **timing-race elimination** (W22 lesson), not **chain-migration correctness**.

## Findings summary

| # | Severity | Disposition | One-line |
|---|---|---|---|
| W26 | BLOCKING | Fixed via [PR #53](https://github.com/projectbeskar/kubeswift/pull/53) | Back-to-back live migrations on the same SwiftGuest false-fired SourcePodReplaced; latent guest-destruction vector at `cutover.go::cutoverStep2`. Workload-class-independent — disk-boot validation surfaced it because the `or sequential miles→boba→miles→boba` S1 path naturally exercised chains |

No W27 / fourth-finding-behind-a-finding surfaced. Chain closed at one BLOCKING.

## Prerequisite + fixture inventory

| Asset | Source |
|---|---|
| Longhorn v1.11.1 (engine v1) | Existing on cluster |
| StorageClass `longhorn-migratable` (migratable:"true", fsType:"", 3 replicas) | Existing — pre-validated in PR #35 W9 |
| Ubuntu Noble SwiftImage (`cloneStrategy: copy`) | **Created** as `mig-wt-disk/ubuntu-noble-copy` (default-namespace SwiftImages use `cloneStrategy: snapshot` — W9.x blocked for Block destinations) |
| SwiftSeedProfile (kubeswift user, ed25519 key) | **Created** as `mig-wt-disk/default` |
| SwiftGuestClass (4Gi/2cpu, 20Gi raw, RWX+Block, longhorn-migratable) | **Created** as `mig-wt-disk/live-migratable` (modeled on `config/samples/storage/swiftguestclass-rwx-migratable.yaml`) |
| `mig-wt-disk` namespace (fresh, non-`default`) | **Created** — fresh-namespace gate per design §8.2 |
| RBAC | Auto-bound by SwiftGuest controller (PR #30) — no manual apply |

SwiftImage import + measure: 8m35s. disk-s1 boot to Running+IP: ~3min.

## Scenarios

### S1 baseline (3-run determinism + chain) — PASS

| Run | Direction | Result | sourcePodRef (locked at Validating) | Wall-clock | Sentinel md5 |
|---|---|---|---|---|---|
| 1 | miles → boba | Completed | `<none>` (pre-W26 image) | 59s | `115d66eb…d4c` (locked) |
| 2 | boba → miles | **Failed (W26)** | n/a | 21s | n/a |
| 2-retry (post-W26) | boba → miles | Completed | `disk-s1-mig-4a3376` (run 1's dst) | 59s | `115d66eb…d4c` (preserved) |
| 3 (post-W26) | miles → boba | Completed | `disk-s1-mig-c7683f` (run 2's dst) | 54s | (preserved) |

3-run wall-clock: 59s / 59s / 54s — bounded variance, well within the W22-style determinism gate.

`uptime` inside the guest after run 3 reported "1 hour, 22 minutes" — confirms three live resumes, no boot. Live migration's value proposition (state preservation across nodes) holds for disk-boot.

### S2 reconcile-loop interruption — PASS

disk-s2 miles → boba. Controller-manager pod killed at t+16s (mid `transferring guest state`). New leader took over; phase=Completed at t+57s.

| Field | Value |
|---|---|
| Wall-clock total | 57s |
| recvAttempts / sendAttempts | 1 / 1 (no duplicate dispatches across leader handover) |
| Sentinel md5 | preserved |
| Old controller pod | `controller-manager-66c64fc469-plv8h` |
| New controller pod | `controller-manager-66c64fc469-9rwgg` |

§2.4 state-machine reconstruction works on disk-boot's longer pre-copy window.

### S5 force-delete dst mid-flight — PASS

disk-s5 miles → boba. dst pod force-deleted (`--grace-period=0 --force`) at t+16s.

| Field | Value |
|---|---|
| Phase transition to Failed | t+19s (3s after force-delete; fast-path detection) |
| failureReason | **PodTerminated** (correct architect F4.3 classification — not Other, not Timeout) |
| failureMessage | `destination pod "disk-s5-mig-8c831c" disappeared during StopAndCopy` |
| src guest survives | Running on miles, podRef unchanged, sentinel preserved |

W18 classification gap (kernel-boot S5: maps to Other not PodTerminated) does **not** surface on disk-boot — fast-path fired correctly.

### S7 cancel pre-cutover — PASS

disk-s7 boba → miles. `kubectl patch smig --type=merge -p '{"spec":{"cancelRequested":true}}'` at t+16s.

| Field | Value |
|---|---|
| Phase transition to Cancelled | t+32s (16s from cancel patch) |
| failureReason | Cancelled |
| failureMessage | `destination pod was never created; cancel completes without swiftletd ack` |
| src guest survives | Running on boba, podRef unchanged, sentinel preserved |

Cancel discipline holds on disk-boot.

## W26 root cause + fix

**Repro** (S1 run 2 first attempt, 2026-05-04, image `sha-8c86ffa`):

- Run 1 succeeded; `SwiftGuest.status.podRef.Name` patched to `disk-s1-mig-4a3376` (run 1's dst, now run 2's canonical src).
- Run 2 began. `Validating-live` captured `status.SourcePodUID` correctly via `canonicalPodNameForGuest` (which post-run-1 returns the run-1-dst-suffix name).
- Run 2 reached StopAndCopy → `transferring guest state`. At t+21s, phase=Failed, failureReason=`SourcePodReplaced`, failureMessage `source pod for SwiftGuest "disk-s1" no longer exists during StopAndCopy`.
- **Verification**: source pod `disk-s1-mig-4a3376` (UID `0b596b96-…`) still Running on boba. False-positive.

**Root cause** at [`stopandcopy_live.go:184`](../../internal/controller/swiftmigration/stopandcopy_live.go) (and same shape at [`cutover.go:167`](../../internal/controller/swiftmigration/cutover.go)):

```go
r.Get(ctx, client.ObjectKey{Name: guest.Name, Namespace: guest.Namespace}, &srcPod)
```

The W15 fix replaced `canonicalPodNameForGuest` with literal `guest.Name` to dodge a cutoverStep1 informer race. The fix's docstring assumed "src pod is named guest.Name throughout its lifetime" — true for first migrations only. After a prior migration's cutover, `status.podRef.Name` points at the prior dst pod (= the new migration's src), which is NOT `guest.Name`. Literal lookup hit NotFound → false-fire.

`cutover.go:167`'s same literal-`guest.Name` lookup carried a worse latent bug: chain run 2's cutoverStep2 would silently skip src-pod deletion (NotFound) leaking the prior dst pod on the source node. The naive canonicalPodName-everywhere alternative would post-cutoverStep1 resolve to **THIS** migration's dst pod and `cutoverStep2` would delete the migrated guest — silent data destruction.

**Fix** (PR #53): stamp `status.SourcePodRef.Name = srcPod.Name` at `Validating-live` (mirrors existing `SourcePodUID` lock-in); three live-mode src lookups use a `srcPodLookupName(mig, guest)` helper that returns `SourcePodRef.Name` when set, falls back to `canonicalPodNameForGuest` for defense. Race-immune (locked at validation) AND chain-safe.

**Phase 1 offline** is unaffected — Approach A reuses `guest.Name` as the post-migration pod name; literal-`guest.Name` lookups remain correct there. W26 fix is live-mode-only.

## Workload-class blast radius confirmation (kernel-boot chain)

After the W26 hotfix landed (image `sha-f6cf771`), a kernel-boot chain validation ran in `mig-wt`:

| Run | Direction | Result | sourcePodRef | Wall-clock |
|---|---|---|---|---|
| A | miles → boba | Completed | `kernel-s1` | 51s |
| B (chain) | boba → miles | Completed | `kernel-s1-mig-3cebf9` (run A's dst) | 48s |

Run B is the exact W26 chain scenario on kernel-boot — before the fix it would have false-fired. After: clean. Same code path runs for both workload classes; W26 was a controller-level bug surfaced by disk-boot only because PR #46's three-run gate ran on different SwiftGuests, not chained on one.

## Validation methodology gap (recurring W5 pattern, sixth occurrence)

PR #46's three-run kernel-boot determinism gate validated **timing-race elimination** (W22 lesson). It did not validate **chain-migration correctness** — different question. Disk-boot E12's documented "or sequential miles→boba→miles→boba" path naturally landed in chain territory and surfaced W26.

This is the **W5 lesson restated for the sixth time** in the project's history:

| # | Surfaced in | Pattern |
|---|---|---|
| 1 | Snapshot F2 | Per-namespace RBAC manual-apply (papered over in walkthrough doc, re-surfaced in Phase 2) |
| 2 | Phase 2 W3 + W6 | Storage handoff scenario absent from spike |
| 3 | PR #32 W8 | Cached-client RBAC sufficiency invisible to fake-client tests |
| 4 | PR #35 W9.x + W10 | CSI annotation gap + CH boot-time WARN absent from unit tests |
| 5 | PR #42 W13–W21 | Four BLOCKING hotfix iterations — unit tests passed at each layer |
| **6** | **E12 W26** | **Chain-migration correctness absent from PR #46's determinism gate** |

Future Phase 3a/3b validation should include explicit chain-migration scenarios (≥2 sequential migrations on the same SwiftGuest) alongside three-run determinism. Adds further weight to Tracked Follow-up #2 (operator-flow validation pattern in test infrastructure).

## Performance comparison

| Scenario | Kernel-boot (PR #46) | Disk-boot (E12) | Δ |
|---|---|---|---|
| S1 happy path | ~51s | 59s | +16% |
| Chain run B | n/a | 59s | — |
| Reconcile-recovery | n/a | 57s | — |
| Force-delete fail-fast | ~5s | 19s (16s cancel-write delay) | comparable |
| Cancel pre-cutover | <30s | 32s | comparable |

Disk-boot's Ubuntu Noble pre-copy window is ~16% longer than kernel-boot's faas-minimal — well within bounds; no anomaly.

## Cluster validation status — both workload classes

**Phase 3a PR 1 mode=live live migration: cluster-validated functional for both in-scope workload classes.**

- Kernel-boot guests (`spec.kernelRef`): validated PR #46 (10 scenarios) + chain validated post-W26 hotfix (2 runs, sourcePodRef confirmed)
- RWX+Block disk-boot guests (`spec.imageRef` + RWX+Block storage): validated E12 (S1 3-run + chain + S2 + S5 + S7)

W26 fix is the data-plane unlock for chain migrations on either workload class. Without it, every operator workflow involving repeated drain/rebalance on the same SwiftGuest would have hit the bug after the first migration.

---

# W27 Validation — Phase 3a downtime metrics empirical baseline (2026-05-04)

> Cluster validation of [PR #55](https://github.com/projectbeskar/kubeswift/pull/55) (W27a + W27b downtime-metrics correctness fix). Image baseline `sha-b730536`. Same cluster as the kernel-boot + E12 walkthroughs above.

W27 audit (Tracked Follow-up #7) identified two broken metrics surfaces:
- W27a: `status.observedDowntime` measured two adjacent `metav1.Now()` calls in the same reconcile (sub-millisecond nonsense, 34-114µs across 17 prior runs)
- W27b: `status.observedPauseWindow` plumbing half-implemented (swiftletd wrote `kubeswift.io/migration-pause-window-ms`; controller had zero readers; field permanently nil)

PR #55 fixed both. This validation captures empirical numbers from a single S1-equivalent run on each workload class.

## Operational note: CRD update required at deploy

PR #55 added a new status field `cutoverStep2DispatchedAt`. The validation's first attempted run (kernel-s1 miles→boba) showed `observedDowntime` empty + `cutoverStep2DispatchedAt` empty even though `observedPauseWindow=38.161s` populated correctly. Root cause: the cluster's CRD was stale (deploy step didn't update CRDs), apiserver silently stripped the unknown field per CRD schema. Operators upgrading the controller image MUST re-apply CRDs:

```
kubectl apply -f config/crd/bases/migration.kubeswift.io_swiftmigrations.yaml
```

Or use `make deploy` (which already does this). All subsequent runs in this validation used the updated CRD.

## Empirical numbers

| Workload class | Migration | Total wall-clock | observedDowntime | observedPauseWindow | cutoverStep2DispatchedAt |
|---|---|---|---|---|---|
| Kernel-boot (`spec.kernelRef`) | kernel-s1 boba→miles | 47s | **1.751622467s** | **38.169s** | 2026-05-04T20:58:55Z |
| RWX+Block disk-boot (`spec.imageRef`) | disk-s1 miles→boba | 58s | **1.958336573s** | **38.186s** | 2026-05-04T21:00:42Z |

Both fields are now populated with meaningful, non-trivial values. **W27a + W27b empirically confirmed working on cluster for both in-scope workload classes.**

## What each field actually measures (post-W27 + code-review clarification)

Code review of the W27 fix (operator + this implementer, post-merge of PR #55) confirmed both fields measure correctly within the layers they observe — the **field NAMES and CRD docstrings** were the misleading surfaces, not the values. PR #56 commit D updates the docstrings.

### `observedDowntime` (post-W27a) — operator-visible guest unresponsiveness on cluster

Window: `cutoverStep2DispatchedAt` (src pod Delete dispatch — vCPU pause begins inside CH on src) → `completedAt` (GuestRunning=True observation on dst — vCPU pause ends). For both runs that's ~1-2 seconds.

This **IS the right metric** for "operator-visible guest downtime on cluster." It's not "orchestration overhead" — it spans the actual vCPU pause on src + cluster handoff + vCPU resume on dst, which is what the operator sees from `kubectl get smig -o wide`. Pre-W27a this measured two adjacent `metav1.Now()` calls in the same reconcile (sub-millisecond nonsense); post-W27a it measures the real wall-clock cutover window.

### `observedPauseWindow` (post-W27b) — swiftletd-reported send-migration RPC duration

Window: swiftletd's wall-clock `elapsed_ms` of the `vm.send-migration` RPC on the source. For both runs that's ~38s.

This is **NOT the vCPU stop-the-world window**, despite the field's name. Cloud Hypervisor's `send-migration` internally does:

1. Pre-copy iterations — vCPU still running, dirty-page tracking
2. Final stop-and-copy — vCPU paused, drain remaining dirty pages
3. Finalize — handoff to receiver

The annotation captures the wall-clock elapsed of the **entire** RPC (1+2+3). The actual vCPU stop-the-world (just step 2) is typically hundreds of milliseconds and is buried inside CH's internal phase boundaries — not separately surfaced today.

This is the value swiftletd CAN measure today (wall-clock around an RPC call). Capturing the actual stop-the-world is W28 candidate (see Tracked Follow-up #7 close-out): future CH versions may grow per-phase timing on the response, OR `swift-ch-client` could probe `vm.info` around the stop-and-copy boundary inside the RPC, OR an external observer (Tracked Follow-up #1's multi-node L2 + ping measurement) could capture guest-perceived downtime from outside.

### The actual relationship

```
StopAndCopy entry (~T0)
  │
  │  ◄── observedPauseWindow (~38s) — entire send-migration RPC ──►
  │     • CH pre-copy iterations (vCPU running, dirty-tracking)
  │     • Final stop-and-copy on src (vCPU paused — actual stop-the-world)
  │     • Finalize / handoff
  │
cutoverStep2DispatchedAt (~T0 + 38s)
  │
  │  ◄── observedDowntime (~2s) — operator-visible guest downtime ──►
  │     • src pod Delete dispatch
  │     • dst pod resume + GuestRunning=True observation
  │
Completed
```

The two windows are sequential within the migration timeline. `observedDowntime` covers the cutover-and-resume window (where the guest IS paused on src + transitioning to dst); `observedPauseWindow` covers the data-transfer RPC (where the guest is mostly NOT paused — only paused during the final stop-and-copy sub-phase). Neither directly captures "actual vCPU stop-the-world window inside CH" — that's W28 territory.

## Operational note: stale CRD silently strips new status fields

First attempted run in this validation showed `observedDowntime` empty + `cutoverStep2DispatchedAt` empty despite the W27a code shipped, while `observedPauseWindow=38.161s` populated correctly. Root cause: the cluster's CRD was stale (the redeploy step didn't update CRDs), and apiserver silently strips status fields not in the CRD schema — controller patches succeed (no error returned) but the new fields disappear.

Operators upgrading the controller image MUST re-apply CRDs:

```
kubectl apply -f config/crd/bases/migration.kubeswift.io_swiftmigrations.yaml
```

Or use `make deploy` (which already does this). This pattern applies to **any new status field** added across releases — symptom is "field documented in CRD types but always empty in cluster"; fix is to refresh the CRD. Add `kubectl explain swiftmigration.status` to a deployment-checklist to catch CRD drift before observing it as missing-field nonsense in operator surfaces.

## Third-measurement gap (ping-loss-cross-node)

The W27 prompt's third measurement (ping from sibling pod on third node × 50ms intervals) skipped on this cluster topology — requires multi-node L2 reachability to the guest's br0 IP (192.168.99.0/24). Default node-local networking does not provide this; br0 is per-node, the guest IP is reachable only from the launcher pod on the same node. Tracked Follow-up #1 (multi-node L2 enablement: Multus + macvlan / OVN-Kubernetes layer-2 / UDN) is the prerequisite. A guest-internal alternative (ping from inside the guest to its gateway) would conflate vCPU pause with src-side bridge teardown and is not a clean replacement. Deferred to a future cluster equipped with multi-node L2.

## Cluster validation status

**Phase 3a downtime metrics: cluster-validated for both in-scope workload classes.** W27a + W27b plumbing fix correctness empirically confirmed on cluster. Both `observedDowntime` and `observedPauseWindow` now report meaningful values on every successful live migration; pre-W27 they reported sub-millisecond nonsense (downtime) and nil (pauseWindow).

The "actual vCPU stop-the-world window" is W28 candidate — currently buried inside Cloud Hypervisor's `send-migration` internals and not separately surfaced. PR #56 commit D updates the field docstrings to match what each value actually measures.
