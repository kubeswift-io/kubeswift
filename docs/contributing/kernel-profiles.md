# Contributing: Kernel Profiles

This guide covers how to add a new kernel profile to KubeSwift. A kernel profile is a Buildroot external tree that produces a bzImage and initramfs for direct kernel boot via SwiftKernel.

> Building a kernel for a **SwiftSandbox** (OCI-rootfs microVM) is a different
> contract — the init is a bridge that mounts an arbitrary OCI image, not a fixed
> workload. See [Build your own sandbox kernel and base images](../sandbox/build-your-own.md).

## When to add a new profile

Each profile targets a specific workload class. The faas-minimal profile exists for function-style workloads with no networking, no persistent storage, and a BusyBox shell. A new profile is warranted when the workload requires a different kernel configuration, a different userspace, or different init behavior that cannot be achieved by modifying the command line alone.

Examples of profiles that would justify a new directory:

- A profile with networking support (virtio-net, DHCP client in initramfs)
- A profile with a custom application binary instead of BusyBox
- A profile with GPU passthrough kernel modules (VFIO)
- A profile with vhost-user support

Do not create a new profile for minor command line variations. The `kernelCmdline` field on SwiftGuest and SwiftKernel handles those.

## Profile directory structure

Create a new directory under `build/kernels/`. Use a descriptive name:

```
build/kernels/<profile-name>/
├── Config.in
├── external.desc
├── external.mk
├── configs/
│   ├── <profile>_defconfig
│   └── <profile>-linux.config
├── rootfs-overlay/
│   └── init
└── buildstuff.sh
```

### Config.in

Required by Buildroot. Can be a comment:

```
# KubeSwift <profile-name> external tree
```

### external.desc

Names the external tree. The `name` field must be unique across all profiles:

```
name: KUBESWIFT_<PROFILE_NAME_UPPER>
desc: KubeSwift <profile-name> kernel profile
```

### external.mk

Required by Buildroot. Can be a comment if no custom packages:

```
# KubeSwift <profile-name> external tree
```

### Buildroot defconfig

Start from the faas-minimal defconfig and modify. The following settings are required for all KubeSwift kernel profiles:

```
BR2_x86_64=y
BR2_LINUX_KERNEL=y
BR2_LINUX_KERNEL_BZIMAGE=y
BR2_TARGET_ROOTFS_CPIO=y
BR2_TARGET_ROOTFS_CPIO_GZIP=y
BR2_ROOTFS_OVERLAY="$(BR2_EXTERNAL)/rootfs-overlay"
```

Everything else depends on the workload. Use `BR2_STATIC_LIBS=y` and musl for smallest output size. Add packages as needed for the target workload.

### Linux kernel config

Start from the faas-minimal linux config. The following are required for Cloud Hypervisor compatibility:

```
CONFIG_64BIT=y
CONFIG_HYPERVISOR_GUEST=y
CONFIG_PARAVIRT=y
CONFIG_KVM_GUEST=y
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_CONSOLE=y
CONFIG_BLK_DEV_INITRD=y
CONFIG_BINFMT_ELF=y
```

Without `CONFIG_SERIAL_8250` and `CONFIG_SERIAL_8250_CONSOLE`, the guest will produce no serial output and `swiftctl console` will show a blank screen.

Add configs for your workload. For networking: `CONFIG_VIRTIO_NET=y`, `CONFIG_NET=y`, `CONFIG_INET=y`. For GPU passthrough: `CONFIG_VFIO=y`, etc.

## /init requirements

The initramfs `/init` is PID 1. It must:

1. **Mount virtual filesystems:** `/proc`, `/sys`, `/dev` (proc, sysfs, devtmpfs)
2. **Bring up loopback:** `ip link set lo up` (needed for some applications)
3. **Exec the workload or keep PID 1 alive.** If PID 1 exits, the kernel panics.

A minimal template:

```sh
#!/bin/sh
set -e
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev 2>/dev/null || mount -t tmpfs none /dev
mkdir -p /dev/pts
mount -t devpts none /dev/pts 2>/dev/null || true
ip link set lo up 2>/dev/null || true

# Replace this with your workload:
exec /bin/sh
```

The `exec` is critical. Without it, `/bin/sh` runs as a child process. When the shell exits (e.g. Ctrl+D), init exits, and the kernel panics.

For an interactive serial console with login prompt, use `getty` instead of a bare shell:

```sh
exec /sbin/getty -L ttyS0 115200 vt100
```

BusyBox includes a `getty` implementation. The `-L` flag means "local" (no modem control). `ttyS0` matches the Cloud Hypervisor serial device.

Make `/init` executable: `chmod +x rootfs-overlay/init`.

## Verifying boot locally

Before pushing the OCI artifact, verify the kernel boots under Cloud Hypervisor:

```bash
cloud-hypervisor \
  --kernel output/images/bzImage \
  --initramfs output/images/rootfs.cpio.gz \
  --cmdline "console=ttyS0 root=/dev/ram0 rdinit=/init" \
  --memory size=128M \
  --cpus boot=1 \
  --serial tty \
  --console off
```

You should see kernel boot messages and the init output on your terminal. Ctrl+C to stop.

If you see no output, check that `CONFIG_SERIAL_8250_CONSOLE=y` is in the kernel config and `console=ttyS0` is in the command line.

## OCI packaging checklist

Before pushing:

1. **Push from the artifact directory** (not from a parent directory with subdirectories):
   ```bash
   cd output/images/
   oras push <registry>/<repo>:<tag> \
     bzImage:application/vnd.kubeswift.kernel.binary \
     rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary
   ```

2. **Verify manifest titles** — Layer annotations should show `bzImage` and `rootfs.cpio.gz` without path prefixes:
   ```bash
   oras manifest fetch <registry>/<repo>:<tag> | jq '.layers[].annotations'
   ```

3. **Verify media types** — Each layer should have the correct `mediaType`.

4. **Test pull** — Pull to a clean directory and verify both files are present:
   ```bash
   mkdir -p /tmp/test-pull && cd /tmp/test-pull
   oras pull <registry>/<repo>:<tag>
   ls -lh
   ```

## PR checklist

A kernel profile PR must include:

- [ ] `build/kernels/<profile>/configs/<profile>_defconfig`
- [ ] `build/kernels/<profile>/configs/<profile>-linux.config`
- [ ] `build/kernels/<profile>/rootfs-overlay/init` (executable)
- [ ] `build/kernels/<profile>/Config.in`, `external.desc`, `external.mk`
- [ ] `build/kernels/<profile>/buildstuff.sh` (build helper)
- [ ] `config/samples/swiftkernel-<profile>.yaml` (sample manifest)
- [ ] Boot verification output in PR description (terminal screenshot or paste of kernel boot + init output)
- [ ] Local Cloud Hypervisor boot test (the verification command above)

[SwiftKernel reference](../swiftkernel.md) · [faas-minimal profile](https://github.com/kubeswift-io/kubeswift/tree/main/build/kernels/faas-minimal)
