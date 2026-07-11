# sandbox kernel profile

Direct-boot kernel + **bridge-initramfs** for the OCI-rootfs sandbox runtime: it
boots an OCI image as the VM root filesystem (read-only base + tmpfs overlay,
`switch_root`), the Firecracker/Kata model. Backs the `SwiftSandbox` kind
(`docs/design/swiftsandbox-design.md`).

Over `faas-minimal` (6.6 LTS base), the sandbox Linux config adds `OVERLAY_FS`,
`VIRTIO_FS`+`FUSE`, `VSOCK`, `SECCOMP`, cgroup v2, namespaces, and the userspace
essentials a stock OCI image needs (`SYSVIPC`, PTYs). Config:
[`configs/sandbox-linux.config`](configs/sandbox-linux.config). Bridge PID-1:
[`rootfs-overlay/init`](rootfs-overlay/init).

## Build

```
make build     # buildroot -> output/images/{bzImage,rootfs.cpio.gz}
make verify    # boot on cloud-hypervisor with a real OCI rootfs (asserts overlay + switch_root)
```

## Publish (SwiftKernel OCI artifact)

The artifact carries the same two blobs a SwiftKernel expects (`bzImage` +
`rootfs.cpio.gz`), pulled per node via the ORAS Job to
`/var/lib/kubeswift/kernels/<ns>-<name>/`:

```
cd output/images
oras push ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.1 \
  bzImage:application/vnd.kubeswift.kernel.binary \
  rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary
```

Then `kubectl apply -f config/samples/sandbox/swiftkernel-sandbox.yaml` on a
cluster with a `kubeswift.io/kernel-node=true` node.

## Boot contract (cmdline)

The SwiftSandbox controller appends to the kernel cmdline:

- `kubeswift.rootfs=block` — mount `/dev/vda` (the OCI ext4) RO as the overlay lower.
- `kubeswift.rootfs=virtiofs` — mount the `sandboxroot` virtio-fs tag as the lower.
- `kubeswift.entrypoint=<path>` — exec this after `switch_root` (default `/sbin/init`, then `/bin/sh`).
