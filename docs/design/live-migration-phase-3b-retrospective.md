# Live Migration Phase 3b — Retrospective

> Closes the Phase 3b arc: `mode: live` is shipped, cluster-validated, and
> documented. This retro records what shipped, the empirical results, the
> fix-forward chain the walkthroughs surfaced, the patterns that recurred,
> and the concrete changes to carry into Phase 3c+.
>
> Design doc: [`live-migration-phase-3b.md`](live-migration-phase-3b.md).
> Spike: `kubeswift_context.md` "Phase 3b Spike — COMPLETE".
> Last updated: 2026-05-29.

---

## 1. What shipped

| PR | Scope | Status |
|---|---|---|
| PR 1 (#61) | swiftletd send/receive state machines, progress-estimate emitter, `swift-ch-client` finalization, CRD rename `observedPauseWindow` → `observedTransferDuration` (alias kept) | Merged + manual-demo validated |
| PR 2 (#64, fix-forward #65, context #66, design #67) | Controller live-mode dispatch (Validating / PreparingLive / StopAndCopyLive / Resuming), auto-mode resolution, eligibility gate, cancel handshake + CancelIgnored gate, 7 new failure reasons | Merged + cluster-validated |
| PR 3 (#71) | swiftctl `migrate --preferred-mode`, `migration describe` Transfer + live progress + gloss, CRD `Transfer` printer column + list parity, operator runbook `docs/migration/phase-3b.md` | In flight |

Plus the **fix-forward chain** the PR 2 walkthrough surfaced (Section 3).

**Workload classes**: kernel-boot (`spec.kernelRef`) and disk-boot on
RWX+Block `longhorn-migratable` storage. VFIO/SR-IOV remain offline-only
(upstream CH #2251).

---

## 2. Empirical results (cluster-validated)

4 GiB guest, default Calico pod network, `cloud-hypervisor v51.1`:

| Workload | `observedTransferDuration` | `observedDowntime` |
|---|---|---|
| No stress (disk-boot RWX+Block, image `sha-0dc892f`) | 38.2s | 2.87s |
| stress-ng MED (2×256M) | ~68s | ~2.6s |
| stress-ng HIGH (50% RAM dirtied) | ~87s | ~2.6s |

The load-bearing UX property held: **downtime stays bounded ~2–3s** across
the whole workload range while transfer scales with the dirty rate. The
two metrics genuinely mean different things, which is why PR 3 surfaces
both with an inline gloss.

---

## 3. The fix-forward chain (PR 2 walkthrough → 3 PRs)

The PR 2 operator walkthrough surfaced two HIGH traps and one LOW, fixed
as a separate sequence rather than bundled into PR 2:

| Item | Bug | Fix | Validation |
|---|---|---|---|
| TFU #17 (#68) | vswiftimage webhook blocked finalizer removal on a being-deleted Ready SwiftImage → namespace stuck Terminating | `ValidateUpdate` early-allow when `deletionTimestamp != nil` | Cluster: Ready+deleting image sheds finalizer, GCs; control edit still rejected |
| TFU #18 (#69) | offline migration of a previously **live**-migrated guest hung — **two** bugs | Bug 1: offline Preparing resolves src pod by `canonicalPodNameForGuest`. Bug 2 (recon-found): SwiftGuest controller self-heals a stale `-mig-` PodRef on the create branch | Cluster: live miles→boba then offline boba→miles → Completed, guest Running |
| AlreadyExists (#70) | Bug 2's self-heal logged one spurious `AlreadyExists` ERROR per offline-after-live migration | tolerate `AlreadyExists` on launcher-pod create | Surfaced by #69 cluster run; bounds the race to a silent single retry |

**TFU #18 Bug 2 is the headline of this cycle**: fixing the documented bug
(Bug 1) alone would have *relocated* the hang from Preparing to a
StopAndCopy/Resuming AlreadyExists wedge. Recon caught it before the
partial fix shipped.

---

## 4. Patterns that recurred (and what to do about them)

### 4.1 "Finding-behind-a-finding" (W5) — restated again
TFU #18 Bug 2 is the ~7th instance in the project's history of a fix
revealing the next layer. **Mitigation that worked this cycle:** recon
*before* writing the fix, tracing every code path the fix touches. Bug 2
was found by tracing `status := guest.Status.DeepCopy()` + every PodRef
set/clear site, not by shipping Bug 1 and waiting for the walkthrough.

### 4.2 Discipline-applied-narrowly is the bug pattern
Both HIGH traps were an established discipline applied to one path but not
its sibling:
- **TFU #17** = PR #26 per-operation validation discipline (Design
  Principle #10) — adopted in the swiftmigration webhook, never in
  vswiftimage (which predated it).
- **TFU #18** = the W26/LBA-2 canonical-pod-name invariant — fixed in the
  live path, never applied to the Phase 1 offline path.

**Action:** when establishing a discipline, audit *all* sibling paths
immediately. The TFU #17 fix included a six-webhook audit (only
vswiftimage was vulnerable); the TFU #18 fix audited the offline path and
deliberately **left `stopandcopy.go:102` alone** (a naive "fix all
`guest.Name` lookups" would have broken it — that site correctly polls for
the recreated `guest.Name` pod).

### 4.3 Code is authoritative; design docs drift
PR 3 alone corrected three design-doc-vs-code divergences: the field is
`spec.mode` not `spec.preferredMode`; there is no `StopAndCopyLive` phase
constant (live reuses `StopAndCopy` + `status.mode`); there was no
`PauseWindow` printer column to remove. Earlier: `spec.allowVersionSkew`
never existed. **Action:** treat design-doc claims about repo state as
hypotheses to verify in code before acting on them. Recon does this; keep
it mandatory before every fix/feature PR.

### 4.4 No full-`Reconcile` test harness (TFU-2, again)
Both TFU #18 Bug 2 and the AlreadyExists fix live in the SwiftGuest
`Reconcile` create branch, which has **no fake-client harness** in the
package — so neither could be unit-tested end-to-end; both relied on
cluster validation. The mitigation where a seam existed was to extract
**pure, testable functions**: `staleMigrationPodRef`, `parsePreferredMode`,
`renderMigrationDescribe`. This is TFU-2 ("operator-flow validation in test
infrastructure") restated for the Nth time and is now the highest-value
testing investment — see Section 6.

### 4.5 Cluster validation catches what unit tests can't
The single-vs-infinite `AlreadyExists` distinction, the offline-after-live
end-to-end completion, and the controller-gen version-marker churn were all
cluster/tooling observations, not unit-test findings. The per-phase
mini-walkthrough discipline (feedback_phase_validation) continues to pay
off; keep it.

---

## 5. What recon-first caught this cycle (concrete)

- **TFU #18 Bug 2** — the secondary stale-PodRef trap; would have shipped a
  fix that relocates the hang.
- **`stopandcopy.go:102` must NOT change** — the audit obligation's naive
  reading would have introduced a new bug.
- **vswiftimage mechanism** — the real cause was a pointer-identity `!=`
  comparison on `ImageSource`, not "rejects all updates"; the known-issues
  doc's "Approach 2" wording was misleading and would have left the bug if
  followed literally.
- **controller-gen drift** — `make generate` with a local v0.21.0 churned
  all 11 CRD version markers; reverted and regenerated with the repo
  baseline v0.20.1 to keep PR 3 scoped.

---

## 6. Recommendations for Phase 3c+

1. **Land TFU-2** — a minimal full-`Reconcile` fake-client harness for the
   swiftguest + swiftmigration controllers. This cycle produced two
   cluster-only-verifiable controller fixes; a harness would have unit-
   confirmed both. Highest-leverage testing investment.
2. **Pin controller-gen in the Makefile** (e.g.
   `go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1`) so
   `make generate` is reproducible regardless of the local install. New
   tracked follow-up.
3. **Keep recon-first mandatory** before every fix/feature PR — it caught
   four concrete problems this cycle (Section 5).
4. **Audit sibling paths on every new discipline** — make it a checklist
   item, not a hope. Both HIGH traps were discipline-applied-narrowly.
5. **Decide the `observedPauseWindow` deprecation removal** (Phase 3b+1):
   the alias is still populated; pick the release that drops it.

---

## 7. Deferred / next (live-migration roadmap)

- **Phase 3c — mTLS migration channel** (today: plaintext + the
  `migration-phase2-unsafe-plaintext: ack` gate).
- **Phase 4 — drain integration** (eviction webhook → auto-migrate on
  `kubectl drain`).
- **Phase 5 — operational polish** (Prometheus metrics, dashboards,
  retention, surfacing swiftletd progress in more places).
- **Cross-cutting follow-ups**: W28 (actual vCPU stop-the-world metric),
  TFU #1 (multi-node L2 for IP preservation), TFU #10 (`swiftctl migrate
  --check` CPU-feature pre-flight), TFU #14 (source-side cancel-during-send
  wedge / worker-thread refactor), TFU #22 (`spec.timeout` default),
  controller-gen pin (new).
- **Permanent constraint**: VFIO/SR-IOV cross-node stays offline-only until
  upstream CH #2251 changes.
