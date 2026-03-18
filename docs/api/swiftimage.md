# SwiftImage

SwiftImage defines a **disk image source**. The controller downloads or clones it into a PVC; SwiftGuest uses that PVC as the root disk. Create SwiftImage before SwiftGuest—SwiftGuest will not run until SwiftImage is `phase=Ready`.

**API:** `image.kubeswift.io/v1alpha1` · **Short name:** `si`

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `format` | Yes | `raw` or `qcow2` — **must match the actual image format** |
| `source.http` | Yes* | HTTP(S) URL to fetch image |
| `source.pvcClone` | Yes* | Clone from existing PVC |

*Exactly one source type.

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

```yaml
spec:
  format: raw
  source:
    pvcClone:
      name: my-pvc
      namespace: default
```

See `config/samples/swiftimage-pvc-clone.yaml`.

## Status

| Field | Description |
|-------|-------------|
| `phase` | Pending → Importing → Validating → Preparing → Ready (or Failed) |
| `sourceFormat` | Original input format (e.g. qcow2) |
| `preparedFormat` | Runtime format after preparation (always raw) |
| `preparedArtifact.pvcRef` | PVC containing image.raw |
| `preparedArtifact.format` | Prepared disk format (raw) |
| `preparedArtifact.size` | Actual measured size of image.raw |

## Operator workflow

1. Create SwiftImage.
2. Wait for `phase=Ready`: `kubectl get swiftimage ubuntu-cloud -w`
3. Create SwiftGuest referencing it.

**If import fails:** Check `format` matches image (Ubuntu = qcow2). See [troubleshooting](../operator/troubleshooting.md).

[SwiftGuest](swiftguest.md) · [SwiftGuestClass](swiftguestclass.md)
