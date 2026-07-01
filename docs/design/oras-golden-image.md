# ORAS Golden Image (P3) — `SwiftImage.spec.source.oci` Design

Status: **Accepted (Path B — chunked dedup)** — Staff-Architect, 2026-07-01.
Sub-phase P3 of the ORAS arc
([`oras-vm-disk-artifacts.md`](oras-vm-disk-artifacts.md) §6, §11). Follows P1
(snapshot/restore/clone triad, #295–#299) and P2 (provenance signing, #300). The
API surface shipped in **PR 1 (#301)**; this doc now specifies the **chunked**
artifact + transfer (PR 2), the path chosen for the cross-version dedup win.

Adds a fourth **image source** — pull a golden VM disk from an OCI registry — so a
base image is published once and consumed everywhere, and (the reason for Path B)
a mostly-unchanged **v1.1 re-uses v1's bytes**: the registry stores only the
chunks that actually changed.

---

## 1. Goal

`SwiftImage.spec.source.oci` materializes a golden **raw** disk from an OCI
registry into the import PVC, then reuses the existing import tail
(resize + `sgdisk -e` + GRUB/serial patch) to produce the same `Ready` raw
`preparedArtifact` an `http` source produces. Composes with
`cloneStrategy: copy|snapshot` unchanged.

The disk is stored **chunked** so identical chunks dedup by content address:
- **zero regions** (a raw disk is mostly zeros) collapse — they are never stored,
- **unchanged blocks across versions** (a package upgrade rewrites scattered
  blocks, not the whole disk) are shared,

yielding the spike's ~97% cross-version dedup for a mostly-unchanged image.

Grounding (the pipeline this slots into):
[`import.go:StartImport()`](../../internal/controller/swiftimage/import.go)
dispatches on `spec.source`; `importHTTP` builds an `ubuntu:22.04` Job whose
`importScript()` does curl-download → `qemu-img convert` (qcow2) → size → GRUB
patch (Linux, privileged loop-mount); `Prepare()` stamps
`status.preparedArtifact`. P3 adds an `importOCI` sibling that swaps the
**download step** for a chunk-reassembling puller.

---

## 2. Decisions

### 2.1 Chunking scheme — sparse, offset-based, **fixed-size** (default 64 MiB)

The golden raw disk is split into fixed-size windows. Each window is classified:

- **all-zero → recorded as nothing.** A raw disk is sparse (a 40 GiB disk with a
  3 GiB OS is ~90% zero); the manifest simply omits zero windows, so no zero
  bytes are ever pushed, pulled, or stored. The importer zero-fills by writing
  chunks into a sparse file at their offsets.
- **non-zero → one OCI layer**, `digest = sha256(chunk)`, recorded in the
  manifest as `{offset, size, digest}`. The registry dedups identical digests
  automatically: the same OS block at the same offset in v1 and v1.1 is one blob;
  a changed block gets a new digest and is the only thing re-pushed.

**Fixed-size, not content-defined (CDC).** VM disks are block-addressed and
updated **in place** — a package upgrade rewrites blocks at their existing
offsets; it does not *insert* bytes and shift the tail. So fixed-size windows stay
aligned across versions and dedup without a rolling hash. CDC (buzhash /
casync-style, shift-resistant) is a follow-on **only if** cluster-measured
v1→v1.1 dedup on real images proves low (§4 measures it). Chunk size is
configurable on publish; **64 MiB** default balances dedup granularity against
layer count (a 3 GiB OS → ~48 data chunks). **Uncompressed** in v1 — dedup
already handles the sparse/zero case (nothing to compress); per-chunk compression
is a size/CPU follow-on (deterministic gzip preserves digests, so it stays
dedup-compatible).

### 2.2 Artifact layout

An OCI artifact, `artifactType: application/vnd.kubeswift.vmimage.v1` (distinct
from the snapshot type):

- **config** blob (`application/vnd.kubeswift.vmimage.config.v1+json`): records
  `{ totalSize, chunkSize, format: raw, osType }`.
- **N layers**, `application/vnd.kubeswift.vmdisk.chunk.v1`, one per non-zero
  chunk, each annotated `kubeswift.io/chunk-offset: <bytes>`. Order is by offset;
  the offset is authoritative (not layer position), so identical chunks at the
  same offset across versions produce byte-identical descriptors → dedup.

### 2.3 Publish — a chunking tool (`snapshot-oras --mode=upload-image`)

Chunking can't be a plain `oras push`, so the `snapshot-oras` binary gains:

```
snapshot-oras --mode=upload-image --file=/data/image.raw --chunk-size=64Mi \
  --repository=<repo> --tag=<tag> [--insecure] [--os-type=linux|windows]
```

It streams the raw file in `chunk-size` windows, **skips all-zero windows**,
pushes each non-zero chunk (oras/registry dedups by digest — a re-push of v1.1
only transfers changed chunks), packs the config + chunk layers into the manifest,
and reports `{totalBytes, transferredBytes, skippedBytes, reference,
manifestDigest}` (the existing `clonecommon.TransferReport` shape — so the dedup
% is observable). A first-party `swiftctl image publish <swiftimage>` (chunk a
Ready SwiftImage's PVC via a Job, optionally P2-sign) is a **follow-on**; v1 ships
the tool + a documented recipe.

### 2.4 Import — reassemble into a sparse file (`--mode=download-image`)

`importOCI` runs a puller init-container:

```
snapshot-oras --mode=download-image --repository=<repo> --tag=<tag>|--digest=<d> \
  [--insecure] --file=/data/image.raw
```

It pulls the manifest + config, `truncate`s `/data/image.raw` to `totalSize` (a
sparse file — the zero regions cost nothing), then pulls each chunk layer
(oras verifies every blob against its digest; the registry/node layer cache dedups
the pull) and writes it at its `chunk-offset`. The main `ubuntu:22.04` container
then runs the existing convert-if-qcow2 → size → GRUB/serial patch tail (Linux;
Windows skips the patch, unprivileged), gated by `osType` exactly as `importHTTP`.
Writes to the **import PVC** (not hostPath) → **not node-pinned**.

### 2.5 API + webhook (shipped — PR 1 / #301)

`spec.source.oci` = `OCIImageSource{repository, tag|digest, insecure,
credentialsSecretRef}` (dockerconfigjson; anonymous when empty) + the exactly-one-of
source rule (`http|upload|pvcClone|oci`). Chunking is **transparent to the API** —
a `source: oci` is a `source: oci`; the artifact happens to be chunked. No API
change for Path B.

---

## 3. Phasing

- **PR 1 (#301, merged)** — API surface + webhook + design.
- **PR 2a** — `snapshot-oras` transfer modes: `--mode=upload-image` (sparse
  chunk + push) and `--mode=download-image` (pull + reassemble into a sparse
  file). Hermetic tests: chunk/reassemble round-trip byte-identical; zero windows
  skipped; a modified-one-chunk re-push transfers only the changed chunk
  (dedup assertion via a `memory.Store`).
- **PR 2b** — controller `importOCI` (puller init-container + `importScript`
  download-skip variant), source dispatch, `SnapshotORASImage()` reuse, status
  stamping. Cluster-validate + **measure real v1→v1.1 dedup**.

## 4. Cluster validation (PR 2b) — the measurement doubles as the Path-B spike

1. `upload-image` a golden **raw** Ubuntu Noble disk to the in-cluster Zot →
   record `transferredBytes` (unique data; ≪ disk size, proving zero-skip).
2. Build a **v1.1** (in-place `apt upgrade` of the same disk) → `upload-image` to
   the same repo, new tag → **`skippedBytes / totalBytes` is the dedup %** (target
   ~high; this de-risks the block-aligned assumption — if low, CDC per §2.1).
3. `SwiftImage` `source.oci` (by digest) → **Ready** with a raw
   `preparedArtifact`; boot a SwiftGuest (Running + IP).
4. A second SwiftImage from the same digest → chunk pulls are cache-served.
5. `cloneStrategy: copy` and `snapshot` both work off an oci-sourced image.

## 5. Non-goals (P3 Path B v1)

- **Content-defined chunking** (rolling-hash shift-resistance) — fixed-size fits
  block-addressed disks; CDC only if §4 measures low.
- **Per-chunk compression** — dedup handles the sparse/zero case; compression is a
  transfer-size follow-on (deterministic gzip keeps digests dedup-stable).
- **Thin overlay** (registry base + local CoW delta, never materializing the full
  disk) — a much larger storage-layer change; v1 full-materializes into the import
  PVC.
- **First-party publish** (`swiftctl image publish`) — the `upload-image` tool +
  a documented recipe in v1.

## 6. Open questions

1. **Chunk-size default tuning** — 64 MiB is a starting point; §4's measurement
   informs whether 32/128 MiB dedups better for the target images.
2. **Signed golden images** — a golden image can be P2-cosign-signed; an optional
   `verifyKeySecretRef` on `OCIImageSource` (cosign verify before import) is a
   clean P3.x (the TLS-for-verify caveat, P2 §2.5, applies).
3. **Manifest size at large disks** — a 40 GiB disk at 64 MiB is ≤640 chunk
   entries (fewer after zero-skip); acceptable, but a very large sparse disk with
   scattered data is the case to watch. Chunk-size is the lever.

---

*🤖 Generated with [Claude Code](https://claude.com/claude-code)*
