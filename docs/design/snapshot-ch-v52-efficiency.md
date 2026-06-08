# Snapshot Efficiency on CH v52 — sparse snapshots + userfaultfd restore

> Status: SCOPING / DESIGN. Grounded against the CH v52.0 binary + the KubeSwift
> snapshot machinery. Adopts CH v52's snapshot/restore efficiency features (CH v52
> capabilities assessment, action 5). Some parts are free (sparse is automatic),
> one is a bounded implementation (userfaultfd restore), one is a follow-up (Tier C
> upload compression). Last updated: 2026-06-08.

## 1. Goal

Make KubeSwift snapshots **smaller** and restore/clone/migration-cutover
**faster** by adopting CH v52's:

- **Sparse memory snapshots** (#8113) — `SEEK_DATA`/`SEEK_HOLE` skip untouched
  guest pages.
- **Userfaultfd demand-paged restore** (#7800) — `--restore
  memory_restore_mode=ondemand` resumes the guest immediately and faults pages in
  lazily instead of reading the whole memory image first.

## 2. Sparse snapshots — FREE (automatic), and PR #118 is unaffected

CH v52 writes the snapshot memory file **sparse** automatically (no API option;
`vm.snapshot` just does it). KubeSwift gets this for free on v52:

- **Tier B (local hostPath) + the staging dir** use **less disk** — the file has
  holes for untouched pages; `du` shrinks (apparent `st_size` stays guest-RAM
  size).
- **No code change for correctness.**

**PR #118 dedup is UNAFFECTED — verified, no change needed.** The
[`cmd/snapshot-s3/transfer.go`](../../cmd/snapshot-s3/transfer.go) skip logic
("a memory-ranges file is always exactly guest RAM size, so size alone is unsafe
— compare sha256") refers to the **logical** size (`st_size`), which sparseness
does **not** change (holes still count toward `st_size`), and the sha256 is
computed over the **full** content (zeros for holes). So the same-size-different-
content guard PR #118 added remains correct and necessary. Document this so a
future maintainer doesn't "optimize" the dedup on a false sparseness assumption.

## 3. Tier C upload — sparse on disk ≠ sparse on the wire

CH's sparse snapshot reduces **local disk**, but **not** the S3 upload size: the
uploader reads the memory file with `os.Open` + `io.Copy`
([`transfer.go`](../../cmd/snapshot-s3/transfer.go)), and a normal read returns
**zeros for holes** — so a sparsely-touched 64 GiB guest still uploads ~64 GiB of
mostly-zeros. S3 objects aren't sparse, so on-disk sparseness alone buys nothing
on the wire.

**The Tier C upload win is compression, not sparseness** — zeros (holes) compress
to almost nothing. Options for a follow-up PR:

- **(A) Stream-compress** the artifacts on upload (gzip/zstd) and decompress on
  download — a small, contained change to the `snapshot-s3` transfer path, with a
  `.zst` suffix in the manifest. Biggest win for sparsely-touched guests.
- **(B) Sparse-aware read** (`SEEK_DATA`/`SEEK_HOLE`) + a sparse object layout
  (e.g. a side index of data extents) — more faithful but more format machinery.

Recommendation: **(A) compression** as a follow-up — simplest, and zeros are the
dominant compressible content. Tracked, not in the first implementation PR.

## 4. Userfaultfd restore — the bounded implementation win

CH v52 `--restore` accepts
`source_url=<>,prefault=on|off,memory_restore_mode=copy|ondemand,resume=true|false`
(confirmed against the v52 binary). `memory_restore_mode=ondemand` registers the
guest memory with **userfaultfd** so the VM resumes immediately and pages fault in
on first access — vs the default `copy`, which reads the entire memory image
before resuming. For large guests this dramatically cuts **restore-to-resume**
latency (the operator-visible window).

**Where it applies:** every KubeSwift restore — `SwiftRestore` (in-place +
clone), **`cloneFromSnapshot`** (fast pool scale-up — the latency matters most
here), and conceptually the live-migration receive path.

**Safety:** ondemand requires the snapshot memory file to stay mapped until all
pages fault in. In every KubeSwift restore the file is **local + mounted
read-only for the pod's lifetime** (Tier B hostPath; Tier C node-local cache), so
there is no "source disappeared mid-fault" hazard — userfaultfd is safe here.
(`prefault=on` is the middle ground — register userfaultfd but pre-touch; left
`off` for the pure-ondemand latency win.)

### Plumbing (mirrors the just-shipped `resume=true` / AutoResume path)

The exact same surface PR #161 added for auto-resume:
`RestoreIntent.MemoryRestoreMode` (Go) → swiftletd `RestoreIntent.memory_restore_mode`
→ `spawn_ch_restore` appends `,memory_restore_mode=ondemand` to `--restore`.
Driven from a restore annotation, set by the controller.

### Default vs opt-in — decision

- **`cloneFromSnapshot` → default `ondemand`.** Fast scale-up is the explicit
  goal; the latency win is the point; the snapshot is local. Default it on.
- **`SwiftRestore` → opt-in, default `copy`** initially (disaster-recovery —
  correctness/predictability first), with a `spec.memoryRestoreMode:
  copy|ondemand` field to opt in. Flip the SwiftRestore default to `ondemand`
  after the cluster round-trip confirms it (a one-line follow-up).

Rationale: deliver the win where it matters and is safest (clone scale-up) while
staying conservative on the DR path until validated. Both are gated on the §6
cluster validation.

## 5. Implementation sequencing (note the restore-path dependency)

PR #161 (auto-resume) is an in-flight change to the **same restore machinery** and
is **awaiting cluster validation** (a `cloneFromSnapshot` guest must come up
Running). To avoid stacking two unvalidated restore changes:

1. **Validate PR #161 first** on the cluster (cloneFromSnapshot round-trip).
2. **Then** ship userfaultfd restore (PR below) on the validated base — it reuses
   the exact same `RestoreIntent`/`spawn_ch_restore` surface, so once #161 is
   confirmed, this is low-risk.
3. **Sparse snapshots** need no code (free on v52); confirm the disk-usage drop in
   the same validation round-trip.
4. **Tier C compression** (§3) is a separate follow-up.

## 6. Validation (cluster)

A memory-snapshot round-trip on the dev cluster (the existing snapshot e2e):
- **Sparse:** confirm the Tier B memory file's `du` ≪ `st_size` on v52.
- **userfaultfd:** a `cloneFromSnapshot` clone with `ondemand` comes up Running,
  the source sentinel is byte-identical, and restore-to-Running latency drops vs
  `copy` (measure both).
- **Regression:** SwiftRestore in-place (default `copy`) unchanged.

## 7. Phased PRs

| PR | Scope | Gate |
|---|---|---|
| 1 | This scoping doc. | — |
| 2 | userfaultfd restore: `RestoreIntent.memoryRestoreMode` plumbing (Go+Rust); `cloneFromSnapshot` defaults `ondemand`; `SwiftRestore.spec.memoryRestoreMode` opt-in (default `copy`). | After PR #161 cluster-validated |
| 3 | (follow-up) Tier C upload compression (§3 option A) — `snapshot-s3` stream-compress + manifest suffix. | — |
| — | Sparse snapshots: **no PR** (automatic on v52); confirmed in the PR 2 validation round-trip. | — |

🤖 Generated with [Claude Code](https://claude.com/claude-code)
