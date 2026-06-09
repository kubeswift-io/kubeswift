# virtiofs shared filesystems (vhost-user-fs)

Share a host directory or a PersistentVolumeClaim into a SwiftGuest as a
[virtiofs](https://virtio-fs.gitlab.io/) mount. This is the first vhost-user
device KubeSwift ships; the architecture and roadmap live in
[`docs/design/vhost-user-devices.md`](design/vhost-user-devices.md).

## What it is

Each entry in `spec.filesystems` becomes one virtiofs device on the guest.
For each entry swiftletd spawns a `virtiofsd` backend (sharing the in-pod
source directory) and hands Cloud Hypervisor `--fs tag=<tag>,socket=<sock>`.
The guest mounts it with `mount -t virtiofs <tag> <mountpoint>`.

virtiofs gives near-native shared-filesystem performance and POSIX semantics
(unlike a 9p mount), and lets several guests — or several nodes, via an RWX
PVC — see the same content live.

## Requirements

- **Cloud Hypervisor path only.** virtiofs is not wired on the QEMU/GPU path
  in v1; the webhook rejects `spec.filesystems` together with
  `spec.gpuProfileRef`. Linux guests only (the webhook rejects
  `osType: windows` — no virtio-fs guest driver is shipped in v1).
- **Guest kernel with `CONFIG_VIRTIO_FS`.** Ubuntu Noble and Rocky 9 cloud
  images have it; the `faas-minimal` SwiftKernel does not (kernel-boot guests
  need a kernel built with virtiofs).
- No cluster prerequisite beyond the standard swiftletd image — it ships
  `virtiofsd`, and the shared-memory backing (`--memory shared=on`) is already
  on by default.

## Fields

```yaml
spec:
  filesystems:
    - name: scratch         # required; per-guest id (drives the socket name).
                            # lowercase alphanumeric + '-', <= 36 chars.
      tag: scratch          # optional; the virtiofs mount tag. Defaults to name.
      readOnly: false       # optional; true => the guest cannot write the share.
      source:               # required; exactly one of hostPath or pvcRef.
        hostPath: /srv/data  #   node-local dir (created if absent).
        # pvcRef:
        #   name: shared-pvc #   a PVC (use RWX to share across guests/nodes).
```

Rules the admission webhook enforces:

- `name` and the effective `tag` (defaults to `name`) are unique within a guest.
- `source` sets **exactly one** of `hostPath` / `pvcRef`.
- not combinable with `gpuProfileRef`, nor with `osType: windows`.

`readOnly: true` is enforced at the launcher pod's volumeMount (and the PVC
volume), so the guest physically cannot mutate the backing content.

## Using it from the guest

After the guest boots:

```bash
sudo mkdir -p /mnt/scratch
sudo mount -t virtiofs scratch /mnt/scratch     # "scratch" = the tag
```

Persist across reboots via `/etc/fstab`:

```
scratch /mnt/scratch virtiofs defaults,nofail 0 0
```

## Examples

- [`config/samples/virtiofs/swiftguest-virtiofs-hostpath.yaml`](../config/samples/virtiofs/swiftguest-virtiofs-hostpath.yaml)
  — node-local hostPath share.
- [`config/samples/virtiofs/swiftguest-virtiofs-pvc-readonly.yaml`](../config/samples/virtiofs/swiftguest-virtiofs-pvc-readonly.yaml)
  — read-only share of an RWX PVC (e.g. a shared model store).

## Notes & limits

- **hostPath is node-local.** A `hostPath` share pins the content to one node;
  it is not replicated. For portable/shared content use an RWX PVC.
- **Live migration.** A guest with `spec.filesystems` migrates like a VFIO
  guest — offline only (the virtiofsd backend is node-local state). Snapshots
  are unaffected.
- **Security boundary.** virtiofsd runs inside the launcher pod with
  `--sandbox none`; the pod (its mount namespace + the single shared source
  mount) is the boundary. The launcher uses no extra Linux capabilities for
  this — consistent with the project's no-privileged-containers rule.
