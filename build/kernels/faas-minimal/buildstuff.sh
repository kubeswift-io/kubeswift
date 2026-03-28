cd ~/code/vmm-kubeswift/kubeswift/build/kernels/faas-minimal

# Fix external.mk (currently has wrong content)
cat > external.mk << 'EOF'
# KubeSwift faas-minimal external tree
EOF

# Fix Config.in (rename from Config.ini if needed)
mv Config.ini Config.in 2>/dev/null || true
cat > Config.in << 'EOF'
# KubeSwift faas-minimal external tree
EOF

# Create external.desc (the missing file that caused the error)
cat > external.desc << 'EOF'
name: KUBESWIFT_FAAS_MINIMAL
desc: KubeSwift faas-minimal MicroVM kernel profile
EOF

# Create configs directory and files
mkdir -p configs

cat > configs/faas_minimal_defconfig << 'EOF'
BR2_x86_64=y
BR2_TOOLCHAIN_BUILDROOT=y
BR2_TOOLCHAIN_BUILDROOT_MUSL=y
BR2_TOOLCHAIN_BUILDROOT_LIBC="musl"
BR2_STATIC_LIBS=y
BR2_LINUX_KERNEL=y
BR2_LINUX_KERNEL_LATEST_LTS_VERSION=y
BR2_LINUX_KERNEL_USE_CUSTOM_CONFIG=y
BR2_LINUX_KERNEL_CUSTOM_CONFIG_FILE="$(BR2_EXTERNAL)/configs/faas-minimal-linux.config"
BR2_LINUX_KERNEL_BZIMAGE=y
BR2_TARGET_ROOTFS_CPIO=y
BR2_TARGET_ROOTFS_CPIO_GZIP=y
BR2_PACKAGE_BUSYBOX=y
BR2_ROOTFS_INIT_BUSYBOX=y
BR2_ROOTFS_OVERLAY="$(BR2_EXTERNAL)/rootfs-overlay"
BR2_TARGET_GENERIC_HOSTNAME="faas"
BR2_TARGET_GENERIC_ISSUE=""
BR2_TARGET_GENERIC_ROOT_PASSWD=""
BR2_STRIP_strip=y
BR2_OPTIMIZE_S=y
EOF

cat > configs/faas-minimal-linux.config << 'EOF'
CONFIG_64BIT=y
CONFIG_X86_64=y
CONFIG_SMP=y
CONFIG_NR_CPUS=128
CONFIG_HYPERVISOR_GUEST=y
CONFIG_PARAVIRT=y
CONFIG_KVM_GUEST=y
CONFIG_TTY=y
CONFIG_SERIAL_8250=y
CONFIG_SERIAL_8250_CONSOLE=y
CONFIG_SERIAL_8250_NR_UARTS=4
CONFIG_SERIAL_8250_RUNTIME_UARTS=4
CONFIG_VIRTIO=y
CONFIG_VIRTIO_PCI=y
CONFIG_VIRTIO_PCI_LEGACY=y
CONFIG_VIRTIO_BLK=y
CONFIG_VIRTIO_NET=y
CONFIG_VIRTIO_CONSOLE=y
CONFIG_HW_RANDOM_VIRTIO=y
CONFIG_BLK_DEV=y
CONFIG_BLK_DEV_INITRD=y
CONFIG_TMPFS=y
CONFIG_PROC_FS=y
CONFIG_SYSFS=y
CONFIG_EXT4_FS=y
CONFIG_BINFMT_ELF=y
CONFIG_BINFMT_SCRIPT=y
CONFIG_NET=y
CONFIG_INET=y
CONFIG_IP_PNP=y
CONFIG_IP_PNP_DHCP=y
CONFIG_PACKET=y
CONFIG_UNIX=y
CONFIG_INITRAMFS_SOURCE=""
CONFIG_MEMORY_HOTPLUG=y
CONFIG_MEMORY_HOTREMOVE=y
CONFIG_MODULES=n
CONFIG_DEBUG_KERNEL=n
CONFIG_SWAP=n
CONFIG_SYSVIPC=n
CONFIG_AUDIT=n
CONFIG_SECCOMP=n
CONFIG_INOTIFY_USER=y
EOF

# Create rootfs-overlay/init
mkdir -p rootfs-overlay

cat > rootfs-overlay/init << 'EOF'
#!/bin/sh
set -e

mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev 2>/dev/null || mount -t tmpfs none /dev

mkdir -p /dev/pts
mount -t devpts none /dev/pts 2>/dev/null || true

ip link set lo up 2>/dev/null || true

echo ""
echo "KubeSwift faas-minimal ready"
echo "kernel: $(uname -r)"
echo ""

exec /bin/sh
EOF

chmod +x rootfs-overlay/init
