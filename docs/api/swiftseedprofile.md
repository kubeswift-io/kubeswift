# SwiftSeedProfile

SwiftSeedProfile defines **cloud-init NoCloud** content. When SwiftGuest references it via `seedProfileRef`, the controller renders user-data, meta-data, and network-config into a ConfigMap and mounts it into the guest pod. swiftletd builds the NoCloud layout for Cloud Hypervisor.

**API:** `seed.kubeswift.io/v1alpha1` · **Short name:** `ssp`

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `datasource` | Yes | `NoCloud` (only supported) |
| `userData` | Yes* | Inline cloud-init user-data |
| `userDataFrom` | Yes* | Secret/ConfigMap ref for user-data |
| `metaData` | No | Inline meta-data |
| `metaDataFrom` | No | Secret/ConfigMap ref for meta-data |
| `networkData` | No | Inline network-config |
| `networkDataFrom` | No | Secret/ConfigMap ref for network-config |

*For user-data: use `userData` or `userDataFrom`, not both.

## Example (inline)

```yaml
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: minimal
  namespace: default
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: kubeswift-guest
    package_update: true
  metaData: |
    instance-id: kubeswift-001
    local-hostname: kubeswift-guest
```

## Example (Secret ref)

Use `userDataFrom` / `metaDataFrom` to reference a Secret or ConfigMap. See `config/samples/swiftseedprofile-with-secret.yaml`.

## Operator notes

- **Optional** — SwiftGuest can omit `seedProfileRef`; VM boots without cloud-init.
- **NoCloud only** — ConfigDrive and Ignition are not implemented.

[SwiftGuest](swiftguest.md) · [Seed rendering](../seed-rendering.md)
