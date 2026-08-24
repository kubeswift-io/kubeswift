# Layer 2 — infrastructure resources

`infrastructure/kubeswift/` holds the cluster's shared VM building blocks:
classes, images, seed profiles, kernels, GPU profiles (and NADs if you use
multi-NIC / multi-node L2). It is shared across environments by default —
environment differences belong in the workloads layer or in per-cluster patches.

Working examples: [`examples/gitops-flux/infrastructure/kubeswift/`](../../examples/gitops-flux/infrastructure/kubeswift/).

Images and kernels can both come from an OCI registry rather than a vendor
URL, which is worth doing under GitOps for the digest-pinning alone. See
[oci-artifacts.md](oci-artifacts.md).

## SwiftImage lifecycle nuances under GitOps

- **Imports are slow and asynchronous** (download → qcow2-to-raw → resize).
  Keep `wait: false` on the infra Kustomization so workloads aren't blocked;
  guests referencing a not-yet-Ready image wait in `Pending`.
- **Pruning an image** deletes its prepared PVC (and with
  `cloneStrategy: snapshot`, its clone-seed). Ensure no guest references it;
  guests keep running but cannot be recreated from a pruned image.

### Immutability: three rules, and two of them bite early

A Ready SwiftImage's spec is immutable, webhook-enforced. Two fields are
stricter than that and freeze **as soon as the import leaves `Pending`**, which
is the part that surprises people in review: the manifest looked fine, and the
Kustomization went `Ready=False` anyway.

| Field | Frozen from | Why |
|---|---|---|
| whole `spec` | phase `Ready` | the prepared disk already exists |
| `spec.cloneStrategy` | import started | the clone-seed is built during import |
| `spec.importStorageClassName` | import started | the import PVC is already bound to a class |

To roll a new image version, add a NEW SwiftImage (e.g.
`ubuntu-noble-2026-08`) and point guests/pools at it. Do not edit the URL of a
Ready image in Git; the webhook rejects the update and the Kustomization
surfaces it. Metadata-only edits (labels/annotations) on Ready images are fine.
The same applies to the two early-freezing fields: moving an image to another
storage class means deleting and recreating the SwiftImage, not editing it.

### `importStorageClassName` and clone strategy

`spec.importStorageClassName` (v0.13.11) pins the storage class of the *import*
PVC. Without it the import lands on the cluster default, which is the wrong
class on any cluster whose default is not the one you want VM disks on. Set it
explicitly in Git rather than relying on a cluster-wide default that a different
team may change underneath you.

`cloneStrategy: snapshot` needs a snapshot-capable CSI driver and a
VolumeSnapshotClass, and it clones the *source volume*, so the clone only
produces a usable disk when the volume mode matches. A guest class that resolves
to Block backed by a Filesystem import cannot be cloned by snapshot; the
controller detects that mismatch and downgrades that guest to the copy path
rather than producing an unbootable disk. Nothing fails, so watch for it in the
controller logs if you expected snapshot-speed provisioning and got copy-speed.

## SwiftKernel: the one that will not re-pull

SwiftKernel is **namespaced**, and pulls its OCI artifact per node onto nodes
labelled `kubeswift.io/kernel-node=true`.

The GitOps trap: the pull is a Job named `swiftkernel-pull-<name>-<node>`, so
re-pull is keyed on **(name, node)** and not on the artifact reference. Changing
`spec.ociRef.image` to a new tag in Git will **not** re-pull on nodes that
already ran the Job — Flux applies the change, the object updates, and nothing
downloads. Either give the new kernel a new SwiftKernel name (the clean GitOps
move, and the same pattern as SwiftImage), or delete the Job on each affected
node to force it. The Jobs carry no distinguishing labels, so delete them by
name (they are owned by the SwiftKernel and live in its namespace):

```bash
kubectl -n <ns> get jobs | grep swiftkernel-pull
kubectl -n <ns> delete job swiftkernel-pull-<name>-<node>
```

Prefer the rename. Deleting Jobs is an imperative fix to a declarative problem
and it will need doing again on the next tag.

## Classes and GPU profiles

SwiftGuestClass and SwiftGPUProfile are small and safe to manage in Git —
changes only affect newly created/recreated guests. The example ships a
`default` class and a `gpu-pcie` class with `coreScheduling: vm` (the
multi-tenant SMT mitigation) plus a Tier-1 `pcie-single` GPU profile.

SwiftGuestClass is **cluster-scoped**, so it is shared across every namespace a
fleet repo manages. Two environments in one cluster cannot hold different
definitions of a class with the same name; give them distinct names instead.
