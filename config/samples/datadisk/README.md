# Data Disks

Attach secondary disks to a SwiftGuest. KubeSwift supports three kinds, all via
`spec.dataDiskRefs[]` (plus the legacy singular `spec.dataDiskRef` shorthand for
one image-backed disk):

| Kind | Field | What it is |
|---|---|---|
| **Blank** | `blank: {size, volumeMode}` | A new, empty, sized volume — no image. Block by default; the guest formats it. The "blank 100Gi for my database" case. |
| **Image-backed** | `imageRef: {name}` | A SwiftImage attached as a second disk (pre-seeded content). |
| **Attached PVC** | `pvcRef: {name}` + `attachAsDisk: true` | An existing Block PVC attached as a raw VM disk. (Without `attachAsDisk`, a `pvcRef` is a filesystem-directory mount — pool storage, not a VM disk.) |

Disks enumerate in the guest in declaration order — the singular `dataDiskRef`
first, then `dataDiskRefs[]`. **Mount by UUID/LABEL, not `/dev/vdX`** — the
host device path (`/dev/kubeswift-data-<name>`) is stable, but the in-guest
letter is not.

## Blank data disk (the v0.4.2 feature)

```bash
kubectl apply -f config/samples/shared/swiftguestclass-default.yaml
kubectl apply -f config/samples/shared/swiftseedprofile-minimal.yaml
kubectl apply -f config/samples/disk-boot/swiftimage-ubuntu-noble.yaml   # wait Ready
kubectl apply -f config/samples/datadisk/swiftguest-blank-datadisk.yaml
kubectl get swiftguest blank-datadisk-test -w
```

Expected:
- The controller creates a guest-owned Block PVC `blank-datadisk-test-data-scratch`.
- `kubectl get swiftguest blank-datadisk-test -o jsonpath='{.status.dataDisks}'`
  echoes `{name, pvcName, volumeMode: Block, devicePath, bound: true}`.
- `DataDisksReady=True`; phase=Running.
- Inside the guest, `lsblk` shows `/dev/vdc` as a blank 50Gi raw disk; format it:
  `sudo mkfs.ext4 -L scratch /dev/vdc && sudo mount LABEL=scratch /mnt`.

The blank PVC is owned by the guest and is garbage-collected when the guest is
deleted.

## Image-backed data disk (singular shorthand)

```bash
# Replace the placeholder URL in swiftimage-datadisk.yaml with your data image.
kubectl apply -f config/samples/datadisk/swiftimage-datadisk.yaml             # wait Ready
kubectl apply -f config/samples/datadisk/swiftguest-datadisk.yaml
kubectl get swiftguest datadisk-test -w
```

Inside the guest, `lsblk` shows the image-backed disk as `/dev/vdc`.

> ⚠️ Do NOT point an image-backed data disk at a bootable OS image (especially
> the same one used for `imageRef`): the partition/filesystem UUIDs collide and
> the guest mounts a mix of partitions from both disks. Use a blank disk for
> empty volumes, or a data-only image. See `docs/api/data-disks.md`.

## Cleanup

```bash
kubectl delete swiftguest blank-datadisk-test datadisk-test
kubectl delete swiftimage data-disk ubuntu-noble
```
