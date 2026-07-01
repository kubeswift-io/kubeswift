# ADR: ORAS / OCI Registries for At-Rest VM-Disk Artifacts

Status: **Proposed → recommended ACCEPT** — Staff-Architect, 2026-06-30. The companion
spike [`oras-vm-disk-artifacts-spike.md`](oras-vm-disk-artifacts-spike.md) is
**COMPLETE — GO** (2026-07-01): byte-identical OCI round-trip same- and cross-node,
golden-base layer dedup ~97%, Referrers API + cosign signing validated against Zot. One
scope refinement folded in below (§8/§12): the **dedup** pillar attaches to the
golden-image path, not the memory-snapshot path. Final accept is the maintainer's.

This is an Architecture Decision Record. It frames the problem, records the decision
and its boundary, and enumerates the alternatives, non-goals, and open questions. It
deliberately does **not** fix CRD schema — where the schema is a real decision, it is
flagged as one.

---

## 1. Context

KubeSwift already distributes immutable artifacts through OCI registries, and it
already exports VM snapshots to a durable off-cluster store. Two shipped facts anchor
this ADR:

- **SwiftKernel pulls OCI artifacts via ORAS today.** The kernel-boot path pulls a
  `bzImage` + `rootfs.cpio.gz` from a registry using the `oras` CLI v1.3.1 in a
  node-local Job ([`internal/controller/swiftkernel/pull.go`](../../internal/controller/swiftkernel/pull.go)),
  with custom media types (`application/vnd.kubeswift.kernel.binary`,
  `application/vnd.kubeswift.initramfs.binary`) and an `OCIRef{Image, PullSecret}`
  surface. KubeSwift is already a registry **client**, materializing artifacts to a
  deterministic node-local path (`/var/lib/kubeswift/kernels/<ns>-<name>/`).
- **Snapshots already export to S3 (Tier C).** `SwiftSnapshot.spec.backend.type: s3`
  captures via Cloud Hypervisor (pause→snapshot→resume) then a node-pinned upload Job
  (`snapshot-s3`, embedding the minio-go SDK) pushes a checksummed `manifest.json` +
  artifacts to any S3-compatible store, and `SwiftRestore` pulls + verifies + restores
  ([`api/snapshot/v1alpha1/swiftsnapshot_types.go`](../../api/snapshot/v1alpha1/swiftsnapshot_types.go),
  [`internal/controller/swiftsnapshot/s3.go`](../../internal/controller/swiftsnapshot/s3.go)).

So this is **not greenfield**. The sharp question is not "can KubeSwift use a
registry" — it demonstrably can — but **what an OCI registry buys over the S3 object
store we already integrate**, and where the boundary sits between a registry (good at
immutable, content-addressed, distributable artifacts) and the live VM disk (a
mutable, random-access block device).

The motivating use cases for bringing ORAS in deliberately, as a first-class at-rest
substrate:

1. **Snapshots / checkpoints as OCI artifacts** — quiesce, capture, package, push;
   dedup a shared base across many VMs; restore = pull + materialize onto local block
   storage, then boot.
2. **Cold / suspended-state migration** — suspend-to-registry, resume-elsewhere
   (frozen disk + memory as artifacts). Distinct from *live* migration, which streams
   over a Cloud-Hypervisor channel and is **out of scope** here.
3. **Golden-image + thin-overlay** — an immutable base pulled from a registry, with a
   thin writable CoW overlay on local block storage.
4. **Provenance & signing** — cosign signatures, SBOMs, and attestations attached as
   OCI **referrers**, extending the supply-chain spine (already keyless-cosign for
   images, charts, and CLI blobs) to disk artifacts. Aligns with the
   confidential-computing direction ([`swiftconfidential-sev-snp.md`](swiftconfidential-sev-snp.md)).

---

## 2. The mismatch — why "VM disks on a registry" is not literally what we are doing

A registry and a live block device have incompatible data models. The ADR is precise
about this because the boundary of the decision lives exactly here.

