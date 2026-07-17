# Sandbox scratch / persistent disks

A SwiftSandbox's rootfs is an ephemeral, RAM-backed overlay on a read-only OCI
image — fine for short jobs, too small (and non-durable) for build caches,
dataset staging, checkpoints, or a GPU model/weight cache. `spec.scratchDisk`
attaches **one secondary block disk** to the sandbox guest.

It reuses the SwiftGuest v0.4.2 blank-data-disk runtime path: a Block PVC is
attached as a **raw device** (typically `/dev/vdc` in the guest). The workload
runs `mkfs` + `mount`, or uses the raw device. There is no auto-mount in v1.

## Two shapes

```yaml
spec:
  scratchDisk:
    blank:                 # a NEW sized Block PVC, OWNED by the sandbox
      size: 20Gi           #   → deleted (GC'd) with the sandbox
      # storageClassName: longhorn   # optional; empty = cluster default
```

```yaml
spec:
  scratchDisk:
    pvcRef:                # an EXISTING Block PVC — PERSISTS beyond the sandbox
      name: model-cache    #   (not sandbox-owned; reuse it across runs)
```

Exactly one of `blank` / `pvcRef`. Blank is **Block volumeMode only** in v1
(a Filesystem escape hatch would need a fill Job + a mount step). `pvcRef` must
reference a Block-mode PVC.

## Lifecycle

- The sandbox does not boot until the disk's PVC is **Bound**
  (`status.conditions[type=ScratchDiskReady]`, `status.scratchDisk.bound`).
- A `blank` PVC is `<sandbox>-scratch`, owned by the SwiftSandbox — it is
  garbage-collected when the sandbox is deleted. A `pvcRef` PVC is left alone.
- `status.scratchDisk.devicePath` is the launcher-side path
  (`/dev/kubeswift-data-scratch`); inside the guest it is a raw virtio-blk
  device.

## Using it

The workload gets a raw device — format + mount it, or use it raw:

```yaml
command: ["/bin/sh", "-c",
  "mkfs.ext4 -F /dev/vdc && mount /dev/vdc /scratch && exec my-job"]
```

For a **persistent** disk reused across runs, don't re-mkfs an existing
filesystem:

```yaml
command: ["/bin/sh", "-c",
  "mount /dev/vdc /cache 2>/dev/null || (mkfs.ext4 -F /dev/vdc && mount /dev/vdc /cache); exec my-job"]
```

## Not yet

- **Auto-mkfs + mount at a path** (a bridge-init addition) — so the workload
  just sees a directory. Follow-up.
- **Pool-shared read-only cache** across warm `SwiftSandboxPool` slots (the GPU
  weight-cache case) — tracked with the warm-GPU-pool work.
