# Source-independent (fully cross-cluster) full-state clone — design

Status: **Proposed** — 2026-07-02. Extends
[`oras-cold-migration.md`](oras-cold-migration.md) (P4). Grounded in a code
investigation of the clone/resolve/launch path (references below are to `main`
after PR #309).

## 1. The problem

P4 ships cold migration end-to-end **same-cluster**: `swiftctl guest export`
pushes a full-state (memory + disk) artifact pair to a registry, and `guest
import` resumes it. But "import" is not yet fully cross-cluster: it still resolves
the **live source guest's spec**. A full-state clone today has **three live-source
dependencies**, all of which must exist in the target namespace:

| # | Dependency | Where | Why it fires |
|---|---|---|---|
| 1 | The source **SwiftGuest** | [`clone.go:120`](../../internal/controller/swiftguest/clone.go) — `prepareCloneFromSnapshot` does `effective.Spec = source.Spec` | Builds the clone launcher from the source spec. |
| 2 | The source **SwiftImage** | [`resolver.go` `resolveDiskBoot`](../../internal/resolved/resolver.go) Gets it and requires `Ready` | The resolver is image-oriented — it resolves `PreparedImage` from the effective spec's `imageRef` even though the clone's disk actually comes from oci (`maybeRootDiskFromOCI` overrides `rg.PreparedImage.PVCName` afterwards). |
| 3 | The source **SwiftSeedProfile** | [`restore.go:221-332`](../../internal/controller/swiftguest/restore.go) rebuilds `seed.iso` when `rg.HasSeed()` | CH `--restore` re-opens the `seed.iso` disk path recorded in `config.json` and **refuses to restore if the file is missing**. The launcher reconstructs it deterministically from the seed ConfigMap (resolved from the source `seedProfileRef`). |

For a true cross-cluster move (source cluster A drained/gone), none of these exist
in cluster B. Only the **registry artifacts** (memory + disk) travel. So the design
question is: **what must the snapshot carry so the target can build the clone
launcher from the registry alone?**

## 2. What a full-state *resume* actually needs

A full-state clone is a **resume**, not a boot. That collapses most of the source
spec to irrelevance:

- **CPU / memory** come from the captured `config.json` (CH `--restore` uses the
  captured vCPU count + RAM). The pod's *resource limits* still need values — take
  them from the captured spec (already `status.guestSpec.{cpu,memoryMi}`).
- **The root disk** comes from the oci disk artifact (`maybeRootDiskFromOCI`). The
  clone needs the **storage class / accessMode / volumeMode / size** for the
  materialized PVC — from the clone's own `guestClassRef` + captured storage.
- **The image is NOT needed** — the disk is byte-materialized from oci, not cloned
  from a base image. Dependency #2 is *incidental*, not essential.
- **Cloud-init does NOT re-run** — the guest resumes mid-flight. So the seed
  *content* is irrelevant to the resume; CH only needs a **file at the seed.iso
  path** so the disk device opens (dependency #3 is a file-existence requirement,
  not a content requirement).
- **Networking**: the launcher sets up tap/br0/dnsmasq (+ any secondary NICs). It
  needs to know networking is on and the **interface names** (for deterministic
  per-clone MAC rewrites — [`ComputeMACRewrites`](../../internal/snapshot/clonecommon/mac.go)
  uses only `source.Spec.Interfaces[].name`, not the live MACs).
- **guestAgent / osType** affect the launcher (vsock, Windows vs Linux).

## 3. Decisions

### D1 — Expand `status.capturedGuestSpec` to a launcher-sufficient surface

Capture, at snapshot time, exactly the fields the resume launcher consumes (and no
more — avoid re-capturing what `config.json` already holds):

```go
type CapturedGuestSpec struct {
    CPU       string `json:"cpu,omitempty"`       // existing (pod limits)
    MemoryMi  int64  `json:"memoryMi,omitempty"`  // existing (pod limits)
    ImageName string `json:"imageName,omitempty"` // existing (informational)

    // NEW — the launcher-sufficient surface (populated for includeDisk captures):
    Storage       *CapturedStorage `json:"storage,omitempty"`       // accessMode/volumeMode/storageClassName
    RootDiskSize  string           `json:"rootDiskSize,omitempty"`  // e.g. "40Gi"
    Network       bool             `json:"network,omitempty"`       // networking enabled
    InterfaceNames []string        `json:"interfaceNames,omitempty"`// for MAC-rewrite seeding
    GuestAgent    bool             `json:"guestAgent,omitempty"`    // vsock agent opted in
    OSType        string           `json:"osType,omitempty"`        // linux|windows
    HasSeed       bool             `json:"hasSeed,omitempty"`       // source had a seedProfile
}
```

Populated by the SwiftSnapshot controller from the resolved source guest at capture
(the source is live *then*). This is additive — a CRD change (`make generate` +
chart sync), no breaking field.

### D2 — Resolver: a "full-state clone, disk-from-oci, no image" path (the crux)

The resolver requires exactly one of `imageRef`/`kernelRef`
([`resolver.go:41-45`](../../internal/resolved/resolver.go)). Add a third boot
source it already half-knows about: **`cloneFromSnapshot`**. Today
`UsesCloneFromSnapshot()` short-circuits to a "not implemented" error and the
SwiftGuest controller routes around it via the effective-spec trick. Instead:

- **Keep the effective-spec fast path when the source guest exists** (no behaviour
  change for same-cluster — the validated path stays exactly as-is).
- **When the source guest is absent AND the snapshot is a full-state oci snapshot**
  (`status.oci.disk` set + `status.capturedGuestSpec.storage` populated), the
  SwiftGuest controller **synthesizes `rg` directly from `CapturedGuestSpec`** — a
  new `resolved.FromCapturedSpec(guest, captured)` constructor that fills
  `Resources`, `Storage`, `RootDisk`, `Network`, `HasSeed=false` (see D3), and a
  `RootDisk.FromOCI=true` sentinel — **skipping image resolution entirely**.
  `maybeRootDiskFromOCI` then supplies the disk as it already does.

`FromCapturedSpec` is a pure function (guest + captured → `*ResolvedGuest`), unit-
testable with no cluster. This contains the blast radius: the image-oriented
resolver is untouched; the new path is a parallel constructor gated on
"source-gone + full-state".

### D3 — Seed: a deterministic minimal placeholder (recommended), not captured content

CH `--restore` needs a **file** at the config.json seed path, not the original
bytes (cloud-init won't re-run). Three options considered:

- **(a) Capture seed content** into `CapturedGuestSpec` → rebuild the real seed.iso.
  Heaviest; re-captures data the resume ignores.
- **(b) Chunk seed.iso to oci** as a third artifact. Extra transfer + plumbing for
  bytes the resume ignores.
- **(c) Rebuild a deterministic *minimal* NoCloud seed.iso** at the config.json
  path (empty/placeholder user-data). **Recommended.** The resume opens the disk
  and never reads it. Set `CapturedGuestSpec.HasSeed` so the launcher knows to
  synthesize the placeholder at the right path; `FromCapturedSpec` sets
  `HasSeed=false` on `rg` so the *content* path is skipped, and the launcher writes
  a minimal cidata ISO purely to satisfy the disk-open.

**Caveat (documented):** a source-independent clone that later *reboots* has no
real seed — the identity-regen bootcmd would not fire. This is acceptable because
the recommended identity path is the **in-guest vsock agent** (regenerate in place,
no reboot — [`identity-regeneration.md`](../snapshots/identity-regeneration.md)),
and the CH v52 clone-reboot firmware hang already discourages the reboot path.
Operators needing the real seed on a source-independent clone use option (a)/(b) —
tracked, not built in v1.

### D4 — Data disks: out of scope for v1

Secondary data disks (`dataDiskRefs`) would each need their own oci artifact +
materialization in the target. v1 source-independence is **root-disk-only**: the
webhook/controller rejects a source-gone import of a snapshot whose source had data
disks (`CapturedGuestSpec` records their presence). Full-state data-disk capture is
a v1.1 follow-on (chunk each data disk to oci alongside the root).

### D5 — `prepareCloneFromSnapshot`: fall back to captured spec

```
source guest exists?
  ├─ yes → effective.Spec = source.Spec   (validated same-cluster path, unchanged)
  └─ no  → full-state oci snapshot with a populated CapturedGuestSpec?
             ├─ yes → rg = resolved.FromCapturedSpec(guest, snap.Status.CapturedGuestSpec)
             │         MAC rewrites from CapturedGuestSpec.InterfaceNames
             └─ no  → fail (today's "source ... no longer exists" — a memory-only
                       or pre-expansion snapshot genuinely needs the source spec)
```

### D6 — Identity

MAC rewrites derive from `CapturedGuestSpec.InterfaceNames` (falls back to `eth0`).
`ComputeMACRewrites` gains an overload taking names instead of a `*SwiftGuest`
(or the caller synthesizes a stub guest with the captured interface names —
smaller change). The vsock identity agent path is unchanged.

## 4. Phasing

- **PR 1 — capture surface (SHIPPED #311):** expand `CapturedGuestSpec` (D1) +
  populate it at capture from the resolved source guest; `make generate` + chart
  sync. Additive; no behaviour change.
- **PR 2 — resolver path (SHIPPED #312):** `resolved.FromCapturedSpec` (D2) +
  `RootDisk.FromOCI` sentinel + unit tests (pure).
- **PR 3 — import fallback (this PR):** `prepareCloneFromSnapshot` dispatches a
  source-gone clone to `prepareSourceIndependentClone` (D5): guards full-state +
  captured surface (else the pre-SI needs-the-source-spec message) and rejects
  captured `hasDataDisks` (D4, controller-side — races-immune vs an
  admission-time check); builds `rg` via `FromCapturedSpec` from the clone's own
  guestClass + the captured surface; a `placeholderSeed` (minimal NoCloud) rides
  the EXISTING seed-ConfigMap + launcher ISO machinery so CH `--restore` can
  re-open the config.json seed disk with zero Rust change (D3); a
  `stubSourceFromCaptured` (identity + interface names + agent opt-in) feeds the
  shared `cloneRestoreAnnotations` builder so MAC rewrites/runtime-dir
  prefixes/agent-enable derive from the captured surface (D6). The controller
  consumes the pre-resolved `rg` (skipping the image-oriented resolver) and the
  root-disk gate also fires on `RootDisk.FromOCI` so `maybeRootDiskFromOCI`
  materializes the disk. Same-cluster clones are byte-identical in behaviour
  (source-present keeps the effective-spec path).
- **PR 4 — validation:** `swiftctl guest import` works with the source (and
  its SwiftImage + seedProfile) deleted; cluster-validate by simulating
  cross-cluster (below). Runbook update (drop the same-cluster scope note).

## 5. Validation strategy (one cluster)

True cross-cluster can't be hardware-validated with one cluster, so **simulate**
source-gone in the same cluster:

1. Boot a source guest, plant a sentinel + record `boot_id`.
2. `swiftctl guest export` (full-state oci) → Ready.
3. **Delete the source SwiftGuest, its SwiftImage, and its SwiftSeedProfile** — the
   three live-source dependencies. The registry artifacts remain.
4. `swiftctl guest import` → the clone must reach Running from the captured spec +
   oci artifacts alone, with the sentinel intact and `boot_id` matching (resume).

Ship explicitly labelled "cross-cluster validated by same-cluster
source-deletion simulation; a true two-cluster move is the natural extension."

### Validation — DONE 2026-07-02, PASS

Dev cluster, controller `sha-e6af503` (PR 3 branch = main + #311 + #312 + PR 3),
snapshot-oras `sha-a2edb7c`, in-cluster Zot. Dedicated source assets so shared
demo assets survive the deletions:

1. `cm5-image` (fresh Noble import) + `cm5-seed` + `cm5-src` (miles, class
   `ft-small`) → Running; planted `/home/kubeswift/cm5-sentinel` + recorded
   `boot_id=387c51dd-feed-4100-aba9-6982d4a7bccf`.
2. `swiftctl guest export cm5-src … --wait` → **Ready**; the snapshot carried the
   full captured surface (`status.guestSpec`: cpu=2, memoryMi=2048,
   rootDiskSize=10Gi, storage RWO/Filesystem, network=true, osType=linux,
   hasSeed=true) alongside both artifacts. (Prerequisite: apply the SI-PR1 CRD
   first — a stale CRD silently strips the new status fields, the W27 trap.)
3. **Deleted `cm5-src`, `cm5-image`, `cm5-seed`** — all three NotFound-confirmed.
4. `swiftctl guest import cm5-clone --from-snapshot cm5-export --target-node boba
   --guest-class ft-small --wait` → **Running on boba**, built from the snapshot +
   registry + guestClass alone.
5. **Resume proven**: sentinel present; clone `boot_id` **identical** to the
   source's; single-boot journal spanning source-boot → clone;
   `cloud-init status: done` (the placeholder seed was never read); hostname
   resumed. Machinery: placeholder seed ConfigMap `cm5-clone-seed` minted (D3,
   zero Rust change); RestoreSeeded clone PVC Bound (default-class fall-through,
   the documented nuance); all four transfer Jobs Complete (memory push/pull +
   disk chunk/materialize).

## 5.5 v1.1 — data-disk full-state capture (design addendum, 2026-07-02)

v1 is root-disk-only (D4). v1.1 extends full-state capture/import to secondary
data disks, grounded in how they resolve today
([`resolver.go::resolveDataDisks`](../../internal/resolved/resolver.go)):

| Data-disk kind | Backing PVC | CH path | v1.1 |
|---|---|---|---|
| `blank` | guest-owned `<guest>-data-<name>` | `/dev/kubeswift-data-<name>` (Block) or `…/data-<name>/image.raw` (FS) — **guest-name-independent** | **captured** |
| `pvcRef` + `attachAsDisk` | operator's Block PVC | `/dev/kubeswift-data-<name>` | **captured** |
| `imageRef` (incl. legacy `dataDiskRef`) | the SwiftImage's **shared** prepared PVC | `…/data-<name>/image.raw` | **excluded** — chunk-after-terminate races other guests attached to the shared PVC, and its lifecycle belongs to the image; a full-state capture of such a source is rejected (loudly, controller-side) |

Because CH disk paths derive from the **disk name**, a clone whose disks keep
the source's names presents identical paths — `config.json` coheres with no
rewriting (the same property the root/seed rely on).

**Capture** (extends `handleFullStateDiskCapture`): after the launcher
terminates (all PVCs frozen + released), one chunk Job **per data disk**
(`<snap>-oci-disk-<name>`, tag `<tag>-disk-<name>`, Block volumeDevices or FS
mount per the disk's shape — the existing root-chunk builder generalized).
`status.oci.dataDisks[]` records `{name, reference, manifestDigest,
pushedBytes}`; `status.guestSpec.dataDisks[]` records the launcher-sufficient
shape `{name, size, block}`.

**Import**: for a full-state snapshot carrying data-disk artifacts (either the
same-cluster or the source-gone path), `maybeRootDiskFromOCI` generalizes to
materialize root **and** each data disk into guest-owned PVCs named
`BlankDataDiskPVCName(clone, name)` (one digest-pinned download Job per disk),
then `rg.DataDisks[i].PVCName` is overridden to the materialized PVCs —
keeping name/Block/HostPath so device order and paths match the captured
`config.json`. `EnsureBlankDataDisks` skips materialized (RestoreSeeded-
labelled) PVCs — they are not blank, and the Filesystem fill Job must not
overwrite them. The v1 `hasDataDisks` rejection is lifted **only when the
artifacts are present** (old snapshots that recorded `hasDataDisks` without
artifacts still fail with the source-spec-required message).

**Validation**: a source with a blank **Block** data disk (the
cluster-validated v0.4.2 path): write a marker onto `vdc`, full-state export,
delete the source (+ image + seed), import → Running with the `vdc` marker
intact and `boot_id` matching.

Phasing: **PR1** API (`CapturedDataDisk` + `status.oci.dataDisks`) + capture +
image-backed rejection; **PR2** import (materialize + `rg.DataDisks` override
+ blank-skip guard + lift the rejection); **PR3** cluster validation +
runbook.

## 6. Non-goals (v1)

- Data-disk full-state capture (D4 — **designed above as v1.1**).
- Real seed content on the source-independent path (D3 caveat — option a/b later).
- A `SwiftColdMigration` orchestration CRD (still compose-first, per
  [`oras-cold-migration.md`](oras-cold-migration.md) §2.2).
- Automatic cross-cluster snapshot-object replication — the operator recreates the
  `SwiftSnapshot` object in cluster B pointing at the same repo/digest (a thin
  `swiftctl` helper to emit that object is a candidate follow-on).

## 7. Risks

1. **Resolver bifurcation** — two paths that build `rg` (image-resolve vs
   `FromCapturedSpec`) can drift. Mitigate: `FromCapturedSpec` is a small pure
   function with a golden test asserting the `rg` it produces matches the
   effective-spec path for the same inputs (same-cluster parity test).
2. **Under-captured spec** — a field the launcher needs but we didn't capture
   surfaces only when the source is gone. Mitigate: the same-cluster parity test +
   the source-deletion cluster validation exercise the exact source-gone path.
3. **Snapshot-object portability** — the target cluster needs a `SwiftSnapshot`
   object with `status.oci.{disk,manifestDigest}` + `status.capturedGuestSpec`. In
   v1 the operator recreates it (or reuses the same-cluster object). A future
   helper emits a portable object.
