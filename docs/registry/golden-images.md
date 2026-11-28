# Golden VM images in an OCI registry

KubeSwift can store and distribute **golden VM disk images** as OCI-registry
artifacts, so a customized base disk (packages baked in, config applied) becomes
a versioned, content-addressed, deduplicated artifact any cluster can pull. A
`SwiftImage` consumes one via `spec.source.oci`; the `swiftctl image publish`
command produces one.

This is the **golden-image** use of the registry — the immutable, reusable base
disk. It is distinct from **snapshots** (`SwiftSnapshot backend.type: oci`, a
VM's captured memory + disk state); see the
[OCI snapshot samples](../../config/samples/oci-snapshots/README.md) and the
[cold-migration runbook](../snapshots/cold-migration.md).

## How it is stored (sparse, chunked, deduplicated)

The disk is streamed in fixed-size windows (default 64 MiB). All-zero windows
are **never stored** (a raw disk is typically ~90% sparse), and each non-zero
window is a content-addressed OCI layer annotated with its byte offset. Two
consequences:

- **Sparse upload** — only the real data is pushed. A 40 GiB disk with 6 GiB of
  content transfers ~6 GiB.
- **Cross-version dedup** — re-publishing a lightly-changed `v2` shares every
  unchanged block with `v1` by digest, so only the changed windows upload. An
  in-place apt-upgrade delta commonly transfers a few hundred MiB, not the whole
  disk.

The registry is a declared external dependency (Harbor / Zot / GHCR / ECR / a
cloud registry). KubeSwift is a registry **client**, never a registry; for
edge/air-gapped sites, run [Zot as an edge profile](edge-zot.md).

## Publish a golden image — `swiftctl image publish`

`image publish` runs entirely **client-side** (no cluster needed), so it fits a
packer / virt-install / CI pipeline that builds the disk.

```bash
# From a qcow2 (converted to raw automatically via qemu-img):
swiftctl image publish ubuntu-noble.qcow2 \
  --to ghcr.io/acme/vm-images --tag noble-24.04

# From a raw disk, to an in-cluster test registry over plaintext HTTP:
swiftctl image publish golden.raw \
  --to zot.registry.svc:5000/vm-images --tag base --insecure
```

Output reports the manifest digest and the transfer/dedup accounting:

```
Pushed ghcr.io/acme/vm-images:noble-24.04
  digest:      sha256:1a2b...
  disk size:   40.00 GiB
  transferred: 6.12 GiB (84.7% skipped — sparse + deduped)
```

Re-publishing a lightly-changed `v2` additionally reports a `deduped:` line for
the bytes already present in the registry from `v1`.

**Pin the digest for reproducible consumers** — record the printed `digest:` and
reference it from `SwiftImage.spec.source.oci.digest` (a tag is mutable).

### Input format

`publish` accepts a **raw** or **qcow2** disk. A qcow2 is detected by its header
and converted to raw with `qemu-img convert -O raw` before chunking (the artifact
always stores raw, which is what the import pipeline expects) — `qemu-img` must be
on `PATH`, and the conversion needs temporary disk space for the full raw size.
Any other format: convert to raw yourself first.

### Credentials

Registry credentials come from your Docker config — run `docker login <registry>`
before publishing. An anonymous push is used if no credentials are present
(fine for an open in-cluster registry).

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--to` | (required) | Target repository **without** a tag, e.g. `ghcr.io/acme/vm-images` |
| `--tag` | `latest` | Artifact tag |
| `--chunk-size-mib` | `64` | Window size. Smaller → finer cross-version dedup, more layers |
| `--os-type` | `linux` | `linux` or `windows` (recorded in the artifact config) |
| `--insecure` | `false` | Allow a plaintext (http) registry — **UNSAFE**, in-cluster/test only |
| `--sign-key` | — | Path to a cosign private key; cosign-sign the pushed artifact |
| `--keep-converted` | `false` | Keep the temp raw produced from a qcow2 input |

## Publish from the cluster — `SwiftImagePublish`

A `SwiftImagePublish` publishes a disk that is already in the cluster, in the
same format `swiftctl image publish` writes. The source is one of:

- **`snapshotRef`**: a Ready SwiftSnapshot with a root disk.
  - `csi-volume-snapshot`: the root disk is restored into a temporary PVC,
    pushed, and the PVC deleted. The guest can keep running; the image is
    crash-consistent, as the snapshot is.
  - `oci` with `includeDisk` (full-state): the snapshot's disk artifact is
    already a golden image, so it is copied registry to registry under the
    destination with new annotations (`snapshot-oras --mode=copy-image`). No
    volume is read. Only the chunks the destination lacks are copied; on the
    same registry they are mounted from the snapshot's repository, so no data
    passes through the Job (a registry that refuses the mount gets them
    streamed). The source is read with the snapshot's
    recorded connection (CA, credentials Secret, verify key: when it has a
    verify key the source is verified before it is copied). The copy does not
    depend on the snapshot afterwards: deleting the snapshot deletes its
    artifact, not the published image. A memory-only snapshot is refused.
- **`guestRef`**: a stopped SwiftGuest's root disk, read as it is. Stop the
  guest first (`runPolicy: Stopped`); the request waits with
  `GuestNotStopped` until `status.phase` is `Stopped`. It then holds the guest
  (annotation `image.kubeswift.io/publish-hold`): no launcher is created for
  it, whatever its runPolicy, until the disk has been read and the request is
  Ready or Failed, or deleted and its Job gone. The guest controller records
  `LauncherHeldByImagePublish` events meanwhile. Nothing stops the guest for
  you and no snapshot is taken. Kernel-boot and shared-base guests are
  refused. A second request for the same guest waits (`GuestHeld`).

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImagePublish
metadata:
  name: web-2026-10
  namespace: team-a
spec:
  source:
    snapshotRef: {name: web-snap}
  destination:
    image: web              # -> <location repository>/team-a/images/web
    tag: "2026.10"
  createImage:              # optional: a SwiftImage pinned to the pushed digest
    name: web-2026-10
```

- **Destination.** By default the image goes to a storage location
  (`destination.locationRef`, else the namespace's default SwiftStorageLocation,
  else the cluster's default SwiftClusterStorageLocation): a cluster
  location's repository plus `<namespace>/images/<image>`, a namespace
  location's plus `images/<image>`. The location gives the credentials, CA
  bundle and signing key; Secrets are only read in the request's namespace.
  Or set `destination.repository` (the full repository) with its own
  `insecure`, `caBundle`, `credentialsSecretRef`, `signingKeySecretRef` and
  `verifyKeySecretRef`. `status.destination` records what was used, resolved
  once.
- **Signing.** When the destination has a signing key the image is signed,
  and the request fails if it is not. The manifest always carries provenance
  annotations, signed into the signature too: `io.kubeswift.source.kind`,
  `.name`, `.uid`, `.volumesnapshot`, `.digest` (a copy), `io.kubeswift.publish.name`,
  `.namespace`, `.uid` and `io.kubeswift.version`. A
  SwiftArtifactVerificationPolicy can require them as claims.
- **Tags do not move.** A tag that already names a different image fails the
  request with `TagInUse` and stays as it was. Publishing the same image again
  is fine. `destination.overwriteTag: true` moves the tag.
- **`createImage`** creates a SwiftImage that pulls the pushed digest (not the
  tag) with the destination's connection settings and verify key, with
  `rootDisk.size` at least the disk's size, and `osType` from the source. It
  has no owner. A SwiftImage of that name that this request did not create
  fails it with `ImageExists`; the image is still in the registry
  (`status.reference`, `status.digest`).
- **The request is one-shot.** Its spec cannot change; create a new one to
  publish again. Deleting it never deletes the published image.
- **What it records.** `osType` comes from `spec.osType`, else the source
  guest's SwiftImage, else `linux`. The temporary PVC uses
  `spec.storageClassName`, else the source guest's root disk class; when that
  guest is gone and no class is set, the request waits with
  `StorageClassRequired`. A snapshot of a kernel-boot guest is refused. A
  guest source is read in place, with no temporary PVC; its image's size is
  the whole root disk.
- **The Job** runs `snapshot-oras --mode=upload-image` without node access:
  non-root, no capabilities, read-only root filesystem, no service account
  token, the disk mounted read-only, a 6 h deadline. At most two publish Jobs
  run at once in the cluster (controller flag
  `--max-concurrent-image-publishes`); the rest wait with `Queued`.

`kubectl get sip` lists requests with their phase and reference. The Ready
condition's reason says what a Pending one waits for (`SourceNotReady`,
`RegistryCredentialsMissing`, `Queued`, ...).

Before publishing a guest's disk as a template, clean it inside the guest
before the snapshot: `cloud-init clean --logs --seed`, empty
`/etc/machine-id`, remove `/etc/ssh/ssh_host_*`, clear shell history (Sysprep on
Windows). KubeSwift does not change the disk.

## Consume a golden image — `SwiftImage.spec.source.oci`

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: golden-noble
spec:
  source:
    oci:
      repository: ghcr.io/acme/vm-images
      # Prefer digest (immutable) over tag for reproducibility:
      digest: sha256:1a2b...        # OR: tag: noble-24.04
      # insecure: true              # plaintext in-cluster registry only
      # caBundle: |                 # PEM, for a registry behind a private CA
      #   -----BEGIN CERTIFICATE-----
      # credentialsSecretRef:       # kubernetes.io/dockerconfigjson, same namespace
      #   name: regcreds
```

The SwiftImage controller runs a node-local import Job that pulls the chunked
artifact by digest, reassembles the sparse `image.raw` into the import PVC, and
runs the same resize + `sgdisk` + GRUB/serial patch tail as an HTTP import. When
the SwiftImage reaches `Ready`, a `SwiftGuest` boots from it via `imageRef` like
any other image.

`oci` and `http` are mutually exclusive.

## Signing

Sign at publish time so consumers can prove provenance:

```bash
export COSIGN_PASSWORD=...   # the key's password (empty for an unencrypted key)
swiftctl image publish golden.raw \
  --to ghcr.io/acme/vm-images --tag base --sign-key cosign.key
```

This attaches a cosign signature to the pushed manifest. Verify it manually:

```bash
cosign verify --key cosign.pub --insecure-ignore-tlog=true \
  ghcr.io/acme/vm-images@sha256:1a2b...
```

> **`--insecure-ignore-tlog=true` is required, and is not a weakening here.**
> KubeSwift signs **offline** — no entry is written to a transparency log,
> deliberately, so publishing a private VM image does not disclose its digest to
> a public log. `cosign verify` demands a log entry by default and exits non-zero
> without this flag, which looks like an invalid signature but is not one. The
> key check — that the signature was made by the key you pass — is unaffected.

> **TLS is required for verification.** `cosign verify` speaks HTTPS only — it
> does **not** honor `--insecure`/`--allow-http-registry` on the registry ping.
> A signature *pushed* over a plaintext (`--insecure`) registry lands, but cannot
> be verified until the registry is fronted with TLS. Use a TLS registry on the
> production path.

> **Signature format.** cosign 2.x writes a `sha256-<digest>.sig` tag; 3.x writes
> a `sha256-<digest>` index holding a Sigstore bundle. Both cosign majors read
> both forms, so which one you have does not affect the command above.

### Verify-on-pull

Point a SwiftImage at a **cosign public key** and the import verifies the
signature **before** trusting any bytes — a golden disk whose signature is
missing or does not verify **fails the import** (no unsigned/tampered image is
ever materialized):

```bash
kubectl create secret generic golden-verify-key --from-file=cosign.pub
```

```yaml
spec:
  source:
    oci:
      repository: ghcr.io/acme/vm-images
      digest: sha256:1a2b...            # pin the digest you signed
      verifyKeySecretRef:
        name: golden-verify-key         # Secret with key "cosign.pub"
```

The import resolves the reference to a digest, `cosign verify`s **that digest**,
then pulls by the **same** digest (so a tag can't be swapped between verify and
pull). Because `cosign verify` is HTTPS-only, `verifyKeySecretRef` together with
`insecure: true` is **rejected at admission** — verify-on-pull requires a TLS
registry.

### Connection settings from a storage location

A source that sets none of `insecure`, `caBundle`, `credentialsSecretRef` and
`verifyKeySecretRef` takes them from a storage location: `source.oci.locationRef`,
else the namespace's default SwiftStorageLocation, else the cluster's default
SwiftClusterStorageLocation. The location gives the credentials Secret name
(looked up in the image's namespace), `insecure`, its CA bundle and its
`verifyKey`; the repository stays the image's own. It applies only when the
location's registry host is the repository's host:

- a default for another registry applies nothing, and the pull is anonymous as
  before;
- a `locationRef` for another registry makes the image wait
  (`StorageLocationInvalid`);
- a missing credentials Secret makes it wait (`RegistryCredentialsMissing`).

```yaml
spec:
  source:
    oci:
      repository: registry.example.com/golden/noble   # same host as the location
      digest: sha256:1a2b...
      # locationRef: {kind: SwiftClusterStorageLocation, name: main}   # else the defaults
```

Setting any connection field uses the source's fields as written, all or
nothing; `locationRef` together with one is refused at admission. Either way
`status.source` records what the import used (`location`: `Explicit`, the
location's `<Kind>/<name>`, or `None`), resolved once before the import starts.

## See also

- [Edge Zot profile](edge-zot.md) — mirroring golden images to edge/air-gapped sites
- [OCI snapshot samples](../../config/samples/oci-snapshots/README.md) and [cold migration](../snapshots/cold-migration.md) — the *stateful* registry use
