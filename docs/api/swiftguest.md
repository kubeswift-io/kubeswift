# SwiftGuest

A SwiftGuest is **one VM instance**. You create it by referencing a SwiftGuestClass (CPU/memory template), SwiftImage (root disk), and optionally a SwiftSeedProfile (cloud-init).

**API:** `swift.kubeswift.io/v1alpha1` · **Short name:** `sg`

## Operator workflow

1. Ensure SwiftImage exists and is `phase=Ready` (import can take 5–15 min).
2. Apply `config/rbac/` in the namespace (required for swiftletd status patching).
3. Create SwiftGuest; controller creates pod; swiftletd launches Cloud Hypervisor.

## Spec

| Field | Required | Description |
|-------|----------|-------------|
| `imageRef.name` | Yes | SwiftImage name (same namespace) |
| `guestClassRef.name` | Yes | SwiftGuestClass name (cluster-scoped) |
| `seedProfileRef.name` | No | SwiftSeedProfile for cloud-init (same namespace) |
| `runPolicy` | No | `Running` (default) or `Stopped` |

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: my-guest
  namespace: default
spec:
  imageRef:
    name: ubuntu-cloud
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  runPolicy: Running
```

## Status

| Field | Description |
|-------|-------------|
| `phase` | Pending, Scheduling, Running, Stopped, Failed |
| `conditions` | Resolved, ImageReady, PodScheduled, GuestRunning |
| `nodeName` | Node where the guest pod runs |
| `podRef` | Reference to the guest pod |

**Phase meanings:** `Pending` = resolution failed or unschedulable; `Scheduling` = pod pending; `Running` = VM up; `Stopped` = VM stopped; `Failed` = resolution, pod, or VM error.

## Example

```bash
kubectl apply -f config/samples/swiftguest-sample.yaml
kubectl get swiftguest sample -w
```

## Prerequisites

- SwiftImage `phase=Ready` before creating SwiftGuest
- `kubectl apply -k config/rbac -n <namespace>`
- Worker nodes with KVM; run [preflight](../operator/worker-node-preflight.md)

[SwiftGuestClass](swiftguestclass.md) · [SwiftImage](swiftimage.md) · [SwiftSeedProfile](swiftseedprofile.md) · [Lifecycle](../architecture/lifecycle.md)
