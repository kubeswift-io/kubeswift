# Golden VM images in an OCI registry

KubeSwift can store and distribute **golden VM disk images** as OCI-registry
artifacts, so a customized base disk (packages baked in, config applied) becomes
a versioned, content-addressed, deduplicated artifact any cluster can pull. A
`SwiftImage` consumes one via `spec.source.oci`; the `swiftctl image publish`
command produces one.

This is the **golden-image** use of the registry — the immutable, reusable base
disk. It is distinct from **snapshots** (`SwiftSnapshot backend.type: oci`, a
VM's captured memory + disk state); see [OCI snapshots](../snapshots/s3-snapshots.md)
and the [cold-migration runbook](../snapshots/cold-migration.md). Design and
rationale: [`docs/design/oras-golden-image.md`](../design/oras-golden-image.md).

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
      # credentialsSecretRef:       # kubernetes.io/dockerconfigjson, same namespace
      #   name: regcreds
```

The SwiftImage controller runs a node-local import Job that pulls the chunked
artifact by digest, reassembles the sparse `image.raw` into the import PVC, and
runs the same resize + `sgdisk` + GRUB/serial patch tail as an HTTP import. When
the SwiftImage reaches `Ready`, a `SwiftGuest` boots from it via `imageRef` like
any other image.

`oci` is mutually exclusive with the other sources (`http` / `upload` / `pvcClone`).

## Signing

Sign at publish time so consumers can prove provenance:

```bash
export COSIGN_PASSWORD=...   # the key's password (empty for an unencrypted key)
swiftctl image publish golden.raw \
  --to ghcr.io/acme/vm-images --tag base --sign-key cosign.key
```

This attaches a cosign signature (default tag-based `sha256-<digest>.sig`) to the
pushed manifest. Verify it manually today:

```bash
cosign verify --key cosign.pub ghcr.io/acme/vm-images@sha256:1a2b...
```

> **TLS is required for verification.** `cosign verify` speaks HTTPS only — it
> does **not** honor `--insecure`/`--allow-http-registry` on the registry ping.
> A signature *pushed* over a plaintext (`--insecure`) registry lands, but cannot
> be verified until the registry is fronted with TLS. Use a TLS registry on the
> production path.

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

## See also

- [`docs/design/oras-golden-image.md`](../design/oras-golden-image.md) — design, chunking, dedup analysis
- [Edge Zot profile](edge-zot.md) — mirroring golden images to edge/air-gapped sites
- [OCI snapshots](../snapshots/s3-snapshots.md) and [cold migration](../snapshots/cold-migration.md) — the *stateful* registry use
