# Tracked Follow-Up: Offline Migration Hangs for a Previously Live-Migrated Guest (Canonical Pod-Name Mismatch)

**Status**: RESOLVED 2026-05-29 via branch
`fix/tfu-18-offline-after-live-pod-name-trap` (Option A, full fix; see
Resolution below). Discovered during Phase 3b PR 2 walkthrough T8
(2026-05-28).

## Resolution (2026-05-29)

Fixed in two parts. The original writeup below scoped only "Bug 1"
(`preparing.go:133`); recon found that fixing it alone **relocates** the
hang, so a second fix was required.

**Bug 1 — offline Preparing pod lookup.** `preparing.go` now resolves
the source pod via `canonicalPodNameForGuest(&guest)` instead of literal
`guest.Name`. Fresh never-live-migrated guests still resolve to
`guest.Name`, so the Phase 1 offline path is unchanged. Contract test
added and verified load-bearing against the reverted fix.

**Bug 2 — SwiftGuest controller stale-PodRef self-heal (secondary trap,
recon-found).** The SwiftGuest controller always (re)creates the
launcher pod as `guest.Name` (pod.go) but looks it up via
`canonicalPodName` (`status.PodRef.Name` when set). `status :=
guest.Status.DeepCopy()` (controller.go) carries a stale PodRef, and
PodRef is only ever set/cleared in `MapPodToStatus` — the *found*-pod
branch. So after Bug 1's fix deletes the `-mig-` pod and offline
StopAndCopy flips `runPolicy=Running`, the controller recreates
`guest.Name` but the stale PodRef makes the next reconcile loop on
`Create` AlreadyExists, never updating status — hanging the migration's
Resuming phase. Fix: on the create branch, clear `status.PodRef` when
`staleMigrationPodRef` (PodRef points at a pod ≠ `guest.Name`). This
never fires during a healthy live migration (the dst `-mig-` pod exists
while PodRef points at it → found → update branch). It also fixes a
**broader latent bug**: any post-live-migrated guest whose launcher pod
is lost (node failure, eviction, manual delete) hit the same wedge.

**Audit finding — `stopandcopy.go:102` deliberately keeps `guest.Name`.**
It polls for the SwiftGuest-controller-*recreated* pod, which is always
created as `guest.Name`; switching it to `canonicalPodNameForGuest`
would look up the stale deleted name and break it. A naive "fix all
`guest.Name` pod lookups in the offline path" would have introduced a
new bug here. `resuming.go` (offline) reads SwiftGuest status, not a pod
by name — unaffected.

**Verification.** Bug 1: contract unit test (red against reverted fix).
Bug 2: decision helper (`staleMigrationPodRef`) unit-tested; end-to-end
wedge confirmed by cluster offline-after-live validation (live-migrate a
guest, then offline-migrate it → expect `Completed`, not hung) — the
SwiftGuest full-`Reconcile` path has no fake-client test harness in the
package, so cluster validation is the contract-level gate (C1).

**Not done here — offline `spec.timeout` floor.** The defense-in-depth
backstop suggested below is intentionally left out: Option A fixes the
root cause, and the no-default-timeout reality is tracked in
`kubeswift_context.md` Tracked Follow-up #22.

**Severity**: HIGH. Offline migration of any guest whose canonical
launcher pod was renamed by a prior live migration hangs indefinitely
in the Preparing phase. Not a data-loss bug — the source VM keeps
running and the guest recovers cleanly when the SwiftMigration is
deleted — but the migration never completes and the guest is left in
an inconsistent state (`spec.runPolicy=Stopped` while the launcher pod
is still Running).

**NOT a Phase 3b PR 2 regression.** The offline Preparing code
(`internal/controller/swiftmigration/preparing.go`) is Phase 1 code,
untouched by PR 2. The bug only manifests when a Phase 3a *live*
migration has previously renamed the guest's canonical pod. PR 2's
walkthrough T8 surfaced it because T8 reused a guest that T7 had just
live-migrated.

**Severity rationale**: offline is a primary, supported migration mode
(the only mode for VFIO/SR-IOV workloads, and the safe choice for
maintenance). "Live-migrate for load-balancing, then later
offline-migrate the same guest for maintenance" is a realistic
operator sequence. Hitting it leaves the migration wedged forever
(no `spec.timeout` default on the offline path observed here) and the
guest with a Stopped runPolicy but a running pod — confusing to
diagnose. Manual recovery is clean (delete the SwiftMigration), but an
operator has to know to do that.

## The bug

The offline Preparing phase resolves the source launcher pod by
`guest.Name`:

```go
// internal/controller/swiftmigration/preparing.go:133
getErr := r.Get(ctx, client.ObjectKey{Name: guest.Name, Namespace: guest.Namespace}, &pod)
```

After a *live* migration, the guest's canonical pod is renamed to the
`<guest>-mig-<short-uid>` form (the dst pod from the live cutover
becomes the guest's running pod; `status.podRef.name` is updated to
that name). On a subsequent *offline* migration:

Failure chain:
1. Guest was live-migrated earlier; its canonical pod is now e.g.
   `t78-guest-mig-511016`, and `status.podRef.name` reflects that.
2. Operator triggers an offline migration of the same guest.
3. Offline Preparing patches `spec.runPolicy=Stopped` (this succeeds)
   and then tries to delete the source pod via
   `r.Get(Name: guest.Name)` → looks up `t78-guest` → **NotFound**
   (the real pod is `t78-guest-mig-511016`).
4. The NotFound branch is taken; the controller concludes the pod is
   already gone and advances to the volume-detach wait.
