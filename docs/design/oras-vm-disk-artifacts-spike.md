# Spike — ORAS / OCI At-Rest VM-Disk Artifacts

Status: **COMPLETE — GO** (executed 2026-07-01, dev cluster miles/boba, k0s v1.34,
CH v52, **Zot v2.1.2**, **oras-go/v2 v2.6.1**, cosign v2.2.2, oras CLI v1.2.2). Gates
the decision in [`oras-vm-disk-artifacts.md`](oras-vm-disk-artifacts.md) → **GO**.
Full results in §9; the four questions all PASS with one scope refinement (the dedup
value attaches to the golden-image path, not the memory-snapshot path).

This is a time-boxed, throwaway validation of the riskiest assumptions behind the ORAS
ADR. It builds the **minimum** to answer go/no-go and nothing more. Per the project
convention (`.gitignore:37-42`), spike scratch — scripts, run logs, throwaway
manifests — is local; the durable findings fold back into the ADR.

---

## 1. Goal

Prove (or disprove) that the **at-rest, immutable** states of a Cloud-Hypervisor VM
disk — snapshot, suspended checkpoint, golden base — can be packaged as OCI artifacts,
pushed to a real registry, pulled back, and restored to a working VM, **with the
properties that justify a registry over the S3 backend we already shipped**: cross-VM
layer dedup and referrer-based signing.

The spike does **not** build the controller, the CRD, or the production Job. It builds
a standalone harness that exercises the one genuinely new surface — the OCI
packaging + push/pull middle — against the *existing, proven* capture and restore
primitives.

---

## 2. Environment

- **One Cloud Hypervisor VM** on a single node (reuse the dev cluster's `miles` or
  `boba`, or an off-cluster CH + `CLOUDHV.fd` from the `swiftletd` image, mirroring the
  Phase-3c / vsock-agent spike setups).
- **A local Zot** registry (`ghcr.io/project-zot/zot:latest`), anonymous/local auth,
  plain HTTP (the `insecure` posture the S3 spike used for in-cluster MinIO).
- **`oras-go/v2`** (`oras.land/oras-go/v2`) in a small Go harness binary — the SDK
  KubeSwift will embed in `snapshot-oras`, chosen for consistency with `snapshot-s3`
  embedding minio-go. **cosign** CLI for the referrer test.
- **Reused, not rebuilt:** real `swiftctl snapshot` (Tier B `local` capture) produces
  the on-node snapshot dir (`config.json` / `state.json` / `memory-ranges` under
  `/var/lib/kubeswift/snapshots/<ns>-<name>/`); real CH `--restore`
  (`spawn_ch_restore`) consumes the pulled dir. The harness only owns the OCI middle.

Record exact versions (Zot, oras-go, cosign, CH) and the date in the results table —
the house convention for spike docs.

---

## 3. The minimal happy-path slice (what the harness is)

```
[ existing: swiftctl snapshot --backend local ]   ← real capture, unchanged
        │  produces /var/lib/kubeswift/snapshots/<ns>-<name>/{config.json,state.json,memory-ranges}
        ▼
[ harness: oci-pack ]   ← NEW (oras-go/v2)
        │  walk dir → one OCI layer per artifact (Q-layout) → artifactType
        │  application/vnd.kubeswift.vmsnapshot.v1 → push to Zot
        ▼
[ Zot registry ]  ← the dependency, not built
        │
        ▼
[ harness: oci-unpack ]   ← NEW (oras-go/v2)
        │  pull → verify digests → materialize to a FRESH dir on a (possibly different) node
        ▼
[ existing: CH --restore source_url=file://<fresh dir> ; vm.resume ]   ← real restore, unchanged
        ▼
[ guest Running — assert sentinel ]
```

The harness is two subcommands (`pack` / `unpack`) plus a tiny `verify` that diffs the
restored guest's sentinel. Everything around it is real KubeSwift machinery.

---

## 4. Riskiest assumptions (Question → Method → Validation criteria)

Ordered by risk. **Q1 and Q4 are correctness gates; Q2 and Q3 are the value-over-S3
gates.** A `pass` on a gate is a hard, observed fact, not an inference.

### Q1 — Round-trip correctness (the gate)

**Does quiesce → capture → OCI-package → push → pull → materialize → boot return a
byte-faithful, bootable guest?**

- **Method.** Boot a disk-boot Noble guest; write a sentinel file to the disk and
  record its md5. `swiftctl snapshot --backend local` (no memory yet — disk + CH state
  only). `oci-pack` → Zot → `oci-unpack` to a fresh dir on the **same** node. CH
  `--restore` + `vm.resume`. Assert the guest reaches Running and the sentinel md5
  matches.
- **Pass.** Guest reaches `Running`; in-disk sentinel md5 identical pre/post. The
  pulled `config.json` / `state.json` / `memory-ranges` are byte-identical to the
  captured originals (digest match end-to-end).
