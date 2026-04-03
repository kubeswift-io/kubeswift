# SwiftGuestClass

SwiftGuestClass is a **cluster-scoped template** for CPU, memory, and root disk. SwiftGuest references it via `guestClassRef.name`. Create one per size tier (e.g. `small`, `default`, `large`).

**API:** `swift.kubeswift.io/v1alpha1` · **Short name:** `sgc`

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `cpu` | Yes | CPU request (e.g. `"2"`, `"1000m"`) |
| `memory` | Yes | Memory request (e.g. `"2Gi"`, `"512Mi"`) |
| `rootDisk.size` | Yes | Root disk size (e.g. `"10Gi"`); must fit imported image |
| `rootDisk.format` | Yes | `raw` or `qcow2` — must match SwiftImage format |

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: default
spec:
  cpu: "2"
  memory: "2Gi"
  rootDisk:
    size: "10Gi"
    format: raw
```

## Operator notes

- **Cluster-scoped** — One SwiftGuestClass can be used by SwiftGuests in any namespace.
- **Format match** — `rootDisk.format` must match the SwiftImage (or prepared artifact) format.
- **Size** — Root disk size must be ≥ imported image size; default sample uses 10Gi.

## Example

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
```

[SwiftGuest](swiftguest.md) · [SwiftImage](swiftimage.md)
