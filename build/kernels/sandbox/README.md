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

## Profiles

| PROFILE | OUTPUT_DIR | Delta over base | For |
|---|---|---|---|
| `sandbox` (default) | `output/` | — | all sandboxes |
| `gpu-sandbox` | `output-gpu-sandbox/` | `CONFIG_MODULES` (loadable modules) | GPU sandboxes — the OCI image `insmod`s the NVIDIA driver |

`gpu-sandbox` is the base sandbox config + a 3-line
[`configs/gpu-sandbox.fragment`](configs/gpu-sandbox.fragment) merged on top (via
`BR2_LINUX_KERNEL_CONFIG_FRAGMENT_FILES`), so the two share one base and never
drift. The NVIDIA driver rides the guest OCI image, not the kernel — the only
kernel delta a GPU sandbox needs is being able to load it (the base kernel is
monolithic). Proven by the GPU-sandbox Phase-0 spike (a GTX 1080 proprietary
driver built against this kernel → `nvidia-smi` over firmware-less mode-3 VFIO).

## Build

```
make build                      # sandbox     -> output/images/{bzImage,rootfs.cpio.gz}
make build PROFILE=gpu-sandbox  # gpu-sandbox -> output-gpu-sandbox/images/...
make verify                     # boot on cloud-hypervisor with a real OCI rootfs
```

Each profile builds into its own `OUTPUT_DIR`; override `OUTPUT_DIR=<dir>` to
reuse an already-built toolchain (a fresh dir rebuilds the toolchain from scratch).

## Publish (SwiftKernel OCI artifact)

The artifact carries the same two blobs a SwiftKernel expects (`bzImage` +
`rootfs.cpio.gz`), pulled per node via the ORAS Job to
`/var/lib/kubeswift/kernels/<ns>-<name>/`:

```
cd output/images                # or output-gpu-sandbox/images for the gpu profile
oras push ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.11 \
  bzImage:application/vnd.kubeswift.kernel.binary \
  rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary
# gpu-sandbox: oras push .../kernels/gpu-sandbox:6.6.1 bzImage:... rootfs.cpio.gz:...
```

Then `kubectl apply -f config/samples/sandbox/swiftkernel-sandbox.yaml` (or
`swiftkernel-gpu-sandbox.yaml`) on a cluster with a `kubeswift.io/kernel-node=true`
node.

## Boot contract (cmdline)

The SwiftSandbox controller appends to the kernel cmdline:

- `kubeswift.rootfs=block` — mount `/dev/vda` (the OCI ext4) RO as the overlay lower.
- `kubeswift.rootfs=virtiofs` — mount the `sandboxroot` virtio-fs tag as the lower.
- `kubeswift.entrypoint=<path>` — exec this after `switch_root` (default `/sbin/init`, then `/bin/sh`).
