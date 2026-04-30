# Storage RWX+Block Runtime Path (W9 follow-up to PR #32)

> Status: scoping doc — implementer briefing
> Last updated: 2026-04-30
> Predecessor: PR #32 — `docs/design/storage-access-mode.md`

## Framing

This is a **runtime-path gap revealed by the API-surface unblock**, not a
PR #32 regression. PR #32's stated scope was "let operators declare
`spec.storage.{accessMode, volumeMode, storageClassName}` on
SwiftGuestClass + SwiftGuest." Every piece of that surface is shipped and
validated:

| | Validated |
|---|---|
| CRD admission accepts RWX+Block | ✓ |
| Per-field merge of class + guest spec.storage | ✓ |
| `status.storage` echo | ✓ |
| `StorageReady=True` (Longhorn migratable=true class) | ✓ |
| `StorageReady=False / LonghornNotMigratable` (missing param) | ✓ |
| Per-guest clone PVC creation, RWX, longhorn-migratable | ✓ Bound |
| W8 RBAC (StorageClass list,watch) | ✓ shipped |

The post-merge cluster walkthrough then moved one step further along the
SwiftGuest lifecycle and surfaced W9 at the rootdisk Copy Job step:

> `Unable to attach or mount volumes ... volume dst has volumeMode Block,
> but is specified in volumeMounts`

The Copy Job mounts the destination as a filesystem path
(`volumeMounts: /dst`) and runs `cp /src/image.raw /dst/image.raw`. With
`volumeMode: Block`, the kubelet refuses the mount because Block PVCs
are surfaced as raw devices via `volumeDevices`, not as filesystem
paths via `volumeMounts`.

This is the **same pattern the project has been working through**:

| Layer | Reveals next layer's gap |
|---|---|
| Phase 1 offline migration | Phase 2's swiftletd plumbing requirements |
| Phase 2 walkthrough | PR #32's API-surface need (storage access mode) |
| PR #32 walkthrough | W9's runtime-path need (Block volumeMode end-to-end) |

Each layer's completion exposes the next. W9 is not surprising; it is
precisely the kind of finding the API-surface unblock was always going
to make addressable. The job here is to land it cleanly, not to
re-litigate the layering.

## Goal

`spec.storage.volumeMode: Block` SwiftGuests boot end-to-end, with the
root disk surfaced as a raw block device through Cloud Hypervisor's
`--disk path=/dev/...` argument, populated correctly from the source
SwiftImage's PVC, and surviving cloud-init's first-boot resize.

Acceptance criteria — when this lands:

- [ ] SwiftGuest with `storage: {accessMode: ReadWriteMany, volumeMode:
      Block, storageClassName: longhorn-migratable}` reaches `Phase=Running`
- [ ] `status.network.primaryIP` populated (full guest boot)
- [ ] Cloud-init `growpart` extends the partition + filesystem inside the
      guest to the SwiftGuestClass `rootDisk.size` (40 Gi by default)
- [ ] `swiftctl console` shows the guest at a login prompt; sentinel write
      survives a pod restart (block device data persistence test)
- [ ] No regression: existing Filesystem-mode guests (every existing
      SwiftGuest uses RWO+Filesystem by default) continue to boot
      identically — the Block path is opt-in via spec.storage

## Scope (three components)

### 1. Copy Job — `internal/controller/swiftguest/rootdisk.go`

Today (`createCloneJob`):

```go
VolumeMounts: []corev1.VolumeMount{
    {Name: "src", MountPath: "/src", ReadOnly: true},
    {Name: "dst", MountPath: "/dst"},
},
script := `cp /src/image.raw /dst/image.raw
qemu-img resize -f raw /dst/image.raw <size>
sgdisk -e /dst/image.raw`
```

Required for Block destinations:

```go
// Block path: src as filesystem mount (still readable as raw file),
// dst as raw device.
VolumeMounts: []corev1.VolumeMount{
    {Name: "src", MountPath: "/src", ReadOnly: true},
},
VolumeDevices: []corev1.VolumeDevice{
    {Name: "dst", DevicePath: "/dev/dst-block"},
},
script := `qemu-img convert -f raw -O raw /src/image.raw /dev/dst-block
sgdisk -e /dev/dst-block`
```

