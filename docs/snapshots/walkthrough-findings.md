# Snapshot Walkthrough — Findings

> Findings surfaced during the operator-perspective walkthrough of
> Phases 0/1/2 snapshot functionality (PR #N). Categorized by class
> and severity. Operators reading the
> [walkthrough](operator-walkthrough.md) follow links here when a
> scenario reports an issue.

## Categories

- **Bug** — something doesn't work as documented or designed.
- **Doc gap** — docs don't cover something an operator needs.
- **UX issue** — works but is hard to use (confusing error message,
  missing convenience, etc.).
- **Design gap** — design doesn't account for something that surfaces
  only with operator-flow exercise.

## Severity

- **Blocking** — operators can't proceed.
- **Important** — operators succeed but with friction or wrong
  intuition.
- **Polish** — small experience improvements; safe to defer.

## Disposition

- **Fixed-in-PR-N** — a referenced PR addresses it.
- **Fix-in-walkthrough-PR** — small change applied alongside this
  walkthrough.
- **Follow-up** — separate PR proposed; not blocking the
  walkthrough.

---

## F1 (Bug, Blocking) — Tier A restore silently produces a fresh boot

**Surfaced in:** Scenario 1.

**What happened.** SwiftRestore reaches `Ready`, the restored
SwiftGuest reaches `Running`, but on SSH the disk content is a fresh
SwiftImage boot rather than the snapshot. Sentinels written before
snapshot are missing; machine-id is regenerated. Backup-and-restore
silently produces a fresh boot, not a restore.

**Root cause.** `EnsureRootDiskClone` in
`internal/controller/swiftguest/rootdisk.go` checked
`!IsControlledBy(pvc, guest)` BEFORE the `RestoreSeededLabel`
short-circuit. SwiftRestore creates the per-guest PVC owned by the
SwiftRestore (not by the SwiftGuest) so the orphan branch fired
first and deleted the snapshot-seeded PVC; the SwiftGuest controller
then created a fresh PVC owned by itself with no `dataSource` and
ran the Copy Job from the SwiftImage.

**Fixed in:** [PR #21](https://github.com/projectbeskar/kubeswift/pull/21).
Reordered the two checks so `RestoreSeededLabel` is consulted first.

**Why neither test caught it.**
- Unit test `TestRestore_HappyPath_CreatesPVCAndGuest` exercises
  `ensureRestorePVC` in isolation with a fake client; the fake
  client has no concurrent SwiftGuest controller to trigger the
  orphan-delete race.
- E2E test `test/snapshot/snapshot-test.sh:184–196` would have
  caught it (asserts the `restore-seeded` label and `dataSource`
  on the per-guest PVC) — but the e2e was never wired into CI.
  Fixed in [PR #22](https://github.com/projectbeskar/kubeswift/pull/22).

**Regression coverage added:** unit test
`TestEnsureRootDiskClone_RestoreSeededOwnedByRestoreNotDeleted`
in PR #21. Verified to FAIL without the fix and PASS with it.

---

## F2 (Doc gap, Important) — RBAC RoleBinding subject namespace must be patched

**Surfaced in:** Scenario 1 setup.

**What happened.** Following the docs literally
("apply RBAC to the namespace") leaves the launcher pod's swiftletd
spamming `pods is forbidden`. The SwiftGuest never reports
`status.network.primaryIP` because the lease poller can't patch the
pod annotation.

**Root cause.** `config/rbac/rolebinding.yaml` has
`subjects[0].namespace: default`. When `kubectl apply -k config/rbac
-n <other-ns>`, the binding is created in `<other-ns>` but its
subject still points at `default`'s ServiceAccount.

**Workaround:** patch after apply:
```bash
kubectl patch rolebinding swiftletd-reporter -n <other-ns> --type=json \
  -p '[{"op":"replace","path":"/subjects/0/namespace","value":"<other-ns>"}]'
```

`test/smoke/boot-test.sh` does this; the operator docs don't
mention it.

**Disposition:** Fix-in-walkthrough-PR (added to the walkthrough's
Setup section + a brief mention in `csi-snapshots.md` Prerequisites).
A cleaner fix — making the rolebinding namespace-template aware via
kustomize — is a follow-up.

---

## F3 (Bug, Important) — lease poller exits permanently after first patch failure

**Surfaced in:** Scenario 1 setup, before F2 was understood.

**What happened.** When swiftletd's first attempt to patch the pod
annotation fails (e.g. RBAC denied during the F2 window), the
lease poller's thread `return`s (rust/swiftletd/src/lease.rs:85)
and never retries. Even after the operator fixes the RBAC,
`status.network.primaryIP` stays empty until the launcher pod
restarts.

**Root cause.** `lease.rs` block-returns from inside the patch
attempt — success, failure, both exit the thread.

**Workaround:** delete the launcher pod
(`kubectl delete pod <guest> -n <ns> --grace-period=0 --force`).
The SwiftGuest controller recreates it; the new pod's lease poller
retries the patch with the now-correct RBAC.

**Disposition:** Follow-up. A small PR adds a retry/backoff loop
around the patch call. Marked Important, not Blocking — F2's
doc fix gets operators past this for new pods; existing pods
that already failed can be force-deleted.

---

## F4 (UX issue, Polish) — SwiftGuest transient phase=Failed during image import

**Surfaced in:** Scenario 1.

**What happened.** While the SwiftImage is `Importing`, a
SwiftGuest that references it shows `status.phase: Failed`.
Operators reading `kubectl get sg` see "Failed" and reasonably
assume something is wrong. Once the image becomes Ready, the guest
moves through `Scheduling → Running` normally.

**Root cause.** The resolver returns "image not Ready" as a
terminal-looking condition; the controller's phase mapping
treats unresolved as Failed rather than Pending.

**Disposition:** Follow-up. A small change to the resolver
condition mapping ("image still importing" → `phase: Pending` with
condition `Resolved=False reason=ImageImporting`) is correct and
non-breaking.

---

## F5 (UX issue, Polish) — `swiftctl snapshot describe` shows size in raw bytes

**Surfaced in:** Scenario 1.

**What happened.** The `Disk: role=root size=42949672960 ...` line
shows raw bytes; operators have to mentally convert (40 GiB).
The `swiftctl snapshot list` table column `SIZE` shows the same
raw value.

**Root cause.** Phase 5 (operator polish) is unshipped per
[`docs/design/snapshots.md`](../design/snapshots.md). The
size-formatting helpers don't exist yet.

**Disposition:** Fix-in-walkthrough-PR (small, contained — add
size formatting helper and use it in `describe` + `list`).

---

## F6 (Doc gap, Important) — csi-snapshots.md doesn't mention the PVC-bind gate

**Surfaced in:** Scenario 1.

**What happened.** The `Restoring → Resuming → Ready` transition
takes 30–90s on Longhorn (full-copy clone of the snapshot into the
per-guest PVC). [`csi-snapshots.md`](csi-snapshots.md)'s "Restore"
section is six lines and doesn't mention this wait. Operators see
"stuck in Restoring" and assume broken.

**Disposition:** Fix-in-walkthrough-PR (add a paragraph to
`csi-snapshots.md` "Restore a snapshot into a new VM" cross-linking
[`../images/clone-strategies.md`](../images/clone-strategies.md)'s
Storage class compatibility matrix; add a troubleshooting row).

---

## F7 (Doc gap, Important) — cloneStrategy: snapshot speedup is workload-shape dependent

**Surfaced in:** Scenario 2.

**What happened.** Following [`clone-strategies.md`](../images/clone-strategies.md)'s
description (≈3–10× speedup on snapshot-capable drivers), an operator
expects `cloneStrategy: snapshot` to be faster than `cloneStrategy:
copy`. On Longhorn with a 10 GiB SwiftImage source and a 40 GiB
SwiftGuestClass target, the snapshot strategy at single-guest scale
took **34 s longer** than the copy strategy (147 s vs 113 s) on a
parallel-applied test.

**Root cause.** The snapshot path has overhead the copy path doesn't:
1. Create PVC at the **source size** (10 GiB) with `dataSource:
   VolumeSnapshot` — Longhorn refuses different-size clones (Phase 0
   §5).
2. Wait for bind.
3. Expand the PVC to the target size (40 GiB).
4. Wait for `status.capacity == target` — Longhorn full-copy
   replication of the resized volume is non-instantaneous.
5. Schedule the launcher pod with a `clone-grow-init` init container
   that runs `qemu-img resize` + `sgdisk -e`.

The copy path does:
1. Create PVC at the **target size** directly.
2. Wait for bind.
3. Run a Copy Job that does `cp` + `qemu-img resize` + `sgdisk -e`.

When the source-to-target resize delta is small (Phase 0 spike used a
4 GiB source) the snapshot path's expand-and-wait is fast and the
overall path is faster. When the resize delta is large (10 → 40 GiB
in this walkthrough), expand-and-wait dominates.

**Where the snapshot strategy actually wins.** Pool scale (Scenario
3). Each cloning operation is independent on most CSI drivers, so
five concurrent `dataSource: VolumeSnapshot` clones don't queue
behind each other the way five concurrent Copy Jobs do.

**Disposition:** Fix-in-walkthrough-PR (add a "When NOT to use
snapshot strategy" note to `clone-strategies.md`). The existing
storage-class compatibility matrix shows the right speedup tiers for
copy-on-write drivers (Rook Ceph RBD, EBS, GCE PD) where the
expand-wait phase is near-instantaneous; the doc just needs to call
out that on full-copy drivers like Longhorn the speedup is fleet-
scale, not single-guest.

---

(Findings F8+ added as Scenarios 3–8 surface them.)