| | OCI registry (ORAS target) | Live VM root disk |
|---|---|---|
| Mutability | Immutable. A blob is identified by its content digest; change one byte → a new digest. There is no in-place mutation. | Mutable. The guest rewrites 4 KiB sectors continuously for the life of the VM. |
| Access granularity | Whole-blob PUT / GET. No range writes; no block-granularity random reads. | Random-access reads and writes at block granularity. |
| Concurrency | No writer-exclusivity or locking protocol; push-then-immutable. | Single-writer exclusivity is mandatory (two VMs on one RW block device corrupt it). |
| Versioning | Content-addressed; identical content dedups by digest for free. | "Version" is meaningless for a hot disk — it changes every millisecond. |

You **cannot** back a running VM's disk with a registry blob. That is why the live
read/write path **stays on real block storage** (local NVMe / Ceph / a CSI driver) and
is explicitly out of scope.

But every **at-rest** state of a disk is the opposite of a hot disk — it is immutable,
whole-artifact, and content-addressable *by nature*:

- a snapshot is a frozen, never-again-mutated point in time;
- a suspended checkpoint (disk + memory) is frozen by construction;
- a golden base image is immutable by definition;
- a cold-migration freeze is, again, immutable for the duration of the move.

For these, a content-addressed immutable blob store is not a compromise — it is the
**correct** storage model. The decision boundary is exactly the immutability
transition: **live mutable (CSI) ⟷ at-rest immutable (registry)**. A snapshot crosses
it one way (quiesce → package → push); a restore crosses it the other (pull →
materialize onto a fresh mutable block device → boot).

A load-bearing observation from the existing code: **the shipped S3 backend already
uses only PUT / GET / DELETE** — never range-reads, partial-writes, list-pagination,
or locking. It quietly already lives inside the semantics a registry can offer. That
is what makes ORAS a near-drop-in for the at-rest path rather than a re-architecture.

---

## 3. Decision

**Adopt ORAS (the `oras-go/v2` Go SDK) as a registry client for the at-rest and
immutable states of VM disks, surfaced as a new snapshot/restore backend
(`backend.type: oci`) alongside the existing `local` and `s3` backends. The live
mutable block device stays on CSI / real block storage and is out of scope. KubeSwift
is a registry client, never a registry.**

Concretely:

- **The mechanism mirrors the shipped `s3` backend, not a new architecture.** A new
  `oci` value on the `SnapshotBackendType` enum; a `snapshot-oras` node-pinned
  transfer-Job image embedding `oras-go/v2`; reuse of the capture primitive (CH
  pause→snapshot→resume producing `config.json` / `state.json` / `memory-ranges`), the
  node-pinning rule, the `manifest.json` integrity layer, and the shared Tier-B
  restore tail (`materializeRestoreTarget` → CH `--restore`) — all unchanged.
- **The pluggability seam is the existing one** — the controller's
  `switch snap.Spec.Backend.Type` dispatch
  ([`internal/controller/swiftsnapshot/controller.go:170`](../../internal/controller/swiftsnapshot/controller.go))
  plus a transfer-Job binary. It is **not** a new Go "RegistryBackend interface." (See
  §6 — this is a deliberate correction of the initial framing.)
- **The registry is a declared external dependency**, exactly like the CSI driver, the
  CNI, and the image registry the cluster already pulls from. Operators bring Harbor /
  Zot / `distribution` / a cloud registry; KubeSwift declares a reference and a pull
  secret. For edge / air-gapped, the recommendation is to run an existing lightweight
  OCI registry — **Zot** is the primary candidate (CNCF, OCI-native, Referrers API,
  offline mirror/sync) — as part of an **edge profile**, not built in.

The decision is **gated on the spike** (§ companion doc). The spike must demonstrate a
correct round-trip and at least one of {cross-VM layer dedup, referrer-based signing}
actually working against a real registry — those are the value-over-S3 claims that
justify ORAS over the Tier C we already shipped.

---

## 4. The boundary, and how it interacts with what already exists

### 4.1 Live mutable disk — unchanged, on CSI

