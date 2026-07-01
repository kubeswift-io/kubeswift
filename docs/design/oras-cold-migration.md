# ORAS Cold / Suspended-State Migration (P4) — Design

Status: **Proposed** — Staff-Architect, 2026-07-01. Phase P4 of the ORAS arc
([`oras-vm-disk-artifacts.md`](oras-vm-disk-artifacts.md) §6.3, §12.1). Builds on
P1 (oci snapshot/restore/clone, #295–#299) and P3 (disk→chunked-oci, #301–#303).
Scope chosen by the operator: **suspended-state** (resume where it left off), not
disk-only cold.

Moves a VM's **full at-rest state — memory + CH device state + disk — across
clusters (or into an air-gap) through an OCI registry**, then **resumes** it,
where live migration cannot run (no shared L2, no cross-cluster CH channel). The
registry is the async transport.

---

## 1. The problem this phase actually has to solve

Two facts collide:

1. An **oci memory snapshot is memory + CH-state only — the disk stays on its
   PVC** (ADR §8; `swiftsnapshot` Tier-B capture writes `config.json` /
   `state.json` / `memory-ranges`, never the disk). Resuming that snapshot in
   another cluster gives a VM with the right RAM but **no disk**.
2. P3 ships **disk → chunked oci**, but for a *golden template* (a SwiftImage's
   at-rest PVC), not a running VM's disk-with-data captured at a specific instant.

So P4's real work is **coherent full-state capture**: the memory snapshot and the
disk artifact must represent the **same instant**, or resume reads RAM that
references disk blocks that have since changed → corruption. And the disk PVC is
**RWO, held by the running launcher pod** — nothing else can mount it while the VM
runs.

---

## 2. Decisions

### 2.1 Coherence mechanism — **capture-then-terminate** (migration, not backup)

This is a **migration** — the VM *leaves* the source — so we do NOT need to resume
it on the source. That removes the hard part:

1. **Pause** the VM (CH pause — the existing oci-snapshot action).
2. **Capture memory** to the node-local hostPath (existing P1 Tier-B capture).
3. **Do NOT resume.** The VM stays paused → **the disk is frozen** at the exact
   instant the memory snapshot captured. No writes can occur.
4. **Terminate the source** launcher (release the RWO PVC) — the memory state is
   already safe on the hostPath.
5. **Chunk the released disk PVC → oci** (P3 `upload-image`) and **push the memory
   hostPath → oci** (P1) — both now reference the same frozen instant, pushed
   async after the short pause window.

The pause window is just the memory snapshot (~seconds, the P1 figure); the
multi-GiB disk chunking happens **after** the source is gone, off the released
PVC. This mirrors **offline migration** (stop source → move → start target) — but
the "move" is via the registry, which is what makes it **cross-cluster**. It
needs **no CSI snapshot** and **no swiftletd disk-chunking** (the released PVC is
chunked by an ordinary node-pinned Job, exactly as P3 does).

*Rejected — CSI-snapshot-at-pause* (snapshot the disk instantly during the pause,
chunk the snapshot async): coherent and keeps the source alive, but needs a
snapshot-capable CSI driver and more moving parts. Capture-then-terminate is
simpler and correct for a migration where the source is leaving. (CSI-at-pause is
the natural basis for a future *live-ish suspended backup* that keeps the source
running — a follow-on, not P4.)

### 2.2 Orchestration — **compose, no `SwiftColdMigration` CRD** (it can't span clusters anyway)

Cross-cluster orchestration is inherently **two independent actors joined only by
the registry** — no single controller can reconcile across cluster A and B. So
(ADR §12.1 resolved): **compose**, do not add a `SwiftColdMigration` CRD.

- **Source cluster A — "full-state export":** a `SwiftSnapshot` gains
  **`spec.includeDisk`** (with `backend.type: oci` + `includeMemory`). The
  capture becomes the §2.1 sequence and `status.oci` records **two** artifact
  refs: the memory artifact (existing) + the **disk artifact** (new). The operator
  (or GitOps) carries the two refs to cluster B (they're just registry
  coordinates).
- **Target cluster B — "full-state import":** `SwiftGuest.spec.cloneFromSnapshot`
  (P1 #299) is extended to accept a **full-state oci snapshot reference** (the two
  refs, or a single manifest that links both). The clone path **materializes the
  disk from oci** (P3 `download-image` → the root PVC) **and** restores the memory
  (CH `--restore`) against it, then **resumes** — reusing the restore-receive
  launcher unchanged.

A thin `swiftctl guest export <guest> --to <repo>` / `swiftctl guest import` pair
wraps the two ends ergonomically (follow-on; the CRDs are the contract).

### 2.3 The full-state artifact — link, don't merge

Keep the memory artifact and the disk artifact as **two OCI artifacts** (each is
already a validated shape: P1's memory snapshot, P3's chunked disk). Link them
with a small **manifest** (an OCI index or a `SwiftSnapshot.status.oci` carrying
both refs + digests). Rationale: the two have different dedup characters (memory
dedups ~0; disk dedups ~97% cross-version) and different lifecycles; merging them
into one blob-set buys nothing and loses P3's disk dedup. The import pulls both by
digest.

### 2.4 Restore coherence — same disk path

CH `--restore` resumes RAM whose `config.json` references the disk by **path**.
The restore-receive launcher in B mounts the **materialized** disk PVC at the
**same path** the source used (the launcher pod builder already fixes that path),
so the resumed RAM and the fresh-from-oci disk cohere. This is exactly how
`cloneFromSnapshot` already resumes (P1 #126's `resumeCloneIfNeeded`); P4 only
changes *where the disk came from* (oci, not a same-cluster PVC).

---

## 3. What's genuinely new (small, because it composes)

| Piece | Reuse | New |
|---|---|---|
| Pause + memory capture | P1 Tier-B oci capture | — |
| Skip-resume + terminate source | offline-migration stop pattern | capture-then-terminate sequencing in the snapshot controller |
| Disk → oci | P3 `upload-image` on the released PVC | a capture Job that chunks the **root** PVC (not a template) |
| `status.oci` two-ref | P1 `OCISnapshotStatus` | + disk artifact ref/digest; `spec.includeDisk` |
| Import: disk from oci | P3 `download-image` | wire into the clone/restore path (materialize root PVC from oci) |
| Import: resume | P1 restore-receive launcher + `resumeCloneIfNeeded` | — |

The runtime (CH `--restore`, launcher, resume) is **untouched**.

## 4. Phasing

- **P4 design** (this note).
- **P4 spike — DONE 2026-07-01, GO** (dev cluster, disk-boot Ubuntu Noble guest
  `cm-src`). The mechanism is a composition whose three hard risks are each
  **already retired by shipped, cluster-validated phases**, so the spike confirmed
  the substrate + composed the rest rather than re-proving proven pieces:
  - **Empirical (this spike):** a disk-boot guest driven straight through the CH
    API (`vm.pause` → `vm.snapshot` → `vm.resume` over the launcher's `ch.sock`)
    produced the memory artifact — `config.json` + `state.json` + a **2 GiB
    `memory-ranges`** (= full guest RAM) — and **resumed** (`state: Running`). The
    disk sits at `/var/lib/kubeswift/disks/root/image.raw`, **frozen while paused**
    (vCPUs stopped → no I/O). A serial-console harness (plant `/root/cm-sentinel`,
    read `/proc/.../boot_id` as the resume-vs-reboot witness) works. So
    **capture-then-terminate = this capture minus the resume**, which leaves the
    disk coherent at the snapshot instant — no new mechanism, just *don't resume*.
  - **Fresh-disk resume — retired by #126:** `cloneFromSnapshot` already
    CH-`--restore`s a memory snapshot against a **per-clone disk** (a fresh PVC)
    and resumes with the source sentinel intact. A disk materialized from oci is
    just another byte-identical fresh PVC.
  - **oci disk byte-identity + bootability — retired by P3:** `upload-image` /
    `download-image` round-trip a disk **byte-identical** and the reassembled disk
    **boots** (P3 loop-mounted + GRUB-patched it, then a guest ran from it).
  - **Memory snapshot/restore coherence — retired by Tier-B** (Phase 2 in-place +
    clone restore).
  - **Large-blob note (ADR §12.7):** `memory-ranges` is the full RAM as one blob
    (2 GiB here) — it does NOT dedup and dominates the move for RAM-heavy guests;
    the *disk* artifact chunks + dedups (P3). Measure push/pull of a multi-GiB
    memory blob during implementation.
- **P4 PRs** (GO): `spec.includeDisk` capture path (capture-then-terminate +
  disk→oci) → full-state `cloneFromSnapshot` import path (disk-from-oci +
  `--restore` + resume) → `swiftctl guest export/import` + runbook → end-to-end
  cluster validation (the full resume + sentinel/boot_id proof runs here, once the
  import path exists to drive it).

## 5. Non-goals (P4 v1)

- **Source stays running** (live-ish suspended backup) — P4 terminates the source
  (it's a migration). CSI-snapshot-at-pause (§2.1) is the basis for that follow-on.
- **`SwiftColdMigration` CRD / one-click cross-cluster orchestration** — the
  registry is the async seam; the two CRDs (export snapshot / import clone) are the
  contract. A cross-cluster *operator* (credential brokering, both-ends automation)
  is a separate arc.
- **Incremental / dirty-page memory** — `memory-ranges` is monolithic (dedup ~0);
  a big paused RAM is a big blob (ADR §12.7 large-blob concern applies).
- **Disk dedup across the migration** — the disk artifact *does* dedup vs a shared
  base (P3), but a specific VM's data disk is largely unique; the win here is
  **portability + resume**, not dedup.

## 6. Open questions (for the spike)

1. **Terminate vs keep-paused during disk chunk** — can we chunk the root PVC
   while the launcher is merely *paused* (PVC still mounted RWO)? If a read-only
   second mount / raw-block read is possible on the CSI driver, we could avoid
   terminating before the chunk. Default (safe): terminate, then chunk the
   released PVC.
2. **Block vs Filesystem root** — P3 chunks a raw disk file; a Block-mode root PVC
   (W9) is a raw device — `upload-image` reads it the same (a device is a file),
   but confirm on Longhorn Block.
3. **Large paused-RAM blob** — a 4–8 GiB `memory-ranges` push/pull time + registry
   tolerance (ADR §12.7); the spike measures it.
4. **Identity on resume** — a resumed VM keeps the source's machine-id / SSH keys /
   IP (the fundamental resume-vs-boot rule). For a *migration* (source is gone),
   collision is not an issue — unlike a clone. The vsock identity agent is NOT
   needed here (the migrated VM legitimately *is* the source, moved).

---

*🤖 Generated with [Claude Code](https://claude.com/claude-code)*