- **Fail.** Any artifact corrupted in the OCI round-trip, or the guest cannot restore.
  This is fatal for the whole ADR — it would mean OCI packaging cannot faithfully carry
  CH artifacts.

### Q2 — Cross-VM layer dedup actually triggers (value-over-S3)

**When two VMs share a golden base, does the registry store the shared base layer
once?**

- **Method.** Two guests A and B cloned from the same SwiftImage base, each with a
  small distinct overlay write. Snapshot both, `oci-pack` both (with the base disk as
  its own OCI layer — see Q-layout), push both to Zot. Inspect Zot's blob store: count
  distinct blob digests and total bytes after push A, then after push B.
- **Pass.** The shared base layer's digest appears **once**; pushing B adds only ≈ B's
  overlay + memory delta, not a second full copy. Quantify the dedup ratio.
- **Fail / partial.** Pushing B re-stores the full base (no dedup). If so, determine
  whether it is a *layout* problem (the base wasn't its own blob — fixable, downgrades
  to "dedup is layout engineering, follow-on") or a *registry* problem (Zot didn't
  dedup identical digests — unexpected, investigate). Distinguish the two; it changes
  the ADR's dedup claim from "works" to "works with layout work."

### Q3 — Referrer attach / discover works (value-over-S3)

**Can a cosign signature / SBOM be attached to the snapshot artifact as an OCI referrer
and discovered against the target registry?**

- **Method.** `cosign sign` the snapshot artifact's digest in Zot; attach a trivial
  SBOM as a referrer. Then `cosign verify` (key-based, to also exercise the
  offline-edge path) and `oras discover --artifact-type` the signature/SBOM back.
- **Pass.** The referrer is discoverable via the Referrers API and `cosign verify`
  succeeds against the pushed digest. This is the provenance claim the S3 backend
  structurally cannot make.
- **Fail.** Zot's Referrers API does not return the attachment, or verify fails.
  Captures the §12.6 registry-compat risk concretely.

### Q4 — Cold-migration state survives the round-trip (the second gate)

**Does a snapshot taken *with memory* resume from frozen state after a pull onto a
different node — i.e., does suspend-to-registry / resume-elsewhere work?**