The root disk and data disks remain PVCs provisioned by CSI per
`spec.storage{accessMode, volumeMode, storageClassName}`. The runtime path is
untouched: Filesystem mode mounts `image.raw`; **Block** mode (the W9 path) surfaces a
raw device at `/dev/kubeswift-root` via `volumeDevices`, which Cloud Hypervisor opens
opaquely as `--disk path=…` (it does not care file-vs-device). ORAS-materialized disks
land on exactly these surfaces — a pull writes into a Filesystem PVC's `image.raw` or
`dd`/`qemu-img convert`s onto a Block PVC — and from that point the existing clone +
boot pipeline is identical.

### 4.2 Snapshot / restore — a new backend, no new CRD

ORAS is `SwiftSnapshot.spec.backend.type: oci` and the corresponding `SwiftRestore`
path. No new CRD for the snapshot/restore/cold-migration cases (§6). The phase machine
extends the way `s3` already did: `Pending → Capturing → (Pushing) → Ready` on
capture; `Pending → Downloading → Restoring → Resuming → Ready` on restore.

### 4.3 Golden image — a SwiftImage source extension, not a new CRD

The golden-base case slots into the existing `SwiftImage` pipeline at the "prepared
artifact" stage. Today `spec.source` is `HTTP | PVCClone | Upload`; an `oci` (or
`oras`) source type would pull a raw base from a registry instead of downloading over
HTTP — materialized to a deterministic node path exactly like SwiftKernel — after
which the existing `cloneStrategy: copy | snapshot` overlay machinery (incl. the
`cloneSeed` VolumeSnapshot) is unchanged. This is a **source extension**, not a new
CRD; it is in scope for the decision but a later phase than snapshot/restore.

### 4.4 Live migration — unaffected

Live migration streams memory/disk over a Cloud-Hypervisor channel (Phase 3a–3c, mTLS
sidecar). ORAS does **not** touch that path. The ORAS scope is **cold / suspended-state**
migration — a different operation with a different downtime profile (push + pull vs
sub-second cutover), valuable precisely where live migration cannot run (cross-cluster,
air-gapped transfer, or where the operator wants a durable checkpoint as a side effect
of the move). The two are complementary, not competing.

---

## 5. Registry as a dependency, not embedded — and the explicit rejection of embedding

**KubeSwift will not implement, embed, or operate a registry.** This is a hard line,
and the reasoning is recorded so a future contributor does not relitigate it:

- A registry is a stateful distributed service — storage backend, garbage collection,
  quota, replication, auth, a full HTTP API surface. Building one is a multi-year
  effort orthogonal to running VMs, and squarely against Design Principle #1
  (minimalism).
- Operators **already run a registry** — every cluster pulls images from one. It is
  infrastructure the cluster already has, like a CSI driver or a CNI. The correct move
  is to consume it through a declared dependency, mirroring how storage is consumed
  through CSI.
- Embedding would couple KubeSwift's release cadence to registry-server CVEs, force us
  to own GC / quota / replication, and duplicate Harbor / Zot / `distribution` for no
  differentiated value.
- The precedent is already set: SwiftKernel is a registry **client**. Stay a client.

For edge / air-gapped, "the operator already runs a registry" can be false. The answer
is **not** to embed one but to ship an **edge profile** that runs an existing
lightweight OCI registry as a declared component — **Zot** is the primary candidate
(CNCF, OCI-native, Referrers API, `zot sync` offline mirror). This is the same posture
as recommending Longhorn for storage or Multus for multi-NIC: a named, swappable
dependency, documented and sampled, not built.

---

## 6. Surface: a backend enum + a transfer Job — not a Go interface, and (probably) no new CRD

### 6.1 Correcting the "interface" framing

The initiating brief asked to "mirror how storage is treated: backend consumed through
an interface; registry pluggable the same way." Grounding in the code corrects this in
two ways, and the correction matters:

- **Storage is not consumed through a Go interface.** There is no `StorageBackend`
  interface. Storage is *declarative delegation to CSI*: the spec names
  `accessMode / volumeMode / storageClassName`, the controller creates a PVC, and the
  CSI driver provisions. The pluggability is the StorageClass, not a Go abstraction.
