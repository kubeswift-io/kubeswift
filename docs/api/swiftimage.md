# SwiftImage

SwiftImage defines a **disk image source**. The controller downloads or clones it into a PVC; SwiftGuest uses that PVC as the root disk. Create SwiftImage before SwiftGuest—SwiftGuest will not run until SwiftImage is `phase=Ready`.

**API:** `image.kubeswift.io/v1alpha1` · **Short name:** `si`

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `format` | Yes | `raw` or `qcow2` — **must match the actual image format** |
| `source.http` | Yes* | HTTP(S) URL to fetch image |
| `source.oci` | Yes* | Pull a golden image from an OCI registry ([golden images](../registry/golden-images.md)) |
| `source.pvcClone` | Yes* | Copy a disk from a PVC in the image's namespace |

*Exactly one source type: `http`, `oci` or `pvcClone`.

## Source: HTTP

```yaml
spec:
  format: qcow2
  source:
    http:
      url: https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
```

- Controller creates an Import Job; download can take **5–15 minutes**.
- URL must be reachable from the cluster.
- **Format:** Ubuntu `.img` files are qcow2—use `format: qcow2`. Raw images use `format: raw`.

## Source: PVC clone

Makes an image from a disk that is already in the cluster: a disk you prepared by hand, or a stopped guest's root disk. No web server or registry round trip.

```yaml
spec:
  format: raw
  source:
    pvcClone:
      name: swiftguest-root-golden   # a PVC in this namespace
```

- **Where the disk is in the PVC.** A Block PVC is the disk. A Filesystem PVC holds it as `image.raw` at its root, which is how every KubeSwift disk PVC stores it.
- **Format.** `format` is the disk's format. A guest's root disk is `raw`. A `qcow2` source is checked for backing and external data files, like an http one.
- **Copied, not referenced.** The disk is copied into the image's own import PVC, read-only from the source. The image does not depend on the source once it is `Ready`, and deleting the source does not affect it. The import PVC is never smaller than the source.
- **Only the image's namespace.** `pvcClone.namespace` may be empty or the image's own namespace. A SwiftImage cannot read another namespace's disk; the webhook refuses it, and so does the controller.
- **The source must be idle.** An image of a disk that is still being written is not consistent. While a pod that has not finished uses the PVC — a running guest, or one starting — the image stays `Pending`, and the `SourceReady` condition names the pod. Stop the guest (`runPolicy: Stopped`) and the import starts. It also waits while the PVC is missing or not `Bound`.
- **Keep the source idle until `Ready`.** The import does not lock the PVC. A guest started on it while the copy runs makes an inconsistent image.
- **Guest preparation is yours.** Before turning a guest's disk into an image others boot from, clean what must be unique per guest: cloud-init state (`cloud-init clean`), `/etc/machine-id`, SSH host keys.

The rest of the import is the same as for the other sources: the GRUB serial-console patch for `osType: linux`, `cloneStrategy`, `importStorageClassName`.

## Status

| Field | Description |
|-------|-------------|
| `phase` | Pending → Importing → Validating → Preparing → Ready (or Failed) |
| `conditions[SourceReady]` | `pvcClone` only: `False` with `SourceNotFound`, `SourceNotBound` or `SourceInUse` while the import waits |
| `sourceFormat` | Original input format (e.g. qcow2) |
| `preparedFormat` | Runtime format after preparation (always raw) |
| `preparedArtifact.pvcRef` | PVC containing image.raw |
| `preparedArtifact.format` | Prepared disk format (raw) |
| `preparedArtifact.size` | Actual measured size of image.raw |

## Operator workflow

1. Create SwiftImage.
2. Wait for `phase=Ready`: `kubectl get swiftimage ubuntu-noble -w`
3. Create SwiftGuest referencing it.

**If import fails:** Check `format` matches image (Ubuntu = qcow2). See [troubleshooting](../operator/troubleshooting.md).

## Kernel images (SwiftKernel)

SwiftImage handles disk images for the disk boot path. For the kernel boot path, use [SwiftKernel](swiftkernel.md) instead.

SwiftKernel manages OCI artifacts containing a Linux kernel (bzImage) and initramfs (rootfs.cpio.gz). These are not disk images — they are raw binary files pulled to each labeled node via ORAS. SwiftKernel and SwiftImage serve different boot paths and are referenced via different fields on SwiftGuest (`kernelRef` vs `imageRef`).

See [SwiftKernel API reference](swiftkernel.md) for the full CRD spec, and the [SwiftKernel guide](../swiftkernel.md) for building and packaging kernel profiles.

[SwiftGuest](swiftguest.md) · [SwiftGuestClass](swiftguestclass.md) · [SwiftKernel](swiftkernel.md)
