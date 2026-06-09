# vhost-user devices: virtiofs & vhost-user-net

KubeSwift ships two [vhost-user](https://www.qemu.org/docs/master/interop/vhost-user.html)
devices: **virtiofs** (shared filesystems) and **vhost-user-net** (operator
DPDK fast-path networking). The architecture and roadmap live in
[`docs/design/vhost-user-devices.md`](design/vhost-user-devices.md). Both rely on
shared guest memory, which KubeSwift already maps by default (`--memory shared=on`).

---

# virtiofs shared filesystems (vhost-user-fs)

Share a host directory or a PersistentVolumeClaim into a SwiftGuest as a
[virtiofs](https://virtio-fs.gitlab.io/) mount. This is the first vhost-user
device KubeSwift ships.

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

---

# vhost-user-net (operator DPDK fast-path)

A `vhost-user` interface gives a guest a virtio-net device whose datapath is an
**operator-provided** vhost-user backend (DPDK / OVS-DPDK). It only beats the
default tap/bridge path when paired with such a userspace fast-path — which is
node-level operator infrastructure. **KubeSwift does not run the backend**; it
mounts the backend's socket into the launcher and points Cloud Hypervisor at it,
exactly as SR-IOV expects the VFs to be pre-provisioned.

## Requirements

- **Cloud Hypervisor path only** (v1). The webhook rejects a `vhost-user`
  interface together with `spec.gpuProfileRef` (which may select QEMU).
- An operator-run vhost-user-net backend on the node exposing a listener socket
  (e.g. `/var/run/vhost/fast0.sock`). KubeSwift mounts the socket's parent
  directory into the launcher (hostPath) so CH can connect.

## Fields

```yaml
spec:
  interfaces:
    - name: mgmt          # keep a normal bridge NIC for DHCP/SSH
      type: bridge
    - name: fast0
      type: vhost-user
      socket: /var/run/vhost/fast0.sock   # required: operator backend listener
      mac: "52:54:00:00:fa:00"            # optional; generated if unset
```

Webhook rules: `socket` is required; `networkRef` and `resourceName` are not
used; not combinable with `gpuProfileRef`. Sockets that share a directory are
mounted once (deduped). A `vhost-user` NIC is never the primary DHCP interface —
pair it with a `bridge` NIC for normal pod networking.

The guest sees an ordinary `virtio-net` device — no in-guest configuration
beyond normal NIC setup. The line rate comes from the backend.

## Validation status

The **wiring** is validated (CH spawns `--net vhost_user=on,socket=...`; the
webhook accepts/bounds it; the socket dir is mounted). The **line-rate datapath
is asset-gated** — it needs a DPDK NIC + backend on the node, which the dev
cluster lacks. This is the same honest posture as the shipped-but-hardware-
unvalidated SR-IOV path.

Example:
[`config/samples/vhost-user-net/swiftguest-vhost-user-net.yaml`](../config/samples/vhost-user-net/swiftguest-vhost-user-net.yaml).

---

# vhost-user devices: blk & generic

Beyond virtiofs and vhost-user-net, `spec.vhostUserDevices[]` attaches two more
operator-backed vhost-user device kinds to a guest:

- **`type: blk`** — a **vhost-user-blk** disk (e.g. an SPDK vhost target). It
  appears as an ordinary virtio-blk disk in the guest; the IOPS come from the
  userspace backend. Cloud Hypervisor `--disk vhost_user=on,socket=`.
- **`type: generic`** — **any** vhost-user device by its virtio id, for backends
  that aren't net/blk/fs. Cloud Hypervisor
  `--generic-vhost-user virtio_id=,socket=,queue_sizes=`.

As with vhost-user-net, **KubeSwift does not run the backend** — the operator's
datapath exposes a node socket; KubeSwift mounts its directory into the launcher
(deduped with the vhost-user-net socket mounts) and points CH at it.

## Fields

```yaml
spec:
  vhostUserDevices:
    - name: fastdisk        # required; unique per guest
      type: blk             # blk | generic
      socket: /var/run/spdk/vhost.0   # required: operator backend listener
    - name: custom0
      type: generic
      virtioId: "block"     # required for generic: number or symbolic name
      socket: /var/run/vhost/custom0.sock
      queueSizes: [1024, 1024]   # optional (generic only)
```

Webhook rules: unique `name`; `socket` required; `virtioId` required for
`generic`; `type` must be `blk` or `generic`; not combinable with
`gpuProfileRef` (CH-only in v1).

## Validation status

**Wiring only** — the CH args, webhook bounds, and socket-dir mount are
unit-validated. The datapath is **asset-gated**: `blk` needs an SPDK (or other
vhost-user-blk) target on the node; `generic` needs whatever backend the
`virtioId` refers to. Same honest posture as SR-IOV and vhost-user-net.

Examples:
[`config/samples/vhost-user-devices/`](../config/samples/vhost-user-devices/).