- **The snapshot subsystem's own backend split is already not a Go interface** either.
  `local` vs `s3` is a `switch` on `spec.backend.type` in the controller plus a
  transfer-Job binary. That is the idiom to follow.

KubeSwift *does* have a genuine pluggable-Go-interface idiom — `gpualloc.Backend`
(`Prepare/Resolve/Release`) and the `ovnBackend` first-match chain — but those exist
because allocation logic has real branching behavior to abstract. The snapshot backend
has none: capture and restore are backend-agnostic; only *transfer* differs, and
transfer is already isolated in a Job-image arg contract
(`--mode=upload|download --dir=/snap --snapshot=<ns>/<name> …`). **Adding a Go
interface here would be ceremony, not abstraction.** The registry is "pluggable the
same way storage is" — through a declared external dependency and a `backend.type`
enum value — which is the honest reading of the brief's intent.

### 6.2 The likely shape (illustrative, not fixed)

- `SnapshotBackendType` gains `oci`; `SwiftSnapshotBackend` gains an `*OCIBackend`
  pointer carrying the registry reference (`registry`/`repository`/`tag`), a
  `credentialsSecretRef` (reuse the SwiftKernel **dockerconfigjson pull-secret**
  pattern, *not* the S3 access-key Secret shape), and TLS/insecure knobs.
- A `snapshot-oras` image, `KUBESWIFT_SNAPSHOT_ORAS_IMAGE`, paralleling
  `snapshot-s3` — node-pinned to the capture node, `/snap` RO mount, the same
  `manifest.json` integrity layer, root + drop-ALL + read-only-rootfs + idempotent
  retries.
- The same status surface (rename `status.s3` → a backend-neutral block, or add
  `status.oci`): location, manifest digest, uploaded/downloaded bytes.

### 6.3 The one-vs-several CRD decision (a real decision — not fixed here)

- **Snapshot / restore: no new CRD.** High confidence. It is a backend value on
  existing CRDs.
- **Golden image: no new CRD** — a `SwiftImage.spec.source` extension (§4.3).
- **Cold migration: genuinely open.** It can be expressed as
  `SwiftSnapshot(oci, includeMemory)` on the source + `SwiftRestore(oci ref,
  targetNode)` on the destination — the registry is the shared substrate, and
  cross-cluster falls out naturally because both ends speak to the same registry.
  **Or** a thin new `SwiftColdMigration` CRD if the two-cluster, two-controller
  orchestration warrants a single object to track the move. The lean is **no new CRD in
  v1** (compose the two existing operations); promote to a dedicated CRD only if
  cross-cluster orchestration demands it. This is flagged as an open question (§12), not
  decided.

---

## 7. Confidential-computing alignment

The signing/provenance use case is not a bolt-on — it is the reason a *registry*
(with a referrer graph) beats a *bucket*, and it composes with the SEV-SNP direction.

- **Supply-chain provenance for disk artifacts.** A snapshot or golden-base artifact
  carries a cosign signature, an SBOM, and attestations as OCI **referrers**,
  discoverable via the Referrers API (`oras discover`, `cosign verify`). This extends
  the *existing* keyless-cosign + SLSA-provenance + SPDX-SBOM spine (today on images,
  charts, and CLI blobs in `release-stable.yaml`) to disk artifacts, on the same
  workflow-OIDC identity. An S3 bucket has no referrer graph; signing there is a
  side-channel.
- **Build-time provenance vs launch-time attestation are distinct and both needed.**
  The registry signature proves *who built and signed this at-rest artifact*
  (supply-chain, build-time). SEV-SNP attestation proves *what actually launched* (a
  PSP-signed measurement, launch-time; KubeSwift is outside the guest TCB per
  [`swiftconfidential-sev-snp.md`](swiftconfidential-sev-snp.md)). Neither subsumes the
  other. The **signed digest is the bridge**: a confidential guest's measured launch
  can pin (via `hostData`) the digest of the signed base image it boots from.
