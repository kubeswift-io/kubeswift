# SwiftKernel

SwiftKernel is the CRD for managing kernel + initramfs artifacts across cluster nodes. It enables the kernel boot path: SwiftGuest VMs that boot directly into a Linux kernel without firmware, GRUB, or a disk image.

## Overview

Kernel boot is the alternative to disk boot. Where disk boot uses a cloud image with rust-hypervisor-firmware as the PVH bootloader, kernel boot passes a bzImage and initramfs directly to Cloud Hypervisor via `--kernel` and `--initramfs`. There is no cloud-init, no GRUB, no root filesystem on a persistent volume.

When to use kernel boot:

- Purpose-built microVMs with a minimal userspace (BusyBox, single binary)
- Sub-second cold start requirements
- Workloads that don't need cloud-init, SSH, or persistent storage
- Function-as-a-Service runtimes, sidecar VMs, sandboxed workloads

When to use disk boot:

- Full Linux distributions (Ubuntu, Fedora, Debian)
- Workloads that need cloud-init for user configuration
- Persistent root filesystems
- Network configuration via DHCP and cloud-init

## Node setup

SwiftKernel pulls artifacts only to nodes that opt in via the `kubeswift.io/kernel-node` label.

### Labeling nodes

```bash
kubectl label node worker-1 kubeswift.io/kernel-node=true
kubectl label node worker-2 kubeswift.io/kernel-node=true
```

### Checking labeled nodes

```bash
kubectl get nodes -l kubeswift.io/kernel-node=true
```

### What happens with no labeled nodes

If no nodes carry the label, SwiftKernel stays in `phase=Pending` with condition:

```
Ready   False   NoKernelNodes   No nodes labeled kubeswift.io/kernel-node=true found
```

The controller watches Node objects. When a node gets the label, the controller starts a pull Job for it without manual intervention.

### Removing the label

Removing the label from a node does not delete already-pulled artifacts. It prevents new SwiftKernels from pulling to that node. Existing SwiftGuest pods already scheduled on that node continue running.

## SwiftKernel CRD reference

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `ociRef.image` | string | Yes | OCI artifact reference |
| `ociRef.pullSecret` | string | No | Secret name for registry auth |
| `kernelCmdline` | string | No | Default kernel command line |
| `profile` | string | No | Profile name (informational) |

### Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | Pending, Pulling, Ready, Failed |
| `conditions` | []Condition | Ready and Failed conditions with reasons |
| `nodeStatuses` | []NodeKernelStatus | Per-node status: `{nodeName, phase}` |

### Phases

**Pending** — No pull Jobs have started. Either no labeled nodes exist, or the controller has not yet reconciled. Check the Ready condition for the specific reason.

**Pulling** — At least one node has a pull Job running. Other nodes may already be Ready.

**Ready** — All labeled nodes have pulled the artifact successfully. SwiftGuest can now reference this SwiftKernel.

**Failed** — A pull Job failed on at least one node. The Failed condition includes the node name and error message. This state requires manual intervention (fix the issue and delete the failed Job to retry).

### Example

```yaml
apiVersion: kernel.kubeswift.io/v1alpha1
kind: SwiftKernel
metadata:
  name: faas-minimal
  namespace: default
spec:
  ociRef:
    image: ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0
  kernelCmdline: "console=ttyS0 root=/dev/ram0 rdinit=/init"
  profile: faas-minimal
```

### Local path

Artifacts are stored at a deterministic path on each node:

```
/var/lib/kubeswift/kernels/<namespace>-<name>/
```

For the example above: `/var/lib/kubeswift/kernels/default-faas-minimal/`. This path is not stored in status — it is computed from the namespace and name.

The directory contains the raw OCI artifact layers as pulled by ORAS. For the faas-minimal profile, this means `bzImage` and `rootfs.cpio.gz`.

## Building a kernel profile