Decision rule on which path to take: read `rg.Storage.VolumeMode` from
the resolved spec (already plumbed by PR #32). The existing `volumeMode`
helper in PR #32 (`resolvedVolumeMode`) returns the resolved value;
`createCloneJob` should branch on it. Keep the Filesystem path
byte-identical to today's behaviour for backward compatibility — the
Block path is purely additive.

The qemu-img resize step is a no-op on block devices (a Longhorn
Migratable PVC is provisioned at the requested size; you cannot resize
a block device with qemu-img). `sgdisk -e` rewrites the GPT backup
header at the device's last sector and works on block devices natively.

Source image (`/src/image.raw`) is still a filesystem mount — the
SwiftImage's import PVC is RWO+Filesystem (today's default; see Open
question (a)). No source-side change unless (a)'s answer says so.

Both paths end with `sync` and an "OK" log line so the Job's success
condition stays the same.

### 2. Launcher pod builder — `internal/controller/swiftguest/pod.go`

Today the root disk is mounted at a filesystem path (e.g.
`/var/lib/kubeswift/disks/root/`) via `volumeMounts`, and swiftletd
generates `--disk path=/var/lib/kubeswift/disks/root/image.raw`. For
Block destinations:

- The pod's launcher container declares `volumeDevices` (e.g.
  `{Name: "rootdisk", DevicePath: "/dev/kubeswift-root"}`)
- The pod NO LONGER declares the corresponding `volumeMounts` entry
  for the root disk (mutually exclusive)
- The RuntimeIntent's `RootDisk.Path` is set to the device path
  instead of the filesystem path

Watch out: the Filesystem path also relies on the launcher's clone-grow-init
init container running `qemu-img resize` + `sgdisk -e` against the
filesystem-mounted PVC. For Block destinations:

- The clone-grow-init container needs `volumeDevices` instead of
  `volumeMounts` for the dst PVC
- `qemu-img resize` is a no-op (see above); the init container's
  contract is sgdisk-only on Block, or skipped entirely
- The grow-init's existence today is for the snapshot path's expand-and-wait
  contract; that constraint is independent of volumeMode

### 3. swiftletd (Rust) — `rust/swiftletd/`, `rust/swift-ch-client/`

The `RuntimeIntent.RootDisk.Path` field is consumed by swiftletd to
produce the CH `--disk` argument. Today the path is a filesystem path
ending in `image.raw`. For Block:

- The path is a device path (`/dev/...`) inside the launcher pod
- CH's `--disk path=/dev/...` is supported natively (CH treats the path
  as an opaque file/device handle and opens it raw)
- swiftletd's startup probes (e.g. checking the root disk size for
  logging) need to handle device paths — `stat()` works on devices, but
  some path-suffix-based logic (`.raw`/`.qcow2` detection) needs to be
  Block-aware

`swift-ch-client::config` already accepts arbitrary path strings via
`DiskConfig.path`; verify no implicit `image.raw` suffix is appended
anywhere in the spawn pipeline. The existing CH spawn rejects qcow2 at
runtime (raw-only invariant); device paths are raw by construction so
no new check needed.

## Open scoping questions (must be answered as part of W9)

These are NOT blockers for starting the work. They are scoping bounds
the implementer needs in their head from day one — answering them is
part of the W9 PR's deliverables, not a pre-requisite.

### (a) qcow2 → raw SwiftImage import pipeline against Block-mode PVCs

The SwiftImage import Job today downloads a qcow2 from
`spec.source.http.url`, runs `qemu-img convert -f qcow2 -O raw` to a
filesystem-mounted destination PVC. If the W9 follow-up only touches
guest-side clone+launcher, the **import pipeline stays on
Filesystem-mode PVCs** and the source SwiftImage continues to be
Filesystem; the Copy Job's source mount is unchanged. This is the
default assumption.

But: should `SwiftImage.spec.storage` exist as a peer to
`SwiftGuestClass.spec.storage`, allowing operators to import SwiftImages
directly to Block-mode PVCs? Argument for: avoids a Filesystem→Block
data hop on every clone. Argument against: scope creep; SwiftImage
import semantics are independent of guest runtime semantics. **Default:
defer SwiftImage.spec.storage to a future PR; W9 leaves the import
pipeline on Filesystem.** Document the assumption in the W9 PR
description so future operators know the import-class is fixed.

### (b) Cloud-init `growpart` on Block-mode root disk inside the guest

Cloud-init's `cloud-init-growpart` runs on first boot and extends the
last partition to fill the disk, then runs `resize2fs`/`xfs_growfs` on
the filesystem inside that partition. The disk is presented to the
guest as `/dev/vda` regardless of host-side volumeMode (the host's
Block vs Filesystem decision is invisible to the guest). The guest's
view of `/dev/vda` is the same raw device either way.