- **Offline / intermittent attestation for sovereign edge.** Because the signature
  travels *with* the artifact in the registry (and a Zot mirror travels with the edge
  site), an air-gapped node verifies provenance **locally** against a pinned key — no
  online round-trip. Caveat carried as an open question: cosign *keyless* verification
  needs Rekor/Fulcio (online), so the edge profile uses **key-based** cosign verify
  (or a mirrored TUF root), not the keyless CI path.

---

## 8. Consequences

**Positive**

- A single artifact substrate for the at-rest world: kernels (already), golden bases,
  snapshots, checkpoints — all OCI, all in the registry the cluster already runs.
- Content-addressed dedup of a shared golden base across many VM snapshots (the
  thin-overlay thesis), which the S3 backend's per-prefix SHA256 skip does **not**
  give cross-snapshot.
- Provenance/signing/SBOM for disk artifacts on the existing supply-chain spine,
  including offline verification for edge.
- Cross-cluster and air-gapped portability fall out of "the registry is the shared
  substrate," enabling cold/suspended-state migration where live migration cannot run.
- Near-zero new architecture: reuses capture, node-pinning, manifest, and the Tier-B
  restore tail; the new code is one Job-image + one enum value + dispatch cases.

**Negative / costs**

- A second durable backend to maintain next to `s3` (the spike must justify it earns
  its keep over Tier C, or we keep only one).
- Large blobs: a `memory-ranges` image can be multi-GiB per checkpoint; registry GC,
  quota, and many-large-blob tolerance (Zot/Harbor) become operational concerns.
- Dedup is not automatic, and the spike sharpened *where* it applies: a shared
  immutable **disk base** dedups almost perfectly (~97% of a second artifact skipped),
  but **memory snapshots do not dedup across VMs**, and because CH writes
  `memory-ranges` as one monolithic blob, even successive snapshots of the *same* VM
  dedup ~0 once RAM changes. So the **dedup pillar belongs to the golden-image path
  (§4.3), not the snapshot path** — sell OCI *snapshots* on portability + provenance +
  edge-consolidation. Incremental-snapshot dedup would need `memory-ranges` chunking or
  CH v52 sparse/dirty-page snapshots (follow-on, not a v1 blocker).
- The edge profile adds a component (a registry) operators must run and secure.
- Keyless cosign's online dependency complicates the air-gap story (key-based verify
  instead).

**Neutral**

- Live migration, the live disk path, Tier A (CSI snapshots), and Tier C (S3) are all
  unchanged; ORAS is purely additive.

---

## 9. Alternatives considered

1. **CSI snapshots alone (the shipped Tier A).** Disk-only, in-cluster,
   CSI-driver-bound. No cross-cluster portability, no memory checkpoint, no provenance,
   no cross-VM golden-base dedup. *Loses* on portability/provenance/dedup; it is the
   fast local-DR tier, not the durable/portable one. Kept; ORAS is its complement.
2. **A dedicated object store (S3/MinIO) — i.e., the shipped Tier C.** This is the
   sharpest comparison, because we already built it. It works and is simpler to reason
   about. But: no referrer graph (signing/SBOM become a side-channel), no
   content-addressed cross-snapshot dedup of a shared base, it sits *outside* the
   cosign supply-chain spine, and at the edge it is a *second* artifact store to run
   alongside the registry images already require. ORAS *wins* on
   provenance + dedup + consolidation; S3 *wins* on simplicity and for operators with
   an existing object-store investment and no registry appetite. **Decision: ship both;
   `s3` stays, `oci` is the recommended durable/portable/provenance-bearing backend.
   Not a forced migration.**
3. **Custom block-replication (DRBD / `zfs send` / rsync-of-raw).** Couples to a
   specific storage technology, is not portable across a heterogeneous estate, has no
   OCI ecosystem (no signing/dedup/distribution), and reinvents artifact distribution.
   *Loses* on portability + ecosystem + minimalism.
4. **Embed a registry in KubeSwift.** Rejected in §5 — stateful-service ownership, CVE
   cadence coupling, duplication of mature registries, against minimalism.

---

## 10. Non-goals (explicit)

