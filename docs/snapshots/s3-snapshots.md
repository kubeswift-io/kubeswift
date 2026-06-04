# S3 Snapshots (Tier C)

Tier C exports a full VM snapshot (disk + optionally memory) to an
**S3-compatible object store** and restores it back **on any node, any
cluster**. This is the cluster-portable, off-cluster-durable backup tier.

Use Tier C when:

- You want backups that survive the loss of the capture node (Tier B
  snapshots live on one node's hostPath; if that node dies, the snapshot
  is gone).
- You want to restore onto a *different* node than the one that captured —
  e.g. clone a golden VM across the cluster, or recover after a node
  failure.
- You want off-cluster durability (object storage outlives the cluster).

For same-node memory snapshots with the lowest capture overhead, use
`backend: local` (Tier B — [local-snapshots.md](local-snapshots.md)). For
disk-only CSI snapshots, use `backend: csi-volume-snapshot` (Tier A —
[csi-snapshots.md](csi-snapshots.md)).

## How it works

Tier C is **capture-then-upload, download-then-restore**. It reuses the Tier
B node-local capture machinery and adds an object-storage hop on each side:

```
CAPTURE (SwiftSnapshot, backend.type: s3)
  Pending ──▶ Capturing ──▶ Uploading ──▶ Ready
              (launcher      (node-pinned    (status.s3.location
               captures to    upload Job      = s3://bucket/key/)
               node cache)    → S3)

RESTORE (SwiftRestore from an s3-backed SwiftSnapshot)
  Pending ──▶ Downloading ──▶ Restoring ──▶ Resuming ──▶ Ready
              (node-pinned     (existing Tier B path: stamp/clone the
               download Job     target SwiftGuest, CH --restore from the
               → node cache)    node cache, resume)
```

S3 is the only cross-node layer. The capture's node-local cache and the
restore's node-local cache are staging dirs under
`/var/lib/kubeswift/snapshots/<namespace>-<name>/` — the durable copy is in
the bucket. The upload Job pins to the capture node (where the artifacts were
written); the download Job pins to the **restore-target node** (where CH will
`--restore`).

### Object layout

Artifacts land under `<prefix>/<namespace>/<snapshot>/` in the bucket:

```
demo/default/snapshot-s3-mem/
  config.json          # CH's view of the VM
  state.json           # CPU/device/regs
  memory-ranges        # serialized RAM (only when includeMemory: true)
  manifest.json        # sha256 + size of every artifact — written LAST
```

`manifest.json` is the upload-complete sentinel and the restore's source of
truth: it is uploaded last (so a partial upload is detectable) and the
download Job verifies every artifact's sha256 + size against it before the
restore proceeds. A checksum or size mismatch fails the download loudly.

## Prerequisites

1. **An S3-compatible object store** reachable from the cluster — AWS S3,
   MinIO, or Ceph RGW. For a quick in-cluster test store, apply
   [`config/samples/s3-snapshots/00-minio.yaml`](../../config/samples/s3-snapshots/00-minio.yaml)
   (demo-grade: single replica, emptyDir, `minioadmin`/`minioadmin`).
2. **The bucket must already exist** — the controller does not create it.
3. **A credentials Secret** in the SwiftSnapshot's namespace with keys
   `accessKeyId`, `secretAccessKey`, and optional `sessionToken`
   ([`01-s3-creds.yaml`](../../config/samples/s3-snapshots/01-s3-creds.yaml)).
4. **The `snapshot-s3` image** must be configured on the controller via the
   `KUBESWIFT_SNAPSHOT_S3_IMAGE` env var (the Helm chart / `make deploy` set
   this). If unset, s3 snapshots fail with *"snapshot-s3 image not
   configured"*.

## Quick start

