# Build your own sandbox kernel and base images

KubeSwift is OCI-native and pluggable: you can build your own [sandbox](overview.md)
kernel profiles and base images and keep an in-house library, without changing any
KubeSwift code. This page documents the two contracts an adopter needs — the
**sandbox kernel contract** (the bridge-initramfs) and the **base-image contract**
(the OCI image a sandbox runs) — and how to publish and sign them.

A sandbox kernel is *not* a plain kernel-boot SwiftGuest kernel (see
[Contributing: Kernel Profiles](../contributing/kernel-profiles.md) for those). A
SwiftGuest kernel's init execs a fixed workload; a sandbox kernel's init is a
**bridge** that mounts an arbitrary OCI image as the root filesystem and runs the
image's entrypoint. `build/kernels/sandbox` is the reference implementation.

## The sandbox kernel contract (bridge-initramfs)

A sandbox kernel = a `bzImage` + an initramfs whose `/init` is the bridge. The
SwiftSandbox controller appends these to the kernel cmdline at launch — your
bridge must honor them:

| Cmdline key | Meaning |
|---|---|
| `console=ttyS0` | serial console (the sandbox log) |
| `kubeswift.rootfs=block` | mount `/dev/vda` (the RO OCI ext4) as the overlay lower |
| `kubeswift.rootfs=virtiofs` | mount the `sandboxroot` virtio-fs tag as the lower |
| `kubeswift.config=/dev/vdX` | the per-sandbox config disk carrying the workload exec |
| `kubeswift.dns-search=a,b,c` | search domains to write into the guest `/etc/resolv.conf` |
| `kubeswift.idle=1` | warm-pool keeper: boot to an idle loop, no workload (a checkout injects one later over vsock) |
| `ip=dhcp` | (networked sandboxes) kernel IP autoconfig |

The bridge (`build/kernels/sandbox/rootfs-overlay/init`) must:

1. Mount `/proc`, `/sys`, devtmpfs `/dev`, devpts.
2. Mount the OCI rootfs **read-only** as an overlay lower + a tmpfs upper, and
   assemble the merged tree (the signed image is never mutated). Retry the
   virtio-blk mount — the device node lags PID 1.
3. Read the config disk **before** it moves `/proc`/`/dev`, and write the guest
   `/etc/resolv.conf` from `/proc/net/pnp` + `kubeswift.dns-search`.
4. **Supervise** the workload — run it as a foreground child (chroot into the
   merged tree, or the guest agent's chroot+chdir+exec for `workingDir`), NOT as
   `exec`-PID-1 (its exit would panic the kernel). Capture the exit code.
5. Emit `KUBESWIFT-EXIT-CODE=<n>` on `/dev/console`, then `poweroff -f`.
6. Start the vsock guest agent in the background (for `swiftctl sandbox
   exec/attach`), if baked in.

### The config-disk exec blob

The controller renders the workload's argv/env/cwd into a raw block device (never
the cmdline — that leaks env to `/proc/cmdline` and the host's `ps`). The format
is line-based, sh-parseable, no filesystem:

```
KUBESWIFT-EXEC-V1
CWD<TAB><base64 of the working dir>
ARGV<TAB><base64 of argv[0]>
ARGV<TAB><base64 of argv[1]>
ENV<TAB><base64 of KEY=VALUE>
KUBESWIFT-EXEC-END
```

Values are base64 (so a multi-line `sh -c` script survives); the file is padded to
a 512-byte multiple. The bridge slurps the device in one `dd`, strips NULs, and
parses line by line, breaking at `KUBESWIFT-EXEC-END`.

### Kernel config

Start from `build/kernels/sandbox/configs/sandbox-linux.config`. A sandbox kernel
needs, beyond the Cloud Hypervisor essentials: `OVERLAY_FS` (RO base + tmpfs
overlay), `VIRTIO_FS`+`FUSE` (the virtiofs rootfs mode), `VSOCKETS`+
`VIRTIO_VSOCKETS` (the exec/attach agent), and the real-userspace essentials a
stock OCI image needs (`SYSVIPC`, `UNIX98_PTYS`, cgroup v2, namespaces).

**Declare every driver's dependencies.** Buildroot's `olddefconfig` silently drops
an enabled symbol whose Kconfig dependency is unmet (e.g. `VIRTIO_PCI` without
`PCI` → a kernel with no virtio at all). `make verify-sandbox-config` is a static
dep-lint that catches exactly this; run it on your config.

### A derived profile (worked example)

To add capability to the base sandbox kernel without forking the whole config, use
a Buildroot **config fragment**. The `gpu-sandbox` profile is the base sandbox
config + a 3-line `configs/gpu-sandbox.fragment` (`CONFIG_MODULES` for the NVIDIA
driver), merged via `BR2_LINUX_KERNEL_CONFIG_FRAGMENT_FILES`, selected with
`make build PROFILE=gpu-sandbox`. Copy that pattern for your own capability
(vhost-user, a custom driver, RT).

### Publish

Package the two blobs as an OCI artifact and reference it from a SwiftKernel:

```bash
cd output/images
oras push <registry>/<repo>/kernels/<name>:<tag> \
  bzImage:application/vnd.kubeswift.kernel.binary \
  rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary
```

```yaml
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata: { name: my-sandbox }
spec:
  ociRef: { image: <registry>/<repo>/kernels/<name>:<tag> }
  profile: my-sandbox
```

A SwiftSandbox selects it with `spec.kernelProfileRef: {name: my-sandbox}`.

## The base-image contract

Any OCI image runs as a sandbox root filesystem — **there is no KubeSwift-specific
requirement**. The bridge supplies the kernel, `/proc`/`/sys`/`/dev`, and the
overlay; your image supplies the userspace. The workload is the image's
`ENTRYPOINT`+`CMD` (overridable per-sandbox via `spec.command`/`args`/`env`/
`workingDir`). Distroless images work (the bridge execs the binary directly; no
shell needed). For a warm-poolable distroless image, nothing extra is required —
the idle keeper runs in the initramfs busybox, not your image.

The only images that need special contents are **capability** images — e.g. a GPU
sandbox image ships the NVIDIA driver built for the matching `gpu-sandbox` kernel
(see [GPU sandboxes](gpu-sandboxes.md)).

## An in-house library (private registry + signing)

Everything above is registry-agnostic. Publish your kernels and base images to a
private registry and reference them by digest:

- **Private base images** — `SwiftSandbox.spec.imagePullSecret` (a
  `kubernetes.io/dockerconfigjson` Secret) authenticates the pull.
- **Signed base images** — `SwiftSandbox.spec.verifyKeySecretRef` (a cosign public
  key) makes the sandbox **cosign-verify the image before it boots**; an unsigned
  or tampered image fails and never runs. `SwiftSandboxPool` takes the same field,
  so a pool never warms an unverified image. Sign with
  `cosign sign --key cosign.key <repo>@sha256:...`.
- **Pin by digest** (`repo@sha256:...`) for reproducibility.

This is the same OCI-native model KubeSwift uses for golden VM disks
(`SwiftImage.spec.source.oci`) and kernels (`SwiftKernel.spec.ociRef`) — one
library, one registry, one signing story across VMs, kernels, and sandboxes.

## See also

- [Running sandboxes](overview.md) · [GPU sandboxes](gpu-sandboxes.md) · [Warm pools](warm-pool.md)
- [Contributing: Kernel Profiles](../contributing/kernel-profiles.md) — kernel-boot SwiftGuest kernels
- Reference: [`build/kernels/sandbox`](https://github.com/kubeswift-io/kubeswift/tree/main/build/kernels/sandbox)
