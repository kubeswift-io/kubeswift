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

`make build` prints the built kernel's `CONFIG_MODULES` so you can see the config
actually took. It has to: buildroot applies `configs/sandbox-linux.config` (and a
profile fragment) to the kernel at its `linux-configure` step and then **stamps**
it, so a later edit to either file is silently ignored on an incremental build —
you get a kernel built from the OLD config with no warning. (That trap once
shipped a `sandbox` kernel carrying `CONFIG_MODULES=y` even though the config had
said `=n` for months.) The build target now re-applies the config whenever it is
newer than the stamp. If you ever suspect drift, `make -C buildroot O=<outdir>
linux-reconfigure` re-applies it unconditionally.

## Publish (SwiftKernel OCI artifact)

The artifact carries the blobs a SwiftKernel expects, pulled per node via the
ORAS Job to `/var/lib/kubeswift/kernels/<ns>-<name>/`: `bzImage`,
`rootfs.cpio.gz`, and `bridge-features`, the list of what this bridge
understands beyond argv/env/cwd (`make build` writes it from `BRIDGE_FEATURES`).

```
cd output/images                # or output-gpu-sandbox/images for the gpu profile
oras push ghcr.io/kubeswift-io/kubeswift/kernels/sandbox:6.6.15 \
  bzImage:application/vnd.kubeswift.kernel.binary \
  rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary \
  bridge-features:text/plain
# gpu-sandbox: oras push .../kernels/gpu-sandbox:6.6.4 bzImage:... rootfs.cpio.gz:... bridge-features:text/plain
```

Then `kubectl apply -f config/samples/sandbox/swiftkernel-sandbox.yaml` (or
`swiftkernel-gpu-sandbox.yaml`) on a cluster with a `kubeswift.io/kernel-node=true`
node.

## Boot contract

The SwiftSandbox controller appends to the kernel cmdline:

- `kubeswift.rootfs=block` — mount `/dev/vda` (the OCI ext4) RO as the overlay lower.
- `kubeswift.rootfs=virtiofs` — mount the `sandboxroot` virtio-fs tag as the lower.
- `kubeswift.config=/dev/vdX` — the config disk carrying the workload (below).
- `kubeswift.entrypoint=<path>` — legacy single entrypoint when there is no config disk.
- `kubeswift.dns-search=<a,b,...>` — search domains for the guest resolver.
- `kubeswift.idle=1` — a warm-pool keeper: no workload, idle until a checkout
  injects one over vsock.
- `kubeswift.model=virtiofs`, `kubeswift.modelpath=<path>` — mount the
  `sandboxmodel` virtio-fs tag read-only (spec.model).
- `kubeswift.mounts=<tag>:<path>[,...]` — mount each virtio-fs tag read-only at
  its path (spec.artifacts). Feature `mounts`.
- `kubeswift.stage=<tag>` (warm-pool slots): mount the tag read-only, nosuid,
  nodev at `/run/kubeswift-stage`, outside the new root. Feature `warm-mounts`.

The config disk is a raw blob: `KUBESWIFT-EXEC-V1`, then one TAB-separated line
per item, then `KUBESWIFT-EXEC-END`, NUL-padded. Values are base64.

- `ARGV <b64>`, `ENV <b64 KEY=VALUE>`, `CWD <b64>`.
- `FILE <b64 path> <octal mode> <b64 content>` — written into the writable
  layer (tmpfs) before the workload runs (spec.secretFiles). Feature `files`.

The bridge reads up to 4 MiB of it. A path must be absolute with no `.` or
`..` component. Any bridge failure prints `KUBESWIFT-EXIT-CODE=125` before it
powers off, so it reads as a failed sandbox rather than a completed one.

The guest agent's single-shot `exec` request takes the same files as
`files: [{path, mode, data}]` (data base64) for a warm-pool checkout. It also takes
`mounts: [{name, path}]` (feature `warm-mounts`): each staged directory
`/run/kubeswift-stage/<name>` is bound read-only (nosuid, nodev) at `path` in
the new root before the files and the command.

**Features.** A bridge ignores what it does not know, so an older one would run
the workload without its files or mounts. swiftletd therefore reads
`bridge-features` next to the kernel before asking for either, and fails the
sandbox if the feature is missing: point the SwiftKernel at a newer artifact.
