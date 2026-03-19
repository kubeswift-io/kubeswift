# KubeSwift faas-minimal external tree
```

---

### `build/kernels/faas-minimal/configs/faas_minimal_defconfig`

This is the heart of the build. Every option is deliberate — nothing speculative.
```
# Architecture
BR2_x86_64=y
BR2_x86_x86_64=y

# Toolchain — use buildroot internal musl toolchain
BR2_TOOLCHAIN_BUILDROOT=y
BR2_TOOLCHAIN_BUILDROOT_MUSL=y

# C library: musl (required for static BusyBox)
BR2_TOOLCHAIN_BUILDROOT_LIBC="musl"

# Static linking for userspace
BR2_STATIC_LIBS=y

# Linux kernel
BR2_LINUX_KERNEL=y
BR2_LINUX_KERNEL_LATEST_LTS_VERSION=y
BR2_LINUX_KERNEL_USE_CUSTOM_CONFIG=y
BR2_LINUX_KERNEL_CUSTOM_CONFIG_FILE="$(BR2_EXTERNAL)/configs/faas-minimal-linux.config"
BR2_LINUX_KERNEL_BZIMAGE=y

# Initramfs — rootfs IS the initramfs, packed as cpio.gz
BR2_TARGET_ROOTFS_CPIO=y
BR2_TARGET_ROOTFS_CPIO_GZIP=y

# BusyBox
BR2_PACKAGE_BUSYBOX=y
BR2_PACKAGE_BUSYBOX_CONFIG="$(BR2_EXTERNAL)/configs/faas-minimal-busybox.config"

# No package manager, no cloud-init, no systemd
BR2_PACKAGE_SYSTEMD=n
BR2_ROOTFS_INIT_BUSYBOX=y

# Custom /init overlay
BR2_ROOTFS_OVERLAY="$(BR2_EXTERNAL)/rootfs-overlay"

# System settings
BR2_TARGET_GENERIC_HOSTNAME="faas"
BR2_TARGET_GENERIC_ISSUE=""
BR2_TARGET_GENERIC_ROOT_PASSWD=""

# Strip binaries
BR2_STRIP_strip=y
BR2_OPTIMIZE_S=y
```

---

### `build/kernels/faas-minimal/configs/faas-minimal-linux.config`

This is derived from the Cloud Hypervisor reference config at `resources/linux-config-x86_64`, with additions for serial console and virtio. It is a fragment config (not a full defconfig) that buildroot merges via `merge_config.sh`.

Actually — buildroot expects `BR2_LINUX_KERNEL_CUSTOM_CONFIG_FILE` to point to a *full* config. The safest approach is to start from the CH reference config and add our required symbols on top. Here's the strategy:

The file below is a minimal full `.config` covering everything required. It is intentionally small — we rely on `make olddefconfig` inside buildroot to fill in the rest with safe defaults.
```
# Cloud Hypervisor faas-minimal kernel config
# Based on: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/resources/linux-config-x86_64
# Additions: serial console, initramfs support

CONFIG_64BIT=y
CONFIG_X86_64=y
CONFIG_SMP=y
CONFIG_NR_CPUS=128

# PVH / hypervisor guest support (required for cloud-hypervisor --kernel)
CONFIG_HYPERVISOR_GUEST=y
CONFIG_PARAVIRT=y
CONFIG_KVM_GUEST=y
CONFIG_XEN=n

# Serial console (required for --serial tty and console=ttyS0)
CONFIG_TTY=y
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_SERIAL_8250_NR_UARTS=4
CONFIG_SERIAL_8250_RUNTIME_UARTS=4

# Virtio (required for cloud-hypervisor virtio devices)
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_PCI_LEGACY=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_CONSOLE=y
CONFIG_HW_RANDOM_VIRTIO=y

# Block devices
CONFIG_BLK_DEV=y
CONFIG_BLK_DEV_INITRD=y

# Filesystems
CONFIG_TMPFS=y
CONFIG_PROC_FS=y
CONFIG_SYSFS=y
CONFIG_EXT4_FS=y
CONFIG_BINFMT_ELF=y
CONFIG_BINFMT_SCRIPT=y

# Networking (needed for any faas networking)
CONFIG_NET=y
CONFIG_INET=y
CONFIG_IP_PNP=y
CONFIG_IP_PNP_DHCP=y
CONFIG_PACKET=y
CONFIG_UNIX=y

# Initramfs
CONFIG_BLK_DEV_INITRD=y
CONFIG_INITRAMFS_SOURCE=""

# Memory
CONFIG_MEMORY_HOTPLUG=y
CONFIG_MEMORY_HOTREMOVE=y

# Disable things that slow boot or add noise
CONFIG_MODULES=n
CONFIG_DEBUG_KERNEL=n
CONFIG_SWAP=n
CONFIG_SYSVIPC=n
CONFIG_AUDIT=n
CONFIG_SECCOMP=n
CONFIG_INOTIFY_USER=y
```

---

### `build/kernels/faas-minimal/configs/faas-minimal-busybox.config`

Minimal BusyBox — only what `/init` and basic shell operation needs.
```
CONFIG_BUSYBOX_CONFIG_STATIC=y
CONFIG_BUSYBOX_CONFIG_FEATURE_PREFER_APPLETS=y

# Shell
CONFIG_BUSYBOX_CONFIG_SH_IS_ASH=y
CONFIG_BUSYBOX_CONFIG_ASH=y
CONFIG_BUSYBOX_CONFIG_ASH_PRINTF=y

# Core utils
CONFIG_BUSYBOX_CONFIG_ECHO=y
CONFIG_BUSYBOX_CONFIG_PRINTF=y
CONFIG_BUSYBOX_CONFIG_TEST=y
CONFIG_BUSYBOX_CONFIG_MOUNT=y
CONFIG_BUSYBOX_CONFIG_UMOUNT=y
CONFIG_BUSYBOX_CONFIG_LS=y
CONFIG_BUSYBOX_CONFIG_CAT=y
CONFIG_BUSYBOX_CONFIG_GREP=y
CONFIG_BUSYBOX_CONFIG_SLEEP=y
CONFIG_BUSYBOX_CONFIG_PS=y
CONFIG_BUSYBOX_CONFIG_KILL=y
CONFIG_BUSYBOX_CONFIG_DMESG=y
CONFIG_BUSYBOX_CONFIG_IP=y
CONFIG_BUSYBOX_CONFIG_UDHCPC=y

# No package manager, no vi, no wget (can add later)
CONFIG_BUSYBOX_CONFIG_DPKG=n
CONFIG_BUSYBOX_CONFIG_RPM=n
CONFIG_BUSYBOX_CONFIG_WGET=n