- Backing a **live, mutable** VM disk with a registry blob. The hot read/write path
  stays on CSI / real block storage. **This is the defining non-goal.**
- Implementing, embedding, or operating a registry (the edge profile *runs an existing
  one*; it does not build one).
- Replacing CSI snapshots (Tier A) or forcing migration off the S3 backend (Tier C).
  ORAS is additive.
- **Live** migration over a registry. That remains the Cloud-Hypervisor streaming path;
  ORAS scope is cold/suspended-state migration only.
- A general-purpose artifact lifecycle / GC engine. The registry owns its GC and
  retention; KubeSwift declares `deletionPolicy` / `ttl` as it already does for `s3`,
  and at most issues deletes.
- Assuming online attestation. The edge must verify provenance offline.

---

## 11. Phased path (post-spike, indicative)

Mirrors how Tier C shipped (design → spike → feature PRs → cluster-walkthrough fixes):

1. **OCI backend for snapshot/restore** — enum + `OCIBackend` config + `snapshot-oras`
   image + dispatch cases; round-trip cluster-validated against Zot.
2. **Provenance** — cosign-sign the artifact post-push as an OCI referrer; surface the
   signature/verify state; document offline (key-based) verify for edge.
3. **Golden-image OCI source** — `SwiftImage.spec.source: oci`, materialize-then-clone.
4. **Cold/suspended-state migration** — compose `SwiftSnapshot(oci, includeMemory)` +
   `SwiftRestore(oci)`; decide §6.3 (compose vs `SwiftColdMigration` CRD) on the
   evidence.
5. **Edge profile** — Zot as the declared edge registry; `zot sync` mirror; air-gap
   runbook.

Each phase is independently shippable. The decision in this ADR commits to phase 1 and
the *direction* of 2–5, not their schemas.

---

## 12. Open questions

1. **One vs several CRDs for cold migration** (§6.3): compose existing
   `SwiftSnapshot` + `SwiftRestore`, or a thin `SwiftColdMigration`? Decide on
   cross-cluster orchestration evidence.
2. **OCI layer layout** — *spike-answered for the common case:* one blob per CH
   artifact works and round-trips byte-identically. Snapshot dedup is ~0 regardless of
   layout because `memory-ranges` is monolithic; the open sub-question is now only
   whether to pursue **incremental-snapshot dedup** (content-defined chunking of
   `memory-ranges`, or riding CH v52 sparse/dirty-page snapshots) — a follow-on, not a
   v1 requirement.
3. **Golden-base dedup model** — *spike-confirmed:* a shared base as its own OCI layer
   dedups by digest (~97% of the second artifact skipped). The remaining decision is
   whether the golden base is a *separate pullable artifact the overlay references*
   (true thin-overlay) vs a base layer duplicated in each artifact's manifest (dedups
   in the registry but re-listed per artifact) — the former is the product goal for the
   `SwiftImage.spec.source: oci` path (§4.3).
4. **Credential model** — registry auth via dockerconfigjson pull-secret (reuse
   SwiftKernel's `OCIRef.PullSecret`) vs the S3 access-key Secret. Lean: pull-secret.
5. **Offline keyless verification** — Rekor/Fulcio are online; the edge profile needs
   key-based cosign verify or a mirrored TUF root. Confirm the offline verify story.
6. **Registry Referrers-API compatibility matrix** — the referrer graph is load-bearing
   for signing/SBOM discovery. Confirm support across the candidate registries (Zot,
   Harbor, GHCR, ECR) before treating §7 as guaranteed.
7. **Large-blob tolerance** — multi-GiB `memory-ranges` blobs and registry quota / GC
   behavior (Zot, Harbor). Validate before recommending memory checkpoints at scale.
8. **`oras-go/v2` vs the ORAS CLI** — the decision picks the **SDK** (consistency with
   `snapshot-s3` embedding minio-go). SwiftKernel's CLI-in-Job is the older idiom;
   confirm the SDK covers push/pull/attach/discover with the auth and resume semantics
   the transfer Job needs (the spike validates this).

---

*🤖 Generated with [Claude Code](https://claude.com/claude-code)*
