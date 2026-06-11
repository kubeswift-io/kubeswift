# Live-migratable SwiftGuests

SwiftGuests in this directory are configured to **live-migrate** cleanly
between nodes (sub-3s downtime; the guest keeps running during pre-copy).
They are also fine for offline migration.

## What makes a guest live-migratable

Live migration (`mode: live`) requires **all** of:

1. **No VFIO devices** — no `gpuProfileRef`, no SR-IOV `interfaces`. VFIO
   state cannot transfer; such guests are **offline-only** (the
   webhook/controller resolve them to offline).
2. **No node-local virtio backends** — no `filesystems` (virtiofs) and no
   `type: vhost-user` interfaces/`vhostUserDevices`. Those backends live on
   the source node and don't follow the guest; such guests are
   **offline-only**.
3. **Disk-boot guests need RWX + Block storage** — a `imageRef` guest holds a
   root-disk PVC, and only `ReadWriteMany` + `volumeMode: Block` on a
   live-migration-capable StorageClass (Longhorn `migratable: "true"`) can be
   attached on both nodes during cutover. `ReadWriteOnce` + `Filesystem` (the
   default) is **offline-only**. Get RWX+Block either from a class
   (`live-migratable`, see [`../storage/`](../storage/)) or a per-guest
   `spec.storage` override.
   - **Kernel-boot guests** (`kernelRef`) have **no root PVC**, so they are
     live-migratable on default storage — the lightest case (and what the
     project validated live migration on).

`spec.migration.preferredMode: live` makes the intent explicit; with `auto`
the controller live-migrates when eligible and falls back to offline
otherwise. `enabled: true` (the default) lets migrations target the guest at
all; set it `false` to pin a guest in place.

## IP across the move

Default node-local-bridge networking gives the guest a **fresh IP** on the
destination — the migration then needs `spec.allowIPChange: true` on the
SwiftMigration. To **preserve the IP**, put the primary interface on a
multi-node L2 NAD (`primary: true` + `networkRef`) — see
[`03-ip-preserving-live-migratable.yaml`](03-ip-preserving-live-migratable.yaml)
and [`../multi-node-l2/`](../multi-node-l2/).

## The manifests

| File | Boot | Storage | Notes |
|---|---|---|---|
| [`01-kernel-boot-live-migratable.yaml`](01-kernel-boot-live-migratable.yaml) | kernel | none (no PVC) | lightest live-migratable guest |
| [`02-disk-boot-live-migratable.yaml`](02-disk-boot-live-migratable.yaml) | disk (Ubuntu Noble) | RWX+Block via `live-migratable` class | realistic disk VM; IP changes cross-node |
| [`03-ip-preserving-live-migratable.yaml`](03-ip-preserving-live-migratable.yaml) | disk | RWX+Block (per-guest override) | primary on a multi-node NAD → IP preserved |

## Prerequisites

Apply the shared bundle once (it creates the `default` SwiftGuestClass and the
`minimal` SwiftSeedProfile these manifests reference — a missing seed profile
is the most common "guest stuck Failed: ResolutionFailed" cause):

```bash
kubectl apply -f ../shared/                       # default class + minimal seed
```

Plus, per manifest:

- **01** — a Ready `SwiftKernel` named `faas-minimal` and a node labeled
  `kubeswift.io/kernel-node=true`.
- **02 / 03** — a Ready `SwiftImage` named `ubuntu-noble`, the
  `live-migratable` SwiftGuestClass and the `longhorn-migratable`
  StorageClass (both in [`../storage/swiftguestclass-rwx-migratable.yaml`](../storage/swiftguestclass-rwx-migratable.yaml);
  the StorageClass is applied once by a cluster admin).
- **03** — a multi-node L2 NAD named `ovn-l2`
  ([`../multi-node-l2/ovn-l2-nad.yaml`](../multi-node-l2/ovn-l2-nad.yaml)).

## Migrating one

Pre-flight, then move (replace the node name with another worker):

```bash
swiftctl migrate live-disk-vm --to <node> --check     # dry-run: eligibility + mode
swiftctl migrate live-disk-vm --to <node>             # creates the SwiftMigration
```

Or apply a SwiftMigration directly — see
[`../migration/swiftmigration-basic.yaml`](../migration/swiftmigration-basic.yaml).
For a default-networking guest (01, 02) add `--allow-ip-change` (the IP
changes on the destination); 03's primary-on-NAD preserves the IP.
