# Snapshot Phase 4 — `cloneFromSnapshot` ergonomics

> Status: DESIGN (pre-spike). Anchors on the Scenario 7 demand
> ([`docs/snapshots/operator-walkthrough.md` §Scenario 7](../snapshots/operator-walkthrough.md))
> and the Phase 3 scoping note
> ([`docs/design/snapshot-phase-3-s3.md` §12](snapshot-phase-3-s3.md)).
> Last updated: 2026-06-04.

## 1. Goal

Spin up **N VMs all cloned from one SwiftSnapshot** — fast (clone-from-snapshot
is seconds; boot-from-cloud-image is minutes) and ergonomically (the pool
controller manages the replicas; the operator does not hand-maintain N
SwiftRestore CRs).

Concretely, two API surfaces (Scenario 7's documented gap):

1. **`SwiftGuest.spec.cloneFromSnapshot`** — a SwiftGuest boots as a clone of a
   referenced SwiftSnapshot, instead of `imageRef` / `kernelRef`.
2. **A SwiftGuestPool template carrying `cloneFromSnapshot`** — each replica
   becomes an independent clone of the snapshot, with per-replica identity.

## 2. What already exists (and what Phase 4 reuses)

Phase 4 is **controller ergonomics over the Phase 1/2/3 restore primitives** —
it adds almost no new runtime mechanism. The pieces already shipped:

| Primitive | Where | Phase 4 use |
|---|---|---|
| Restore-receive launcher (CH `--restore` from a node-local snapshot dir) | `swiftguest/restore.go::BuildRestorePod`, driven by `snapshot.kubeswift.io/*` annotations on the guest | **Identical** — a cloneFromSnapshot guest launches exactly this way |
| Per-clone identity regen (deterministic MAC rewrites, cmdline marker → seed bootcmd regenerates machine-id/hostname/SSH keys on first reboot) | `swiftrestore/local.go::restoreAnnotations` + the snapshot-stager | **Identical per replica** — each replica is a distinct clone |
| s3 download Job (pull + sha256-verify artifacts to a node-local cache) | `swiftrestore/s3.go::buildDownloadJob` + `handleDownloading` | **Reused** for Tier C clones (each replica downloads on its node) |
| Tier B local restore (same-node) | `swiftrestore/local.go` | **Reused** for Tier B clones (replicas pinned to the capture node) |
| Pool replica fan-out (template DeepCopy, topology spread, PVC-per-replica, rolling update) | `swiftguestpool/controller.go` | **Identical** — `cloneFromSnapshot` rides on `template.spec` like any other field |

The pool controller already does `spec := pool.Spec.Template.Spec.DeepCopy()` per
replica, so once `cloneFromSnapshot` is a `SwiftGuestSpec` field it is templated
to every replica **for free**. The work is in the SwiftGuest controller (drive a
clone boot) and the validation webhook (exclusivity + backend rules).

## 3. CRD surface

```go
// SwiftGuestSpec (new field, mutually exclusive with imageRef/kernelRef)
type SwiftGuestSpec struct {
    // ... imageRef, kernelRef, gpuProfileRef ...

    // CloneFromSnapshot boots this guest as a clone of a SwiftSnapshot
    // (Tier B local or Tier C s3). Mutually exclusive with imageRef and
    // kernelRef. The guest resumes the captured state byte-for-byte; identity
    // (MAC, machine-id, hostname, SSH host keys) is regenerated per-clone.
    // +optional
    CloneFromSnapshot *CloneFromSnapshotSource `json:"cloneFromSnapshot,omitempty"`
}

type CloneFromSnapshotSource struct {
    // SnapshotRef names a SwiftSnapshot in the same namespace. It must be Ready.
    SnapshotRef corev1.LocalObjectReference `json:"snapshotRef"`

    // TargetNode pins where this clone runs. Required for an s3 (Tier C)
    // snapshot whose capture node is not where the clone should run (the same
    // role as SwiftRestore.spec.targetNode); for a Tier B (local) snapshot the
    // clone is pinned to the capture node and this is ignored. In a pool, the
    // pool's topology-spread + node-capacity selection supplies this per
    // replica (see §6) — operators rarely set it by hand.
    // +optional
    TargetNode string `json:"targetNode,omitempty"`

    // Regenerate lists identity attributes reset on each clone. macAddresses is
    // ALWAYS forced on (two clones sharing a MAC L2-collide) regardless of this
    // list; the list controls hostname/machineId/sshHostKeys (which fire on the
    // clone's first reboot via the seed bootcmd). Defaults to all four.
    // +optional
    Regenerate []IdentityRegenerationItem `json:"regenerate,omitempty"`
}
```

Exclusivity (webhook, hard-reject at admission): `cloneFromSnapshot` with
`imageRef` or `kernelRef` is rejected (three mutually-exclusive boot sources).
`gpuProfileRef` + `cloneFromSnapshot` is **rejected in Phase 4** — VFIO state
cannot be CH-restored (Phase 0 Constraint #1, the same rule
`includeMemory`+VFIO snapshots already enforce).

## 4. Architecture decision — SwiftGuest-native, NOT pool-spawns-SwiftRestore

Two candidate implementations:

- **(A) SwiftGuest-native boot path.** `cloneFromSnapshot` is a first-class boot
  source. The SwiftGuest controller, on a guest with the field set, drives the
  clone boot directly (Tier C: download Job → restore-receive launcher; Tier B:
  restore-receive launcher pinned to the capture node), computing per-guest
  identity regen. The pool just templates the field.
- **(B) Pool spawns N SwiftRestore objects.** The pool controller creates one
  SwiftRestore per replica. Rejected: SwiftRestore *creates* its target guest
  (`ensureCloneTargetGuest`), which fights the pool's own replica creation and
  PVC-per-replica ownership; two controllers would race on the same guest name;
  and a standalone SwiftGuest with `cloneFromSnapshot` (no pool) would have no
  driver. (B) also leaves `cloneFromSnapshot` on SwiftGuest unimplemented, which
  is half of Scenario 7's ask.

**Decision: (A).** `cloneFromSnapshot` is a SwiftGuest boot source; the pool is a
pure templating consumer. This is the design Scenario 7 and `snapshots.md`
already sketch ("each guest restores from the snapshot → controller assigns each
a unique MAC/hostname/identity → each VM resumes independently").

### 4.1 Required refactor — share the restore primitives

(A) needs the SwiftGuest controller to run the s3 download + identity-regen that
today live in the `swiftrestore` package. To avoid a `swiftguest → swiftrestore`
import (swiftrestore already imports swiftguest — that would cycle), extract the
backend-agnostic primitives into a neutral package both import:

- `internal/snapshot/clonecommon/` (new) — `buildDownloadJob`,
  `resolveCloneNode`, the deterministic MAC-rewrite computation, the
  restore-annotation builder, and the `s3LocalDir`/key-prefix helpers.
- `swiftrestore` keeps its phase machine and calls the shared helpers.
- `swiftguest` grows a `cloneFromSnapshot` boot path that calls the same helpers
  + the existing in-package `BuildRestorePod`.

This refactor is the bulk of Phase 4's risk and is the **first PR** (pure
extraction, no behavior change, SwiftRestore tests stay green).

## 5. Per-replica identity (the central correctness rule)

CH `--restore` resumes byte-for-byte — cloud-init does not re-run — so without
intervention every replica shares the source's machine-id, hostname, SSH host
keys, and guest-visible MAC. The Phase 2 mechanism applies **per replica**:

- **MAC**: the controller rewrites each clone's hypervisor `config.net[].mac` to
  a value deterministic in `(namespace, guestName, iface)` — already implemented
  as `computeMACRewrites`. Always forced for a clone. This prevents L2 collision
  immediately (visible to the bridge), the one collision that cannot be fixed
  from inside the guest.
- **machine-id / hostname / SSH host keys**: the cmdline marker +
  `kubeswift.clone=true` seed bootcmd regenerates them on the clone's **first
  reboot**. Until then they are inherited (the documented resume-vs-boot
  limitation). Pools needing immediate divergence reboot each replica once after
  first resume (a future in-guest vsock agent would remove the reboot — out of
  scope).

The pool gives each replica a unique name (`<pool>-<ordinal>`), so the
deterministic MAC + per-name identity are unique across replicas by construction.

## 6. Pool integration

`SwiftGuestPool.spec.template.spec.cloneFromSnapshot` templates to each replica.
Two pool-specific concerns:

1. **Node placement for Tier C.** Each replica needs a `targetNode`. The pool
   already computes topology spread; Phase 4 fills each replica's
   `cloneFromSnapshot.targetNode` from the replica's scheduled node. **Open
   question OQ1**: pre-assign nodes at replica-create time (pool picks via the
   exported `swiftmigration.NodeHasCapacity` / GPU-style capacity check) vs let
   the download Job float and pin the launcher after. Leaning pre-assign for
   determinism (mirrors the migration target-selection pattern). Tier B pools
   are constrained to the capture node — a Tier-B-templated pool with
   `replicas > (fits on one node)` is a webhook warning, not a hard error.
2. **Snapshot lifetime.** The SwiftSnapshot must outlive the pool (every replica
   reads it / its S3 objects). The pool does not own the snapshot; deleting the
   snapshot while a pool references it is a webhook reject (or a finalizer on
   the snapshot keyed by referencing pools — **OQ2**).

## 7. Backend support

| Backend | Phase 4 | Notes |
|---|---|---|
| Tier B (local) | ✅ | Replicas pinned to the capture node; bounded by that node's capacity |
| Tier C (s3) | ✅ | The natural pool fit — replicas spread across nodes, each downloads from object storage. Recommended for pools |
| Tier A (csi-volume-snapshot) | ❌ deferred | Disk-only; `cloneStrategy: snapshot` on SwiftImage already serves disk-only pool scaling (Scenario 3). cloneFromSnapshot is for full-state (memory) clones |

## 8. Spike plan (de-risk before the PRs)

The central risk is **(A)'s SwiftGuest-native clone boot** — does a SwiftGuest
with `cloneFromSnapshot` actually boot as a clone reusing BuildRestorePod, driven
by the SwiftGuest controller rather than SwiftRestore? Spike on the dev cluster
(boba + miles, the Tier C MinIO setup from the Phase 3 walkthrough):

- **S1**: hand-stamp the restore annotations + a manual s3 download on a
  SwiftGuest (no `cloneFromSnapshot` field yet) to confirm the SwiftGuest
  controller's existing `BuildRestorePod` path boots a clone from an
  s3-downloaded cache. (Validates the reuse before writing the field.)
- **S2**: two such guests on different nodes from the same snapshot — confirm
  deterministic distinct MACs, no L2 collision, both reach Running.
- **S3**: confirm the resume-vs-boot identity limitation reproduces (shared
  machine-id until reboot) and the seed-bootcmd regen fires on reboot — so the
  Phase 4 docs state it accurately.

Spike is **non-merge** (branch `spike/snapshot-phase-4`), findings to a
`-spike.md` doc, per the project's spike contract.

## 9. PR plan

1. **PR 1 — extract `clonecommon`** (refactor; no behavior change; SwiftRestore
   tests green). The load-bearing risk; ship first.
2. **PR 2 — `SwiftGuest.spec.cloneFromSnapshot` CRD + webhook** (exclusivity,
   gpuProfileRef reject, snapshot-Ready check). `make generate` + chart sync.
3. **PR 3 — SwiftGuest controller clone-boot path** (Tier C download → restore-
   receive; Tier B same-node restore-receive; per-guest identity). The feature.
4. **PR 4 — pool node-assignment + snapshot-lifetime guard** (OQ1/OQ2 resolved).
5. **PR 5 — cluster walkthrough + runbook + samples** (a real N-replica pool
   cloned from a Tier C snapshot, spread across boba+miles, sentinel per
   replica). The W5-pattern validation step.

## 10. Open questions (for the spike / design review)

- **OQ1** — pool node pre-assignment vs float-then-pin (lean: pre-assign).
- **OQ2** — snapshot-lifetime enforcement: webhook-reject delete vs
  referencing-pool finalizer (lean: finalizer, mirrors `CloneSeedFinalizer`).
- **OQ3** — does a `cloneFromSnapshot` guest need its own root PVC, or does the
  restore-receive launcher run disk-from-the-cache like the SwiftRestore clone
  path? (Phase 2 clone restore uses the snapshot-stager + emptyDir; confirm the
  same holds with no per-guest PVC, or whether pools want a persistent PVC per
  replica.)
- **OQ4** — interaction with `dataDiskRef` and `spec.storage` (RWX+Block): does a
  clone-booted guest support a secondary data disk / Block root? (Likely yes for
  data disk, defer Block-root-clone.)