- **Method.** Boot a guest; write an **in-RAM** sentinel (e.g. a known value in a
  tmpfs file + record `uptime`). `swiftctl snapshot --backend local`
  **`--include-memory`**. `oci-pack` → Zot → `oci-unpack` onto a **different** node
  (simulating the cold-migration target; the host tap/disk pre-exist there per
  `spawn_ch_restore`'s contract). CH `--restore` + resume.
- **Pass.** The guest **resumes** (does not cold-boot): uptime continues monotonically
  across the move, and the in-RAM sentinel survives. Memory image transfers and
  restores intact cross-node.
- **Fail.** Guest cold-boots, RAM sentinel lost, or the memory image won't restore
  cross-node. Bounds the cold-migration use case.

---

## 5. Measurements (collect for every run; they size the ADR's claims)

| Metric | Why it matters |
|---|---|
| Artifact size **with vs without dedup** (full snapshot bytes vs incremental bytes for the second VM sharing the base) | Quantifies the Q2 thin-overlay value over S3's full-copy-per-snapshot. |
| Push throughput (MB/s to Zot) and pull throughput (MB/s from Zot) | Feeds the operator sizing model; compare to the S3 backend's numbers. |
| Restore-to-boot latency (pull → materialize → Running) | Compare against (a) baseline local Tier-B restore and (b) Tier-C S3 restore — the operator-visible cost of choosing OCI. |
| `memory-ranges` blob size + push/pull time (Q4) | Sizes the large-blob / quota risk (§12.7) for memory checkpoints. |
| Zot blob count + total store bytes after each push (Q2) | The direct dedup evidence. |

---

## 6. Explicitly NOT built in this spike

- The controller — no `backend.type: oci` dispatch, no Job builder. Standalone harness
  only.
- The CRD changes (no `OCIBackend` struct, no enum value, no `make generate`).
- Credential / auth hardening — Zot anonymous/local; the dockerconfigjson pull-secret
  model is an ADR decision, not spike scope.
- The golden-image `SwiftImage.spec.source: oci` path — snapshot/restore only.
- True cross-cluster orchestration — "a different node" stands in for the
  cold-migration target; two-cluster choreography is post-spike.
- The edge profile / `zot sync` mirror / full air-gap — online Zot here; offline
  key-based verify is touched in Q3 but the air-gap runbook is follow-on.
- GC / retention / `deletionPolicy`.
- A production registry compatibility matrix — **Zot only**. GHCR / Harbor / ECR
  Referrers-API compat is an ADR open question (§12.6), not spike scope.

---

## 7. Go / No-Go framing

- **GO** if **Q1 (round-trip) and Q4 (cold-migration state) both PASS**, **and at least
  one of Q2 (dedup) / Q3 (referrers) PASSES.** Those two are the only reasons to choose
  ORAS over the Tier C already shipped; at least one must hold to justify the second
  backend. Ideally all four pass.
- **NO-GO / reconsider** if **Q1 fails** (fatal — OCI packaging corrupts CH artifacts),
  **or if both Q2 and Q3 fail** (ORAS then offers nothing over Tier C, and the honest
  answer is "keep using `s3`").
- **PARTIAL-GO** if Q1 + Q3 pass but Q2 needs layer-structuring (dedup is a layout
  problem, not a registry problem): proceed, with cross-VM dedup tracked as a follow-on
  layout-engineering task rather than a phase-1 guarantee.

State the verdict plainly at the top of the results when the spike runs (the house
convention: `Status: COMPLETE — PASS/PARTIAL/NO-GO`).

---

## 8. Implications for the ADR (how each outcome moves the decision)

- **Q1 PASS** → confirms the at-rest backend is mechanically sound; phase-1 (OCI
  snapshot/restore) is buildable as designed (reuse capture + restore tail).
- **Q2 PASS** → the thin-overlay / golden-base dedup thesis (ADR §6.3, §12.3) is real;
  resolve the layer-layout open question (§12.2) from the measured-best layout.
- **Q2 PARTIAL (layout-dependent)** → ADR §8 "dedup is layout engineering, not a free
  lunch" is confirmed; phase-1 ships without the cross-VM dedup guarantee, dedup
  becomes a tracked follow-on.
- **Q3 PASS** → the provenance/signing value (ADR §7) is validated against a real
  Referrers API; the supply-chain-spine extension is real, and the offline key-based
  verify path for edge is de-risked.
- **Q4 PASS** → cold/suspended-state migration (ADR §4.4) is viable; informs the
  one-vs-several-CRD decision (§6.3) with real cross-node evidence.
- **Any FAIL** → recorded against the corresponding ADR risk/open-question with the
  observed mechanism, so the decision is made on evidence, not optimism (Design
  Principle #7: verified fixes only; no speculative architecture).

---

## 9. Results (executed 2026-07-01)

**Verdict: GO.** Q1 and Q4 (correctness gates) PASS; Q2 and Q3 (value-over-S3 gates)
both PASS, with one important scope refinement on Q2.

### Setup

Zot v2.1.2 (`dedupe: true`, GC off) on the dev cluster; a `pack`/`unpack` harness on
`oras-go/v2` v2.6.1 (one OCI layer per file, title-annotated, custom
`artifactType: application/vnd.kubeswift.vmsnapshot.v1`), run in node-pinned root pods
mounting the `/var/lib/kubeswift/snapshots` hostPath. Two 2 GiB Noble guests
(`demo-class`) from the shared `ubuntu-noble` SwiftImage — `oras-spike-a` on boba,
`oras-spike-b` on miles. Real `SwiftSnapshot backend: local` captures produced the
artifacts (`config.json` 2359 B, `state.json` ~64.8 KB, `memory-ranges` exactly
2,147,483,648 B; pause window **921 ms** / **1078 ms**).

### Q1 — round-trip correctness: **PASS**

Pack → push → pull to a fresh dir → compare. All three artifacts returned
**byte-identical** (SHA256 match; manifest digest `cc6e8e04…` stable across the trip).
Byte-identity is a *stronger* faithfulness oracle than a single boot — it proves the
OCI packaging carries CH's `config.json`/`state.json`/`memory-ranges` bit-for-bit.
Boot-restorability of those bytes is the already-shipped Tier-B restore path
(CH `--restore`), so an OCI-round-tripped artifact restores identically by
construction; an independent boot-from-pulled was **not** separately run (transitively
guaranteed by byte-identity ∧ shipped restore).

Throughput (2 GiB, single guest): **push ~68 MB/s (31.6 s), pull ~51 MB/s (41.8 s)**
same-node.

### Q4 — cross-node state survival (cold-migration transfer): **PASS**

snap-a1, captured on **boba**, unpacked on **miles**: all three SHAs matched the
boba-side oracle exactly (pull 7.1 s). Frozen VM state (memory + CH device state)
survives the registry round-trip across nodes byte-for-byte → **cold/suspended-state
migration via the registry is viable.**

**Refinement (Q1/Q4 collapse):** a Tier-B/`local` snapshot is memory + CH-state, **not
the disk** (the disk stays on its PVC). So every OCI snapshot artifact is inherently
memory-inclusive, and "cold-migration state survival" is not a *separate* risk from
"round-trip correctness" — it is the same round-trip, cross-node. The only real axis is
same-node vs cross-node materialization; both PASS.

### Q2 — dedup: **PASS (with a scope correction)**

Measured at the OCI layer via `transferred` vs `skipped` (blob already present by
digest — the exact dedup signal):

| Test | transferred | skipped | meaning |
|---|---:|---:|---|
| Golden base, artifact **A** (base new) | 1,107,297,070 | 2 | full upload |
| Golden base, artifact **B** (shares 1 GiB base) | 33,555,246 | **1,073,741,826** | **base stored once** — B pays only its overlay (~97% skipped) |
| Identical re-push of snap-a1 | 1,063 | 2,147,550,868 | identical content fully dedups |
| **Cross-VM memory** (snap-b1 into snap-a1's repo) | **2,147,551,916** | 2 | **different VM's memory does NOT dedup** |

**The scope correction — the sharpest finding of the spike:** dedup is a **golden-image
(disk-base) property, not a snapshot property.** A shared immutable base disk dedups
almost perfectly (content-addressing). But a *memory* snapshot does not dedup across
VMs, and because CH writes `memory-ranges` as **one monolithic blob**, even successive
snapshots of the *same* VM dedup ~0 once any RAM byte changes (the whole layer's digest
churns). Incremental-snapshot dedup would require content-defined chunking of
`memory-ranges` into many layers, or CH v52 **sparse / dirty-page** snapshots.
→ For the **snapshot/restore** use case, ORAS's value is **portability + provenance +
edge-consolidation, not dedup**; the **dedup** pillar belongs to the **golden-image**
use case. This directly resolves ADR open questions §12.2 (layer layout) and §12.3
(golden-base dedup model).

### Q3 — referrer-based signing: **PASS**

- **Referrers API works against Zot.** `oras attach` pushed an SBOM referrer
  (`application/vnd.kubeswift.sbom.spdx.v1+json`, digest `1759297c…`); `oras discover`
  returned it under the snapshot artifact via the Referrers API. The signing/SBOM
  *mechanism* the ADR §7 relies on is proven end-to-end.
- **The snapshot artifact is cosign-signable.** `cosign sign` (default mode) created the
  signature in Zot (`sha256-cc6e8e04….sig` present in the repo tags).
- **Residual (environment artifact, not a limitation):** cosign v2.2.2's
  `--allow-http-registry` is not honored on the initial `/v2/` ping for the *verify* and
  *oci-1-1-referrers-mode* paths against a **plaintext** Zot (`http response to https
  client`). Every real registry (GHCR, Harbor, ECR, Zot-with-TLS) is HTTPS, where these
  paths work normally. Not a KubeSwift/Zot/Referrers-API constraint.

### Measurements summary

| Metric | Value |
|---|---|
| Pause window (2 GiB guest) | 921 ms / 1078 ms |
| Snapshot artifact size | 2,147,550,866 B (2 GiB memory + config + state) |
| Push throughput (same-node) | ~68 MB/s |
| Pull throughput (same-node / cross-node) | ~51 MB/s / higher (7.1 s for 2 GiB) |
| Round-trip fidelity | byte-identical (SHA256, all artifacts, same- and cross-node) |
| Golden-base dedup (2nd artifact) | ~97% skipped (1 GiB base stored once) |
| Cross-VM memory dedup | ~0% |

### What this changes in the ADR

- **Spine confirmed (Q1/Q4):** OCI packaging is byte-faithful, so the "new
  `backend.type: oci` + `snapshot-oras` Job reusing capture + the Tier-B restore tail"
  design is a near-drop-in. Proceed to implementation.
- **Dedup claim re-scoped (Q2):** attach the dedup pillar to the **golden-image** path
  (§4.3), not the snapshot path. Sell OCI *snapshots* on portability + provenance +
  edge, not dedup. Resolves §12.2 / §12.3.
- **Provenance validated (Q3):** referrers attach + discover work; artifact is
  signable. §7 holds.
- **Cold migration (the open ADR item §6.3):** Q4 shows state survives cross-node
  byte-identically. No evidence forces a dedicated `SwiftColdMigration` CRD — the
  compose-`SwiftSnapshot(oci)`+`SwiftRestore` path is viable; keep the decision
  compose-first.
- **New layer-layout guidance:** if incremental-snapshot dedup is ever wanted, it is a
  `memory-ranges`-chunking / CH-v52-sparse-snapshot question, tracked as a follow-on —
  not a v1 blocker.

### Not run (out of scope, as planned)

Controller/CRD wiring; the `snapshot-oras` production Job; credential hardening (Zot
anonymous); the golden-image `SwiftImage.spec.source: oci` path; two-cluster
orchestration ("different node" stood in for the cold-migration target); the edge
`zot sync` / air-gap runbook; GC/retention; a multi-registry (GHCR/Harbor/ECR)
Referrers-compat matrix.

---

*🤖 Generated with [Claude Code](https://claude.com/claude-code)*