Kernel profiles use [Buildroot](https://buildroot.org/) to produce a Linux kernel and initramfs. The faas-minimal profile in `build/kernels/faas-minimal/` is the reference implementation.

### Prerequisites

- Linux build host (x86_64)
- Buildroot (cloned separately)
- Host packages: `build-essential`, `libelf-dev`, `libssl-dev`, `bc`, `flex`, `bison`, `cpio`, `unzip`, `rsync`, `wget`

### Profile directory structure

```
build/kernels/faas-minimal/
├── Config.in            # Buildroot external tree marker
├── external.desc        # External tree name and description
├── external.mk          # External tree Makefile (can be empty)
├── configs/
│   ├── faas_minimal_defconfig      # Buildroot defconfig
│   └── faas-minimal-linux.config   # Linux kernel config
├── rootfs-overlay/
│   └── init             # /init script (PID 1)
└── buildstuff.sh        # Build helper script
```

**Config.in** and **external.mk** are required by Buildroot's external tree mechanism but can be minimal (a comment line).

**external.desc** names the external tree:

```
name: KUBESWIFT_FAAS_MINIMAL
desc: KubeSwift faas-minimal MicroVM kernel profile
```

### Buildroot defconfig

The defconfig selects:

- `BR2_x86_64=y` — x86_64 target
- `BR2_TOOLCHAIN_BUILDROOT_MUSL=y` — musl libc (smaller than glibc)
- `BR2_STATIC_LIBS=y` — Static linking
- `BR2_LINUX_KERNEL=y` with custom config — Linux kernel
- `BR2_LINUX_KERNEL_BZIMAGE=y` — Build bzImage
- `BR2_TARGET_ROOTFS_CPIO=y` + `BR2_TARGET_ROOTFS_CPIO_GZIP=y` — Gzipped cpio initramfs
- `BR2_PACKAGE_BUSYBOX=y` — BusyBox userspace
- `BR2_ROOTFS_OVERLAY` — Custom `/init` from rootfs-overlay

### Linux kernel config

The kernel config must include Cloud Hypervisor compatibility:

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
CONFIG_TMPFS=y
CONFIG_PROC_FS=y
CONFIG_SYSFS=y
CONFIG_BINFMT_ELF=y
```

Without these, Cloud Hypervisor will fail to boot or the guest will have no console output.

### The /init script

The initramfs `/init` is PID 1. It must not exit (kernel panics on PID 1 exit). A minimal `/init`:

```sh
#!/bin/sh
set -e
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev 2>/dev/null || mount -t tmpfs none /dev
mkdir -p /dev/pts
mount -t devpts none /dev/pts 2>/dev/null || true
ip link set lo up 2>/dev/null || true
exec /bin/sh
```

The `exec /bin/sh` replaces the init process with a shell. Without `exec`, the shell runs as a child of init and Ctrl+D would cause a kernel panic.

### Build outputs

After building with Buildroot, the outputs are:

- `output/images/bzImage` — Linux kernel
- `output/images/rootfs.cpio.gz` — Compressed initramfs

These two files are what gets packaged as an OCI artifact.

## Packaging as OCI artifact

KubeSwift uses [ORAS](https://oras.land/) v1.3.1 to push and pull kernel artifacts as OCI artifacts.

### Push

Push from the directory containing the build outputs:

```bash
cd output/images/

oras push ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0 \
  bzImage:application/vnd.kubeswift.kernel.binary \
  rootfs.cpio.gz:application/vnd.kubeswift.initramfs.binary
```

Pushing from the artifact directory ensures clean layer titles (`bzImage` and `rootfs.cpio.gz` without path prefixes).

### Media types

| File | Media type |
|------|-----------|
| `bzImage` | `application/vnd.kubeswift.kernel.binary` |
| `rootfs.cpio.gz` | `application/vnd.kubeswift.initramfs.binary` |

### Verify the manifest

```bash
oras manifest fetch ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0 | jq .
```

Check that:
- Two layers are present
- Layer titles are `bzImage` and `rootfs.cpio.gz` (no path prefixes)
- Media types match the table above

### Pull test

```bash
mkdir -p /tmp/kernel-test && cd /tmp/kernel-test
oras pull ghcr.io/projectbeskar/kubeswift/kernels/faas:6.6.0
ls -lh
```

Expected:

```
-rw-r--r-- 1 user user 5.2M ... bzImage
-rw-r--r-- 1 user user 1.8M ... rootfs.cpio.gz
```

## Using SwiftKernel in SwiftGuest

### kernelRef

Set `spec.kernelRef.name` to the SwiftKernel name. Do not set `spec.imageRef` — the fields are mutually exclusive. Setting both causes a resolution error.

```yaml
spec:
  kernelRef:
    name: faas-minimal
  guestClassRef:
    name: default
```

### kernelCmdline override

The effective kernel command line is determined by priority:

1. SwiftGuest `spec.kernelCmdline` (highest priority)
2. SwiftKernel `spec.kernelCmdline` (default)

If neither is set, the cmdline is empty and the kernel uses its compiled-in default.

For the faas-minimal profile, a working cmdline is:

```
console=ttyS0 root=/dev/ram0 rdinit=/init
```

- `console=ttyS0` — Serial console output (required for `swiftctl console` to show anything)
- `root=/dev/ram0` — Root filesystem is the initramfs
- `rdinit=/init` — Run `/init` from the initramfs as PID 1

### No seedProfileRef

Kernel boot does not support cloud-init. The `seedProfileRef` field is ignored in the kernel boot path. If you need user configuration in a kernel boot guest, build it into the initramfs.

### Node targeting

When a SwiftGuest uses `kernelRef`, the controller automatically adds `nodeSelector: {"kubeswift.io/kernel-node": "true"}` to the pod. The pod will only schedule on nodes where the SwiftKernel artifacts have been pulled.

## Troubleshooting

### SwiftKernel stuck in Pending

**Cause:** No nodes have the `kubeswift.io/kernel-node=true` label.

**Fix:**
```bash
kubectl get nodes -l kubeswift.io/kernel-node=true
# If empty:
kubectl label node <node-name> kubeswift.io/kernel-node=true
```

### SwiftKernel stuck in Pulling

**Cause:** The pull Job is still running or cannot complete.

**Check Job logs:**
```bash
kubectl get jobs | grep swiftkernel-pull
kubectl logs job/swiftkernel-pull-faas-minimal-<nodename>
```

Common causes:
- OCI image reference is wrong (404 from registry)
- Registry requires authentication (set `spec.ociRef.pullSecret`)
- Node cannot reach the registry (network policy, proxy)
- Node disk is full

### SwiftGuest stuck in Scheduling with kernelRef

**Cause:** The pod cannot schedule. Either no labeled nodes exist, or the SwiftKernel is not Ready.

**Check:**
```bash
kubectl get swiftkernel faas-minimal -o jsonpath='{.status.phase}'
kubectl describe pod <guest-name>
```

The SwiftKernel must be Ready before the SwiftGuest controller creates the pod. If the SwiftKernel is not Ready, the resolver returns a resolution error and the guest enters Failed phase.

### Kernel panic in guest

**Cause:** The kernel command line is wrong, or the initramfs `/init` is missing or not executable.

**Check CH arguments:**
```bash
swiftctl debug <guest-name>
```

Verify that `--kernel`, `--initramfs`, and `--cmdline` are present and point to valid files. Check that the initramfs contains `/init` with execute permission.

[SwiftKernel API](api/swiftkernel.md) · [Kernel boot quickstart](kernel-boot-quickstart.md) · [Contributing: kernel profiles](contributing/kernel-profiles.md)