Expected: growpart works identically on Block-host-volumeMode guests as
on Filesystem-host-volumeMode guests, because the host-side mode never
crosses the virtio-blk boundary. The W9 PR must verify this empirically
on the cluster — a successful boot of an Ubuntu Noble guest on
RWX+Block must show `df` reporting the full 40 GiB after
`cloud-init-growpart` runs. If empirical reality differs (e.g.
Longhorn's Block-mode causes virtio-blk to surface a different
geometry), document the deviation and decide whether to ship a
workaround.

### (c) `qemu-img resize` + `sgdisk -e` against Block targets

`qemu-img resize` on a block device is a **no-op** — block devices have
their size fixed at provision time by the storage layer. This is fine
for Block-mode clones because the destination PVC is created at the
SwiftGuestClass `rootDisk.size` directly (vs the Filesystem path where
the destination PVC starts at the source-image size and is qemu-img
resized to grow the file inside it).

`sgdisk -e` rewrites the GPT backup header at the device's last sector.
This works on block devices natively — sgdisk operates byte-level
through the block device's standard read/write interface. Verify on
the cluster.

The W9 PR's clone Job script for Block:

```
qemu-img convert -f raw -O raw /src/image.raw /dev/dst-block
sgdisk -e /dev/dst-block
sync
```

vs the existing Filesystem script (preserved):

```
cp /src/image.raw /dst/image.raw
qemu-img resize -f raw /dst/image.raw <size>
sgdisk -e /dst/image.raw
sync
```

The Block script is shorter — no resize step, no `cp` (qemu-img convert
handles the bulk transfer atomically and supports sparse-aware writes).

## Out of scope

- **Other CSI drivers' Block support.** The storage architecture review
  PR #32 deferred to picks up Ceph RBD, AWS EBS, etc. W9 validates on
  Longhorn Migratable; other drivers' Block mode follows.
- **F2 split-brain mitigation on RWX.** Cloud Hypervisor's
  concurrent-write coordination behaviour on RWX disks is its own
  design problem; W9 lands the Block runtime path independent of
  whether the storage layer prevents split-brain. Phase 3 live
  migration's StopAndCopy phase is where F2 mitigation lives.
- **SwiftImage.spec.storage** for direct Block imports (see Open
  question (a)).
- **Snapshot path Block support.** PR #32 didn't validate the
  cloneStrategy=snapshot path against Block destinations; the snapshot
  path uses CSI dataSource + clone-grow-init. CSI dataSource probably
  handles volumeMode transparently (it's a property of the destination
  PVC's spec, not the dataSource). Verify in W9 testing; if it works,
  document; if it doesn't, surface as W9.x.

## Test plan

**Unit tests:**

- Block branch of `createCloneJob` produces correct VolumeDevices +
  script (no `cp`, qemu-img convert + sgdisk -e + sync).
- Filesystem branch unchanged (regression check).
- Launcher pod builder produces VolumeDevices for Block storage and
  VolumeMounts for Filesystem.

**Integration tests on cluster (the missing-by-construction test from
W5/W7/W8):**

- Apply RWX+Block SwiftGuestClass + SwiftGuest on `longhorn-migratable`.
- Verify `Phase=Running`, `status.network.primaryIP` populated.
- `swiftctl console` shows login prompt.
- Inside guest: `df -h /` reports ~40 GiB (cloud-init-growpart success).
- Restart launcher pod; verify sentinel file in guest survives.
- Apply RWO+Filesystem SwiftGuest in the same namespace; verify it
  reaches Running unchanged (no Block-path regression on the default
  configuration).

**Test infrastructure:**

This is the right place to land the operator-flow validation pattern
from Tracked Follow-up #2. The test harness should run on a real
cluster (k0s + Longhorn or equivalent), not a fake client. PR #22's
e2e-on-cluster.yaml workflow is the right scaffolding; add a
`test/storage/rwx-block-runtime.sh` that runs the steps above on
path-touch trigger for `internal/controller/swiftguest/**`,
`rust/swiftletd/**`, and `rust/swift-ch-client/**`.

## References

- PR #32 design doc: `docs/design/storage-access-mode.md`
- PR #32 walkthrough findings: `kubeswift_context.md` § "PR #32
  walkthrough findings (post-merge cluster validation)"
- W7 (cached-client RBAC, same-shape lesson as W8): `kubeswift_context.md`
  § "Phase 2 walkthrough resumption"
- KubeVirt's Block-mode root disks (reference architecture): KubeVirt
  uses `volumeMode: Block` for live-migratable VMIs by default and
  passes the device through libvirt to QEMU as a raw disk
- Longhorn Migratable RWX: requires `parameters.migratable: "true"` on
  the StorageClass; volumeMode must be Block (see PR #32
  `swiftguestclass-rwx-migratable.yaml` sample)
