# Creating live-migratable SwiftGuests

This page is the practical "how do I make a VM that live-migrates" guide.
For the migration *operation* and its phases, see [overview.md](overview.md).

## What makes a guest live-migratable

A guest is eligible for `mode: live` only when **all** of these hold:

1. **No VFIO devices** — no `gpuProfileRef`, no SR-IOV `interfaces`. VFIO state
   cannot transfer; such guests are **offline-only**.
2. **No node-local virtio backends** — no `filesystems` (virtiofs) and no
   `type: vhost-user` interfaces / `vhostUserDevices`. Those backends live on
   the source node and don't follow the guest; **offline-only**.
3. **Disk-boot guests need RWX + Block storage.** An `imageRef` guest holds a
   root-disk PVC, and only `ReadWriteMany` + `volumeMode: Block` on a
   live-migration-capable StorageClass (Longhorn `migratable: "true"`) can be
   attached on both nodes during cutover. The default `ReadWriteOnce` +
   `Filesystem` is **offline-only**. Get RWX+Block from a class
   (`live-migratable`) or a per-guest `spec.storage` override.
   - **Kernel-boot guests** (`kernelRef`) have **no root PVC**, so they are
     live-migratable on default storage — the lightest case.

`spec.migration.preferredMode: live` states the intent (with `auto` the
controller live-migrates when eligible, else falls back to offline).
`spec.migration.enabled: false` pins a guest in place.

## A ready-to-use guest set

[`config/samples/migratable-guests/lab-live-migration-set.yaml`](../../config/samples/migratable-guests/lab-live-migration-set.yaml)
defines three live-migratable guests of different boot types / OSes:

| Guest | Type / OS | Storage |
|---|---|---|
| `lm-kernel` | kernel-boot microVM (faas-minimal) | none (no PVC) — lightest |
| `lm-ubuntu` | Ubuntu Noble 24.04 disk VM | RWX+Block (`live-migratable` class) |
| `lm-rocky` | Rocky Linux 9 disk VM | RWX+Block (`live-migratable` class) |

```bash
# Prerequisites (once): default class + minimal seed, the live-migratable class
# + the longhorn-migratable StorageClass, and the ubuntu-noble / rocky9-cloud images.
kubectl apply -f config/samples/shared/
kubectl apply -f config/samples/storage/swiftguestclass-rwx-migratable.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml \
               -f config/samples/rocky/swiftimage-rocky9.yaml
# wait for both SwiftImages to reach Ready, THEN:
kubectl apply -f config/samples/migratable-guests/lab-live-migration-set.yaml
```

> **Apply images before guests.** A disk-boot guest created while its image is
> still `Importing` fails resolution (`SwiftImage not Ready`) and a Failed guest
> does not self-recover — recreate it once the image is `Ready`.

More focused single-guest examples (incl. an IP-preserving variant) are in the
[`migratable-guests/`](../../config/samples/migratable-guests/) directory README.

## Migrating one

```bash
swiftctl migrate lm-ubuntu --to <worker> --check          # pre-flight: capacity, mode, IP, CPU
swiftctl migrate lm-ubuntu --to <worker> --allow-ip-change
```

`--allow-ip-change` is required on default node-local networking (the guest gets
a fresh IP on the destination). To preserve the IP, put the primary interface on
a multi-node L2 NAD (`primary: true` + `networkRef`) — see
[`config/samples/migratable-guests/03-ip-preserving-live-migratable.yaml`](../../config/samples/migratable-guests/03-ip-preserving-live-migratable.yaml)
and [networking-requirements.md](networking-requirements.md). That path needs
OVN-Kubernetes; it does not work on a plain Calico cluster.

## Operational notes (learned on the dev cluster)

These bite in practice — check them before blaming the migration logic:

- **Target-node capacity.** The Validating phase refuses a target without CPU
  headroom (`insufficient CPU headroom: need 2, have ...`). Always `--check`
  first and target a node with room.
- **mTLS identity is worker-node-only.** With `migration.mtls.enabled` (Phase
  3c), each **worker** node gets a per-node identity Secret
  (`kubeswift-migration-node-<node>`). A guest running on the **control-plane**
  node has no such cert, so it **cannot be a live-migration source**
  (`MigrationIdentityNotReady`). Keep the control plane **tainted** so guests
  don't land there; move any that did off with an **offline** migration.
- **Offline always works** — it uses no mTLS tunnel (stop → detach → recreate).
  Use `--preferred-mode offline` for VFIO/virtiofs guests, or any time the live
  path is unavailable.
- **Cluster load matters.** The mTLS live-migration handshake has tight
  sidecar-readiness sequencing; on a CPU-saturated cluster (slow container/
  stunnel startup) it can race and fail. Keep migration headroom.