5. The real pod `t78-guest-mig-511016` is never stopped, keeps the
   RWX+Block PVC attached, so `isPVCStillAttached` stays true forever.
6. Preparing parks at `waiting for volume detach (PVC
   "swiftguest-root-<guest>")` indefinitely. Guest left with
   `runPolicy=Stopped` but pod Running.

Note: the PVC name resolution at preparing.go:161
(`RootDiskCloneName(guest.Name)` → `swiftguest-root-<guest>`) is
*correct* — the PVC name is stable across migrations. The bug is
solely the pod lookup at line 133.

## Root cause — W26 canonical-pod-name lesson never applied to the offline path

This is the **W26 lesson (LBA-2 / `srcPodLookupName` invariant)
recurring in a code path that predates it.** Phase 3a's W26 fix
(PR #53) established that, after a migration cutover,
`status.podRef.name` points at the renamed pod, so src-pod resolution
must use the canonical/locked-in name — NOT literal `guest.Name`. That
fix was applied to the three *live*-mode src-pod lookup sites
(`stopandcopy_live.go`, `cutover.go`, `preparing_live.go`) via
`srcPodLookupName(mig, guest)` / `canonicalPodNameForGuest(guest)`.

The *offline* Preparing path (`preparing.go`) is Phase 1 code and was
never revisited — it still uses literal `guest.Name`. For a guest that
has only ever been offline-migrated this is correct (offline reuses
`guest.Name` as the recreated pod name — kubeswift_context "Approach A"
note). It breaks only after a live migration introduces the
`-mig-<uid>` canonical name.

The pattern, restated: **a load-bearing invariant (canonical pod-name
resolution) fixed in one code path but not its sibling. The two paths
share the same underlying assumption; fixing one and not the other
leaves a latent trap.** Same shape as the LBA-1/LBA-2/W26 family.

## Fix shape (NOT yet implemented)

Offline Preparing should resolve the source pod the same way the live
path does — via the canonical name, not `guest.Name`:

```go
// preparing.go:133, replace:
//   r.Get(ctx, client.ObjectKey{Name: guest.Name, ...}, &pod)
// with:
   r.Get(ctx, client.ObjectKey{Name: canonicalPodNameForGuest(&guest), ...}, &pod)
```

`canonicalPodNameForGuest(guest)` (in validating_live.go) returns
`guest.Status.PodRef.Name` when set, falling back to `guest.Name` — so
it is correct for both the fresh-guest offline case (returns
`guest.Name`) and the post-live-migration case (returns the
`-mig-<uid>` name). Likely small and localized (one call site; the PVC
name resolution at line 161 is already correct and needs no change).

**Audit the offline path for other `guest.Name`-based pod lookups**
while fixing this — any other site in `preparing.go` /
`stopandcopy.go` / `resuming.go` (offline) that resolves a pod by
`guest.Name` has the same latent trap.

**Consider an offline `spec.timeout` floor** as defense-in-depth: the
hang was indefinite because no timeout bounded the volume-detach wait.
Even with the pod-name fix, a backstop timeout would convert a future
"waiting for detach" stall into a clean Failed rather than an
indefinite hang.

## Test that must accompany the fix

Per the contract-level test discipline (TFU-2): a test exercising
offline migration of a guest whose canonical pod was renamed by a
prior live migration — i.e., a guest fixture with
`status.podRef.name = "<guest>-mig-<uid>"` (≠ `guest.Name`) and a
matching launcher pod, then assert offline Preparing finds and stops
that pod (not a NotFound on `guest.Name`). The existing offline
Preparing tests presumably use fresh guests whose pod name equals
`guest.Name` — that's the gap that let this ship.

## Reproduction (observed 2026-05-28, image sha-ed55768)

1. Create a fresh guest `t78-guest` on miles (pod name `t78-guest`).
2. Live-migrate it miles→boba (T7). Canonical pod becomes
   `t78-guest-mig-511016`; `status.podRef.name` updated.
3. Offline-migrate it boba→miles (T8, `mode=offline`).
4. Observe: hangs at `phaseDetail=waiting for volume detach (PVC
   "swiftguest-root-t78-guest")` for 240s+; `kubectl get pod t78-guest`
   → NotFound; `t78-guest-mig-511016` still Running; guest
   `runPolicy=Stopped` but pod up.
5. Control: a fresh guest `t8b-guest` (pod name `t8b-guest`, never
   live-migrated) offline-migrates cleanly — `Completed`,
   `observedDowntime=40.77s` — confirming the bug is specific to the
   post-live-migration canonical-pod-name mismatch, and that offline
   itself is not regressed.

## Manual recovery (for operators hitting this before the fix)

Delete the hung SwiftMigration:
`kubectl delete swiftmigration <name> -n <ns>`. The pre-cutover
cleanup path restores `spec.runPolicy=Running` (verified: recovered in
~3s, guest back to Running, unharmed). The guest stays on its current
node (the offline migration never moved it).

## Connection to tracked work

- **W26 / PR #53 / LBA-2 (`srcPodLookupName`)** — this is the same
  canonical-pod-name invariant; the fix should cite W26 and reuse
  `canonicalPodNameForGuest`.
- **TFU-2 (contract-level test discipline)** — the missing
  offline-after-live test is another instance of the
  isolated-tests-pass / cross-path-contract-drifts pattern.
- **Phase 1 offline migration (Approach A)** — the `guest.Name`
  assumption originates here; it was correct in a world with no live
  migration (no pod renaming) and became a latent trap once Phase 3a
  introduced canonical pod renaming.
