# ORAS Golden Image (P3) — `SwiftImage.spec.source.oci` Design

Status: **Proposed** — Staff-Architect, 2026-07-01. Sub-phase P3 of the ORAS arc
([`oras-vm-disk-artifacts.md`](oras-vm-disk-artifacts.md) §6, §11). Follows P1
(snapshot/restore/clone triad, #295–#299) and P2 (provenance signing, #300).

Adds a fourth **image source** — pull a golden VM disk from an OCI registry —
so a base image is published once and consumed everywhere: portable across
clusters, content-addressed (one digest → stored once), and layer-cache-reused.
This is the consumption side of the ADR's golden-image story; the ~97%
cross-version dedup the spike measured is a **follow-on** (needs disk chunking —
§5), not P3 v1.

---

## 1. Goal

`SwiftImage.spec.source.oci` pulls a golden **raw** disk artifact from any OCI
registry into the import PVC, then reuses the existing import tail
(resize + `sgdisk -e` + GRUB/serial patch) to produce the same `Ready` raw
`preparedArtifact` an `http` source produces. A `source: oci` image then composes
with `cloneStrategy: copy|snapshot` **unchanged** — the import PVC is identical,
so per-guest cloning, snapshots, pools, and boot all work as-is.

Grounding (the pipeline this slots into):
[`import.go:StartImport()`](../../internal/controller/swiftimage/import.go)
dispatches on `spec.source` → `importHTTP` builds an `ubuntu:22.04` Job whose
`importScript()` does curl-download → `qemu-img convert` (qcow2 only) → size →
GRUB patch (Linux, privileged loop-mount); `Prepare()` stamps
`status.preparedArtifact` (raw PVC). P3 adds an `importOCI` sibling that swaps the
**download step only**.

---

## 2. Decisions

### 2.1 The artifact — a single raw disk layer, `artifactType` distinct from snapshots

A golden image is **one raw disk blob**, far simpler than a snapshot's
multi-file layout (config.json/state.json/memory-ranges). The OCI artifact:

- `artifactType: application/vnd.kubeswift.vmimage.v1` — **distinct** from the
  snapshot type (`…vmsnapshot.v1`); a golden image is semantically not a
  snapshot, and the distinct type lets a registry/tooling tell them apart.
- **one layer**, mediaType `application/vnd.kubeswift.vmdisk.raw.v1`, title
  annotation `image.raw` — so the existing `snapshot-oras --mode=download`
  materializes it to `<dir>/image.raw` by title, no new puller code.
- manifest annotations record the source OS / format for humans.

**Raw at rest** (Design Principle #3): the published artifact SHOULD be raw so
the import skips `qemu-img convert` and the bytes are dedup-friendly. The import
still honors `spec.format` (converts a qcow2 layer) for flexibility, but raw is
recommended and is what the publish recipe (§2.4) produces.

### 2.2 The import path — reuse the snapshot-oras puller as an init container

`importOCI` builds a Job with **two containers**, reusing both existing pieces:

1. **initContainer** `snapshot-oras --mode=download --repository=<repo>
   --tag=<tag>|--digest=<digest> [--insecure] --dir=/data` — pulls + digest-
   verifies the artifact (oras `Copy` already verifies every blob against the
   manifest) and materializes `image.raw` into the shared `/data` (the import
   PVC). Auth reuses the dockerconfigjson mount pattern from
   [`clonecommon.BuildOCIDownloadJob`](../../internal/snapshot/clonecommon/download.go)
   (`DOCKER_CONFIG=/oras-auth`, or anonymous).
2. **main container** `ubuntu:22.04` — runs an `importScript` variant that
   **skips the curl download** (`/data/image.raw` already exists) and keeps the
   tail: convert-if-qcow2 → size → GRUB/serial patch (Linux) — the exact
   privileged-loop-mount steps `importHTTP` uses, gated by `osType` identically
   (Windows → skip patch, unprivileged main container).

This writes to the **import PVC** (not a node-local hostPath), so — unlike the
snapshot download Job — it is **not node-pinned** (the PVC attaches wherever the
Job schedules). No new pull mechanism; `importOCI` reuses `importHTTP`'s PVC
creation/sizing verbatim.

### 2.3 Auth + API surface

```go
// ImageSource gains:
OCI *OCIImageSource `json:"oci,omitempty"`

type OCIImageSource struct {
    Repository string  // e.g. ghcr.io/org/golden-ubuntu-noble (no tag)
    Tag        string  // mutually exclusive with Digest
    Digest     string  // sha256:… — pins the artifact (recommended for reproducibility)
    Insecure   bool    // plaintext registry — UNSAFE, in-cluster/test only
    CredentialsSecretRef *SecretObjectReference // kubernetes.io/dockerconfigjson; empty = anonymous
}
```

`credentialsSecretRef` = dockerconfigjson (mirrors `OCIBackend` on snapshots +
SwiftKernel's `OCIRef.PullSecret`) — the pull-credential shape the project
already uses; NOT an S3-style access key.

### 2.4 Publishing — documented `oras push` recipe (v1); first-party tool is a follow-on

P3 v1 is **pull-only** — the operator publishes the golden artifact out-of-band
with a documented layout:

```
oras push <repo>:<tag> \
  --artifact-type application/vnd.kubeswift.vmimage.v1 \
  image.raw:application/vnd.kubeswift.vmdisk.raw.v1
```

(`oras push` sets the layer title to the filename `image.raw` — exactly what the
puller materializes by.) A first-party `swiftctl image publish <swiftimage>`
(push a Ready SwiftImage's raw PVC to a registry, optionally cosign-sign via P2)
is a clean **follow-on** — the consumption side (the portability win) is what P3
v1 ships.

### 2.5 Webhook

Add `OCI` to the exactly-one-of rule in
[`validateSwiftImage`](../../internal/webhook/swiftimage/validator.go) (HTTP /
Upload / PVCClone / **OCI**), require `oci.repository`, and reject `tag` + `digest`
both set. Spec-immutability-when-Ready is inherited (a golden image's source is
fixed once imported).

---

## 3. Phasing

- **P3 design** (this note).
- **P3 PR 1** — API: `OCIImageSource` + `ImageSource.OCI` + webhook one-of +
  `make generate` + chart CRD sync + deepcopy. Unit-test the webhook.
- **P3 PR 2** — import path: `importOCI` (puller init-container + `importScript`
  download-skip variant), source dispatch branch, `snapshot-oras` image resolver
  reused (`SnapshotORASImage()`), status stamping. Unit-test the Job shape;
  cluster-validate.

## 4. Cluster validation (P3 PR 2)

Publish a golden **raw** Ubuntu image to the in-cluster Zot (`oras push`); create
a `SwiftImage` with `source.oci` (by digest) → **Ready** with a raw
`preparedArtifact`; boot a SwiftGuest from it (Running + IP). Then create a
**second** SwiftImage from the **same digest** → its pull is layer-cache-served
(same-digest dedup, the v1 win). Confirm `cloneStrategy: copy` and `snapshot`
both work off an oci-sourced image.

## 5. Non-goals (P3 v1)

- **Cross-version chunked dedup** (the spike's ~97%) — a single-blob raw layer
  dedups only when byte-identical (same digest). Sharing unchanged regions across
  golden v1/v1.1 needs the disk **chunked into fixed-size layers** (or CH v52
  sparse artifacts) — a follow-on; P3 v1's honest win is portability +
  content-addressed single-source + layer-cache reuse.
- **Thin overlay** (registry base + local CoW delta, never materializing the full
  image) — a much larger storage-layer change; P3 v1 full-materializes into the
  import PVC like `http`.
- **First-party publish** (`swiftctl image publish`) — documented `oras push`
  recipe in v1.
- **qcow2-at-rest** — allowed (import converts) but discouraged; raw is the
  dedup-friendly, recommended form.

## 6. Open questions

1. **Verify a signed golden image on pull?** P2 signs snapshot artifacts; a golden
   image can be signed the same way. Should `importOCI` optionally `cosign verify`
   before importing (a `verifyKeySecretRef` on `OCIImageSource`)? Deferred —
   pull-and-verify is a clean P3.x once operators ask; the TLS-for-verify caveat
   (P2 §2.5) applies.
2. **Layer title robustness** — v1 relies on the layer being titled `image.raw`.
   If operators push with a different title, the puller misses it. A
   `--mode=download-image` that materializes the single disk layer regardless of
   title (or reads it from `artifactType`) is a small hardening follow-on.
3. **Digest-pinned vs tag** — recommend digest for reproducibility; a tag is
   mutable (a re-push under the same tag changes content). The webhook allows both
   but docs steer to digest for production.

---

*🤖 Generated with [Claude Code](https://claude.com/claude-code)*