```bash
# In-cluster test store (skip if you have a real S3 endpoint).
kubectl apply -f config/samples/s3-snapshots/00-minio.yaml
# create the bucket once, e.g. via the MinIO console (:9001) or mc:
#   mc alias set local http://minio.minio.svc:9000 minioadmin minioadmin
#   mc mb local/kubeswift-snapshots

kubectl apply -f config/samples/local-snapshots/01-seed-profile.yaml   # shared seed
kubectl apply -f config/samples/s3-snapshots/01-s3-creds.yaml
kubectl apply -f config/samples/s3-snapshots/02-source-guest.yaml
# wait for snapshot-s3-source to reach Running, then:
kubectl apply -f config/samples/s3-snapshots/03-snapshot.yaml
# watch: Pending -> Capturing -> Uploading -> Ready
kubectl get swiftsnapshot snapshot-s3-mem -w

# cross-node clone restore (edit spec.targetNode to a node in your cluster):
kubectl apply -f config/samples/s3-snapshots/04-restore-clone.yaml
# watch: Pending -> Downloading -> Restoring -> Resuming -> Ready
kubectl get swiftrestore snapshot-s3-clone -w
```

## Cross-node restore and `spec.targetNode`

`SwiftRestore.spec.targetNode` pins the node the restore lands on. It is only
consulted for the s3 backend (Tier A/B ignore it):

- **Clone / cross-node restore** (target name ≠ source): `targetNode` is
  **required** — the target guest doesn't exist yet, so there's no node to
  infer. The download Job and the restore-receive launcher both run there.
- **In-place restore** (target name = source): `targetNode` is optional; when
  omitted it defaults to the source guest's current node.

Without a resolvable node the restore fails fast with *"s3 restore requires
spec.targetNode"*.

## Identity on clones

Same as Tier B: CH `--restore` resumes the captured guest **byte-for-byte** —
cloud-init does not re-run, so a clone inherits the source's machine-id, SSH
host keys, hostname, and guest-visible MAC. `spec.identity.regenerate`
**must** include `macAddresses` for clones (the hypervisor rewrites the clone
MAC to avoid an L2 collision); the other items regenerate on the clone's first
reboot via the seed profile's bootcmd. See
[identity-regeneration.md](identity-regeneration.md).

## Security

- **Credentials live in a Secret**, surfaced to the upload/download Jobs as the
  standard `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`
  env vars. They are never written to annotations, logs, or status. Grant the
  IAM principal least privilege: `s3:PutObject` / `s3:GetObject` /
  `s3:ListBucket` scoped to the bucket + prefix.
- **The upload Job** mounts the snapshot dir **read-only** and runs as the
  image's non-root uid.
- **The download Job** runs **as root** — it must write the kubelet-created,
  root-owned node-local cache hostPath (`DirectoryOrCreate`, mode 0755). It is
  otherwise maximally constrained: drop `ALL` capabilities, no privilege
  escalation, read-only root filesystem, and the hostPath mount exposes only
  the single snapshot directory.
- **Encryption** is the object store's responsibility (SSE on the bucket).
  KubeSwift does not encrypt artifacts client-side in Phase 3.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| SwiftSnapshot stuck `Pending`, event *"snapshot-s3 image not configured"* | `KUBESWIFT_SNAPSHOT_S3_IMAGE` unset on the controller. Redeploy via Helm / `make deploy`. |
| Upload/download Job pod `ImagePullBackOff` / `401` | The `snapshot-s3` ghcr package is private, or your registry needs an imagePullSecret. Publicize the package (one-time) or add a pull secret. |
| Upload Job `Error`, logs show `NoSuchBucket` | The bucket doesn't exist — create it first (the controller doesn't). |
| Restore `Failed`, *"s3 restore requires spec.targetNode"* | Clone restore without `spec.targetNode`. Set it to the target node. |
| Download Job `Error`, *"checksum mismatch"* / *"size mismatch"* | Corrupted or partial object. The manifest verification is doing its job — re-run the snapshot. |
| Restore `Failed`, *"has no status.s3"* | The source SwiftSnapshot never finished uploading (not `Ready`). |
| MinIO: upload fails with a host/addressing error | Set `forcePathStyle: true` (MinIO / Ceph RGW require path-style addressing). |

## Cluster walkthrough

<!-- Populated after the cluster MinIO round-trip validation (PR 5). -->
_Empirical round-trip validation (source on one node → s3 snapshot → clone
restore on another node, sentinel survives) is recorded here after the
cluster run._
