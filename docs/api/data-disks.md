# Data Disks

A SwiftGuest can attach secondary disks beyond its OS root disk. There are three
kinds, all declared in `spec.dataDiskRefs[]` (with a singular `spec.dataDiskRef`
shorthand for one image-backed disk). Data disks work with every boot path
(disk, kernel, GPU) and compose with GPU passthrough.

| Kind | Field | What it is | VM-visible disk? |
|---|---|---|---|
| **Blank** | `blank: {size, storageClassName, volumeMode}` | A new, empty, sized volume — no image. The controller provisions a guest-owned PVC; the guest formats it. | Yes |
| **Image-backed** | `imageRef: {name}` | A Ready SwiftImage attached as a second disk (pre-seeded content). | Yes |
| **Attached PVC** | `pvcRef: {name}` (+ `attachAsDisk: true`) | An existing PVC. With `attachAsDisk` and a **Block** PVC, attached as a raw VM disk; otherwise a filesystem-directory mount (pool storage, not a VM disk). | Only with `attachAsDisk` |

## Blank data disk (the common case)

```yaml
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: db-vm
spec:
  imageRef: {name: ubuntu-noble}
  guestClassRef: {name: default}
  seedProfileRef: {name: minimal}
  runPolicy: Running
  dataDiskRefs:
    - name: data            # DNS-label, unique, <=36 chars
      blank:
        size: 100Gi         # required, > 0
        # volumeMode: Block  (default) | Filesystem
        # storageClassName: ""  (empty = cluster default class)
```

What happens:

1. The controller creates a **guest-owned** PVC `db-vm-data-data` — `ReadWriteOnce`,
   `volumeMode` from spec (**Block** by default), `storage` = `size`,
   `storageClassName` from spec (empty = default class). Nothing is copied — there
   is no source image.
2. The pod attaches it to the launcher as a raw device at
   `/dev/kubeswift-data-<name>` (`volumeDevices`, not a mount).
3. swiftletd passes it to Cloud Hypervisor as `--disk path=/dev/kubeswift-data-<name>`;
   it appears in the guest as the next virtio-blk device (`/dev/vdc`, ...).
4. The guest partitions/formats it: `sudo mkfs.ext4 -L scratch /dev/vdc`.

The guest must not boot with a missing disk: until every blank/image PVC is
**Bound**, the guest holds in `Scheduling` with `DataDisksReady=False` naming the
blocker. `status.dataDisks[]` echoes each disk's `{name, pvcName, volumeMode,
devicePath, bound}`. The blank PVC is garbage-collected when the guest is deleted.

### `volumeMode: Block` vs `Filesystem`

- **Block** (default) is the recommended path: the raw device is handed to the
  guest, which runs `mkfs`. It is the live-migration-capable mode (RWX+Block is
  the migratable combination).
  Requires a Block-capable StorageClass (e.g. Longhorn).
- **Filesystem** is an escape hatch for Block-incapable clusters: the controller
  creates a Filesystem PVC and a one-shot Job truncates a blank `image.raw` of the
  requested size into it, which is then attached as a VM disk.

## Image-backed data disk

```yaml
  dataDiskRefs:
    - name: seeded
      imageRef: {name: my-data-image}   # a Ready SwiftImage
# or the singular shorthand (one image-backed disk named "data"):
  dataDiskRef: {name: my-data-image}
```

> ⚠️ **Never point an image-backed data disk at a bootable OS image** (especially
> the same one used for `imageRef`). A cloud OS image has fixed partition and
> filesystem UUIDs identical to the root disk's; the guest then mounts a mix of
> partitions from both disks (`/` from one, `/boot` from the other), corrupting
> the boot in a hard-to-debug way. Use a **blank** disk for empty volumes, or an
> image built from a genuine *data* volume (e.g. `qemu-img create -f raw data.raw 100G`
> pre-populated), not a cloud OS image.

## Attaching an existing PVC as a raw disk

```yaml
  dataDiskRefs:
    - name: vol1
      pvcRef: {name: my-block-pvc}   # must be a Block PVC
      attachAsDisk: true
```

`attachAsDisk` requires a **Block** PVC (a Filesystem PVC has no raw device to
attach and is rejected at resolution). A `pvcRef` **without** `attachAsDisk` is a
filesystem-directory mount into the launcher pod (the SwiftGuestPool per-replica
storage path), **not** a VM-visible disk.

## Device-letter ordering (important)

Data disks enumerate in the guest in **declaration order** — the singular
`dataDiskRef` first, then `dataDiskRefs[]` in order — as `/dev/vdc`, `/dev/vdd`, …
(after the root `/dev/vda` and, on disk boot, the cloud-init seed `/dev/vdb`).

**The in-guest `/dev/vdX` letter is not stable** across reboots or reordering.
The in-pod host device path `/dev/kubeswift-data-<name>` *is* stable, but the
guest does not see that name. **Always mount data disks by `UUID=` or `LABEL=`**
(e.g. `mkfs.ext4 -L scratch /dev/vdc` then `mount LABEL=scratch /mnt`), never by
`/dev/vdX`.

## Validation rules (admission webhook)

- Exactly one of `imageRef` / `pvcRef` / `blank` per entry.
- `blank.size` > 0; `blank.volumeMode` ∈ {Block, Filesystem}.
- `attachAsDisk` only with `pvcRef`.
- `name` is a unique DNS label per guest; `data` is reserved for the singular
  `dataDiskRef` shorthand.
- At most 8 entries in `dataDiskRefs`.

## Limits

- Live-migration of data disks is not yet covered; a guest-owned RWO blank PVC
  detaches/reattaches with the guest on **offline** migration (handled by the
  Preparing dual-poll), like the root PVC.
- Filesystem-mode blank disks ride the same runtime path as image-backed data
  disks; Block-mode is the cluster-validated path.
