# Tracked Follow-Up: vswiftimage Webhook Traps SwiftImage Deletion (Finalizer Removal Blocked)

**Status**: RESOLVED 2026-05-29 via branch `fix/tfu-17-vswiftimage-finalizer-trap`
(Approach 1; see Resolution below). Discovered during PR 2 walkthrough
namespace cleanup (2026-05-28).

## Resolution (2026-05-29)

Fixed via **Approach 1**: `vswiftimage` `ValidateUpdate` now returns
early (allow) when `newObj.GetDeletionTimestamp() != nil`. A being-
deleted object shedding finalizers is never performing a spec mutation,
so the immutability rule must not gate it. Mirrors PR #26's
`ValidateDelete` pass-through intent (Design Principle #10). Regression
tests added (contract test + over-allow guard) — see "Test that must
accompany the fix" below, now satisfied.

**Mechanism correction:** the diagnosis below ("it treats ANY update as
a spec-mutation attempt") describes the *symptom*, not the mechanism.
The rule *does* compare old vs. new spec fields — but
`oldImg.Spec.Source != img.Spec.Source` is a **pointer-identity**
comparison over `ImageSource`'s pointer fields (`HTTP`/`Upload`/
`PVCClone`). The old (etcd) and new (admission request) objects are
independent decodes, so their Source pointers always differ and the
rule fires unconditionally on every Ready-image update. The `Format`
half (a string) compares by content and is correct. This matters for
the fix: Approach 1 carves out being-deleted objects regardless of the
comparison; a literal reading of Approach 2 ("compare only spec fields")
is *already* what the buggy code does — a correct Approach 2 needs
**content/deep equality**, not just field comparison.

**Sibling-webhook audit (obligation discharged):** of the six validating
webhooks (swiftguest, swiftimage, swiftseedprofile, swiftsnapshot,
swiftrestore, swiftmigration), **only vswiftimage was vulnerable**. The
others are immune because their immutability checks use content/deep
comparison (`specsEqual` / `identityEqual` / value-struct `!=`) or have
no immutability rule at all — *not* because they check `deletionTimestamp`
(none do). Cross-referenced with finalizer-adding sites: vulnerable
webhook ∩ finalizer-bearing resource = SwiftImage only. (swiftguestpool
has no webhook.)

**Residual deferred:** Approach 1 does not fix the pointer-comparison
defect for *non-deletion* metadata edits (e.g. relabeling a Ready image
still trips the false rejection). Filed as Tracked Follow-up #23 in
`kubeswift_context.md` (LOW severity).

**Severity**: MEDIUM-HIGH. Not a data-loss bug, but it permanently
traps namespace deletion and generates a continuous controller
reconcile-error storm. Any SwiftImage that reaches
`status.phase=Ready`, acquires a finalizer, and is then deleted will
trap its namespace in Terminating forever until manually unstuck.

**Severity rationale**: it does not corrupt or lose live data, but it
is operationally severe — a stuck-terminating namespace blocks
namespace reuse, the error storm pollutes controller logs (masking
real errors), and the manual recovery requires temporarily removing a
webhook from the cluster (a privileged, scary operation for an
operator to perform under pressure). Left unfixed, every snapshot/pool
walkthrough that creates a clone-seed-protected SwiftImage and tears
down its namespace will hit this.

## The bug

The `vswiftimage` validating webhook enforces a spec-immutability rule
("SwiftImage spec is immutable") when `status.phase=Ready`. It fires
this rule on **all** UPDATE operations — including finalizer removal
on an object that already has `metadata.deletionTimestamp` set.

Failure chain:
1. A SwiftImage reaches `status.phase=Ready` and carries the
   `kubeswift.io/clone-seed-protected` finalizer (added for clone-seed
   protection in snapshot Phase 1 cloneStrategy work).
2. The SwiftImage is deleted (`deletionTimestamp` set), e.g. by
   namespace deletion.
3. The controller attempts to remove the finalizer — a metadata-only
   UPDATE — so GC can proceed.
4. The `vswiftimage` webhook rejects the UPDATE because
   `status.phase=Ready` and it treats ANY update as a spec-mutation
   attempt. The finalizer-removal patch is denied.
5. Finalizer never clears → object never GC's → namespace stuck
   Terminating forever → continuous reconcile-error storm in
   controller logs.

## Root cause — same family as PR #26

This is the **PR #26 per-operation-discipline lesson recurring in a
webhook that predates that discipline.** Design Principle #10 ("treat
terminal states as terminal; validation logic must enumerate which
operations it fires on; default-to-everything is the bug pattern") was
written for exactly this. The SwiftMigration webhook was refactored to
per-operation discipline (ValidateCreate full / ValidateUpdate
shape-only / ValidateDelete pass-through) in PR #26. The `vswiftimage`
webhook never got the same treatment — its spec-immutability rule
fires on every UPDATE regardless of whether the update is a legitimate
spec mutation or a being-deleted object shedding finalizers.

The pattern, restated: **validation that fires on every operation
needs to consider whether each operation is one where validation adds
value vs. blocks legitimate work.** Finalizer removal on a
being-deleted object is never a spec mutation; blocking it adds no
protection and traps deletion.

## Fix shape (Approach 1 IMPLEMENTED 2026-05-29)

Two acceptable approaches were available; Approach 1 shipped:

1. **Skip validation when being deleted** (simplest):
   In the `vswiftimage` ValidateUpdate handler, return early (allow)
   when `newObj.metadata.deletionTimestamp != nil`. A being-deleted
   object's spec changes are moot, and finalizer removal must be
   permitted for GC.

2. **Compare only spec fields, allow metadata/finalizer changes**:
   The immutability rule should compare `old.Spec` vs `new.Spec` and
   reject only genuine spec mutations, allowing metadata-only changes
   (finalizers, labels, annotations) through regardless of phase.

Approach 1 is the minimal fix and directly mirrors the PR #26
ValidateDelete pass-through intent. Approach 2 is more thorough (also
allows label/annotation edits on a Ready image, which may or may not
be desirable). Recommend Approach 1 unless there's a reason to allow
metadata edits on Ready images generally.

**Audit the OTHER webhooks for the same bug class while fixing this.**
Any validating webhook with a phase-gated immutability rule that fires
on UPDATE is a candidate. Known per-operation-disciplined: swiftmigration
(PR #26). Suspect (predates discipline): vswiftimage (this bug), and
potentially others — swiftsnapshot, swiftrestore, swiftguest,
swiftguestpool webhooks should each be checked for "fires on all
UPDATE including finalizer removal on being-deleted objects."

## Test that must accompany the fix (DONE 2026-05-29)

Per the contract-level test discipline (TFU-2): a test that creates a
Ready SwiftImage with a finalizer, sets deletionTimestamp, attempts
finalizer removal, and asserts the webhook ALLOWS it. The existing
webhook tests asserted spec-immutability for the *cloneStrategy* rule
but never set `phase=Ready` on an update — so the Source-immutability
path (the buggy one) was entirely uncovered. That gap let this ship.

Shipped in `validator_test.go`:

- `TestValidateUpdate_FinalizerRemovalOnDeletingReadyImage_Allowed` —
  the contract test. Verified load-bearing: it fails against
  guard-stripped code with the exact "spec is immutable" rejection and
  passes with the guard. `old`/`new` are built via separate `makeImage`
  calls so their `Spec.Source` pointers differ as in real admission.
- `TestValidateUpdate_SpecMutationOnReadyImage_StillRejected` —
  over-allow guard: a genuine spec change on a Ready image that is NOT
  being deleted is still rejected (passes with or without the carve-out,
  confirming it exercises the immutability rule itself, not the guard).

## Manual recovery procedure (for operators hitting this before the fix)

This was performed 2026-05-28 to clear snapshots-wt-s2 and
snapshots-wt-s3. All steps reversible, all verified:

1. Back up the validating webhook config:
   `kubectl get validatingwebhookconfiguration kubeswift-validating-webhook -o yaml > /tmp/vwc-backup.yaml`
2. Temporarily remove ONLY the `vswiftimage` webhook entry from the
   ValidatingWebhookConfiguration (it was index 1; verify before
   editing).
3. Strip the stuck finalizer from the trapped SwiftImage(s):
   `kubectl patch swiftimage <name> -n <ns> --type=json -p '[{"op":"remove","path":"/metadata/finalizers/0"}]'`
   (verify the finalizer index; clone-seed-protected was the one)
4. Objects GC; namespaces complete deletion.
5. Restore the `vswiftimage` webhook from backup (re-add the entry in
   original order with original kubeswift-webhook-service config).
   Verify serving (denial count returns, error storm stops).

**Risk during recovery**: the webhook-down window allows spec
mutations on Ready SwiftImages cluster-wide. Keep the window short.
Note: during the 2026-05-28 recovery, two OTHER SwiftImages already
stuck deletion-pending on the identical bug (default/ubuntu-noble-pool,
default/ubuntu-noble-snap, plus orphaned ubuntu-noble-pool-clone-seed
VolumeSnapshot) also cleared — benign, they were already
deletion-marked and trapped by the same issue.

## Connection to tracked work

- **PR #26 per-operation-discipline** — this is the same lesson; the
  fix should explicitly cite Design Principle #10.
- **TFU-2 (contract-level test discipline)** — the missing
  finalizer-removal-on-Ready test is another instance of the
  isolated-tests-pass-contract-drifts pattern. Sixth data point if
  counted alongside snapshot Tier A, W27 metrics, Finding 1, Finding 2,
  fail:224.
- **Snapshot Phase 1 cloneStrategy** — the clone-seed-protected
  finalizer comes from this work; the webhook interaction was never
  exercised in the snapshot walkthrough because no clone-seed-protected
  image was deleted during it.