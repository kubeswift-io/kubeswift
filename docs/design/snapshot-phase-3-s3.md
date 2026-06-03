# Snapshot Phase 3 — Tier C / S3 object-storage export — Design

> Status: design-draft (2026-06-03). Builds on Snapshots Phases 0/1/2 (SHIPPED):
> SwiftSnapshot/SwiftRestore CRDs, the `csi-volume-snapshot` (Tier A) and `local`
> (Tier B) backends, the swiftletd CH pause/snapshot/resume action loop.
>
> Phase 3 adds the **`s3` backend** (already reserved in the CRD enum +
> `S3Backend` struct): capture to the node-local hostPath (Tier B), then upload
> to S3-compatible object storage; restore downloads from S3 to a node-local
> cache, then restores via the existing Tier B path.

## 1. Goal & non-goals

**Goal.** A SwiftSnapshot with `backend.type: s3` exports a VM snapshot (disk +
optionally memory) to an S3-compatible object store, and a SwiftRestore with an
`s3`-backed source pulls it back and restores it — **on any node, any cluster**.
This is the cluster-portable, off-cluster-durable backup tier (the design's "Tier
C").

**Non-goals.**
- **No new capture mechanism.** Tier C reuses Tier B's swiftletd CH
  pause/snapshot/resume to produce the node-local artifacts; S3 is purely the
  transport+durability layer on top. (Disk-only Tier C — capturing the disk via
  the CSI path then exporting — is a possible later option; Phase 3 ships the
  Tier-B-derived memory+disk export, matching the design doc's Tier C.)
- **No cross-tier auto-promotion** (Tier B → C "promote" ergonomics) — that is a
  separate ergonomic follow-up.
- **No incremental/dedup upload** — Phase 3 uploads the full artifact set per
  snapshot. Incremental is a Phase 5 optimization.
- **No client-side encryption** in Phase 3 — rely on S3 server-side encryption
  (SSE-S3/SSE-KMS configured on the bucket) + TLS in transit. Client-side
  envelope encryption is a tracked follow-up.

## 2. Backend model — capture-then-upload, download-then-restore

```
CAPTURE (SwiftSnapshot, backend.type=s3)
  Pending -> Capturing -> Uploading -> Ready
    Capturing : Tier B local capture (swiftletd CH pause/snapshot/resume) to
                /var/lib/kubeswift/snapshots/<ns>-<snap>/ on the guest's node N
    Uploading : an upload Job, PINNED to node N, mounts that hostPath + the S3
                credentials Secret, pushes the artifact set to
                s3://<bucket>/<prefix>/<ns>/<snap>/, writes a manifest, then
                (optional) prunes the local copy
    Ready     : status records the S3 location + the manifest digest

RESTORE (SwiftRestore from an s3-backed SwiftSnapshot)
  Pending -> Downloading -> Restoring -> Resuming -> Ready
    Downloading : a download Job, PINNED to the restore-target node M, pulls
                  s3://.../<snap>/ to /var/lib/kubeswift/snapshots/<ns>-<snap>/
                  on node M (the node-local cache), verifying the manifest digest
    Restoring/Resuming : the EXISTING Tier B (local) restore path takes over
                  unchanged — CH --restore from the node-local cache
```

The key invariant (the design crux): **the local hostPath cache is node-pinned;
S3 is the only cross-node/cross-cluster layer.** So the upload Job pins to the
capture node (where Tier B wrote the artifacts) and the download Job pins to the
restore-target node (where CH will `--restore`). Section 6 details the pinning.

## 3. CRD surface

The `S3Backend` struct already exists; Phase 3 extends it and adds a restore
source + status fields.

```go
type S3Backend struct {
    Bucket               string  // required
    Region               string  // required for AWS; optional for some S3-compatible
    Prefix               string  // optional key prefix; objects land at <prefix>/<ns>/<snap>/
    Endpoint             string  // NEW: S3-compatible endpoint (MinIO/Ceph RGW); empty = AWS
    ForcePathStyle       bool    // NEW: path-style addressing (MinIO/RGW typically need true)
    CredentialsSecretRef *SecretObjectReference // Secret with access keys (§5)
    // RetainLocalCache bool   // NEW (optional): keep the node-local copy after upload (default false -> prune)
}
```

Status additions (SwiftSnapshotStatus): `s3.location` (`s3://bucket/key/`),
`s3.manifestDigest` (sha256 of the manifest), `observedUploadBytes`,
`uploadCompletedAt`. SwiftRestore gains a `Downloading` phase + `downloadedBytes`.

`includeMemory` is honoured for s3 exactly as for Tier B (memory captured at
Capturing time); a disk-only s3 snapshot (`includeMemory: false`) exports just
the disk artifacts.

## 4. Artifact layout + manifest

Node-local (Tier B output, unchanged) and the S3 mirror share one layout:

```
s3://<bucket>/<prefix>/<ns>/<snap>/
  manifest.json        # the source of truth for restore (see below)
  config.json          # CH VM config (Tier B captures this)
  memory.img           # present iff includeMemory (CH memory snapshot)
  disks/
    root.raw           # the raw root disk (and dataDisk-*.raw for secondaries)
```

`manifest.json` (written by the upload Job, verified by the download Job):

```json
{
  "schemaVersion": 1,
  "swiftSnapshot": "ns/snap",
  "createdAt": "<stamped by controller>",
  "includeMemory": true,
  "hypervisorVersion": "cloud-hypervisor v51.1",
  "artifacts": [
    {"path": "config.json", "bytes": 4096, "sha256": "..."},
    {"path": "memory.img",  "bytes": 4294967296, "sha256": "..."},
    {"path": "disks/root.raw", "bytes": 42949672960, "sha256": "..."}
  ],
  "totalBytes": 47244640256
}
```

Per-artifact sha256 lets the download Job verify integrity and lets restore fail
loudly on a truncated/corrupt object rather than booting a broken guest (Design
Principle #6, no silent failures). The controller stamps timestamps (scripts
cannot use Date.now-equivalents deterministically; the controller owns them).

## 5. Credentials & security

- **Credentials in a Secret**, referenced by `credentialsSecretRef` (same
  namespace as the SwiftSnapshot). Keys: `accessKeyId`, `secretAccessKey`,
  optional `sessionToken`. The upload/download Job mounts the Secret as env
  (`AWS_ACCESS_KEY_ID` etc.) — never as annotations, never logged (S1 trust-
  boundary discipline carried from migration).
- **TLS in transit** to the S3 endpoint (https); the Job rejects plaintext
  endpoints unless an explicit opt-in (mirrors the migration plaintext-ack
  posture). Server-side encryption (SSE) is a bucket-policy concern, documented
  for operators.
- **Least privilege**: the Job needs only `s3:PutObject`/`GetObject`/`ListObject`
  on `<bucket>/<prefix>/*`. Documented IAM/bucket-policy snippet.
- **Minimal container**: the uploader runs `drop: ALL`, non-root,
  `readOnlyRootFilesystem`, no extra caps (it only reads a hostPath + does
  network I/O). It mounts the node hostPath **read-only** for upload.

## 6. Node-affinity — the crux

- **Upload Job** must run on the node where Tier B wrote the artifacts. The
  SwiftSnapshot status (from Tier B) records the capture node; the upload Job is
  pinned there via `spec.nodeName` (the same direct-binding pattern migration
  uses) or a `kubernetes.io/hostname` nodeSelector. The hostPath
  (`/var/lib/kubeswift/snapshots/...`) is mounted read-only.
- **Download Job** pins to the **restore-target node** — for an in-place restore,
  the guest's current node; for a clone/cross-node restore, the node the restored
  guest will run on (the SwiftRestore's target). It writes the node-local cache
  the existing Tier B restore then consumes.
- **Edge case — capture node gone:** if the capture node is drained/dead before
  upload, the node-local artifacts are lost and the s3 export cannot proceed (the
  Job can't schedule there). The controller surfaces this as a clear Failed
  ("capture node <N> unavailable for upload"); operators re-snapshot. (A future
  option: capture to RWX storage instead of hostPath so any node can upload — a
  Phase 5 portability improvement, out of scope here.)

## 7. The uploader/downloader — a Go image (`snapshot-s3`)

Decision (spike-confirm §10.1): a **small Go binary image** using `minio-go`
(works against AWS S3, MinIO, Ceph RGW — the S3-compatible matrix), NOT an
off-the-shelf `aws-cli`/`rclone`/`mc` container. Rationale: matches the project's
Go-everything + minimal-image + minimal-caps ethos (cf. `snapshot-stager`,
`gpu-discovery`), gives full control over the manifest + per-artifact checksums +
streaming multipart upload of multi-GB memory images, and avoids a shell+CLI
attack surface. The binary has two modes (`--mode=upload|download`) + flags for
bucket/prefix/endpoint/path-style; creds via env. Plugs into the image build
matrix alongside `snapshot-stager`.

Streaming + resumption: multi-GB memory images use multipart upload; the binary
is idempotent (re-run checks which objects already exist with matching size/etag
and skips them), so a re-scheduled Job resumes rather than restarts.

## 8. State machine wiring

- **swiftsnapshot controller** (`controller.go` backend dispatch already has the
  `case SnapshotBackendS3` slot returning "not implemented"): for s3, run the
  existing Tier B Capturing to completion, then transition to a new `Uploading`
  phase that creates+watches the upload Job, then `Ready`. New file `s3.go`
  mirrors `local.go`.
- **swiftrestore controller**: for an s3-sourced restore, a new `Downloading`
  phase creates+watches the download Job, then hands off to the existing local
  restore path (`local.go`) which is source-agnostic once the node-local cache
  exists.
- **cleanup/finalizer**: deleting an s3 SwiftSnapshot with `deletionPolicy:
  Delete` runs a delete Job (or the controller's S3 client) to remove
  `<prefix>/<ns>/<snap>/`; `Retain` leaves the objects. Mirrors the Tier A/B
  deletion-policy handling.

## 9. Testing & validation

- **Unit:** manifest build/verify, the Job spec builders (node-pinning, Secret
  env, read-only hostPath), the controller phase transitions
  (Capturing→Uploading→Ready; Downloading→Restoring), deletion-policy.
- **Cluster (MinIO):** stand up an in-cluster MinIO as the S3 endpoint (no AWS
  dependency for CI/dev). Round-trip: snapshot a guest → s3 export → delete the
  guest → SwiftRestore (clone) on the OTHER node from s3 → guest boots, sentinel
  survives. Plus a disk-only (`includeMemory:false`) round-trip and a
  deletion-policy check.
- **The W5 discipline:** the round-trip MUST run on the cluster (not just unit
  tests) before claiming Phase 3 shipped — the snapshot history (Tier A silent
  data-loss, PR #21) shows backup/restore correctness is exactly where unit tests
  mislead.

## 10. Open questions / spike targets

1. **Uploader tool — minio-go vs off-the-shelf.** Lean minio-go (§7); spike: a
   minimal multipart upload of a multi-GB file to in-cluster MinIO, confirm
   streaming + resumption + checksums.
2. **Capture-node-gone portability.** Phase 3 fails loudly (§6); confirm the
   operator UX is acceptable vs. investing in RWX-staged capture now.
3. **Memory image size / cost.** A 4Gi guest's memory.img is ~4 GiB per snapshot;
   document the S3 cost/bandwidth profile + recommend disk-only s3 for routine
   backups, memory-included for DR.
4. **Cross-cluster restore.** s3 makes the artifacts cluster-portable; the
   restore needs the SwiftImage/kernel/seed to resolve on the target cluster.
   Document the cross-cluster prerequisites (or scope Phase 3 to same-cluster
   cross-node, with cross-cluster as a documented manual flow).
5. **Endpoint TLS + SSE posture** — the plaintext-endpoint opt-in gate + the
   SSE-bucket documentation.

## 11. PR plan

1. **PR 1 — this design doc** + the CRD surface (S3Backend Endpoint/ForcePathStyle,
   status fields, the SwiftRestore Downloading phase) + `make generate`. No
   behavior yet.
2. **PR 2 — `snapshot-s3` uploader/downloader image** (Go + minio-go, upload &
   download modes, manifest + checksums, multipart, idempotent-resume) + unit
   tests against a mocked/MinIO target. Image build-matrix wiring.
3. **PR 3 — swiftsnapshot s3 capture path**: `s3.go`, the Uploading phase, the
   node-pinned upload Job, status + manifest, deletion-policy. Unit-tested.
4. **PR 4 — swiftrestore s3 path**: the Downloading phase, the node-pinned
   download Job + manifest verification, hand-off to the local restore.
   Unit-tested.
5. **PR 5 — cluster round-trip (in-cluster MinIO)** + operator runbook
   (`docs/snapshots/s3-snapshots.md`: backend config, the credentials Secret, IAM
   policy, cross-node/cross-cluster restore, cost guidance) + sample manifests.

## 12. Phases 4 & 5 (scoped, not detailed here)

- **Snapshot Phase 4 — cloneFromSnapshot ergonomics.** SwiftGuestPool templating
  from a snapshot (walkthrough Scenario 7 demand): a pool whose replicas clone
  from a SwiftSnapshot (Tier B or, post-Phase-3, Tier C). Mostly controller
  ergonomics over the existing restore primitives; its own design pass.
- **Snapshot Phase 5 — operational polish.** Mirrors the migration Phase 5 just
  shipped: Prometheus metrics (`kubeswift_snapshot_total{backend,result}`,
  `kubeswift_snapshot_bytes`, capture/upload durations), a Grafana dashboard,
  retention/GC of terminal SwiftSnapshots, and (stretch) incremental upload. The
  migration Phase 5 PRs (#105–#107) are the template.
