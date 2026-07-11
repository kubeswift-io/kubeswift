#!/bin/sh
# Verify the sandbox kernel config declares the DEPENDENCIES of the drivers it enables.
#
# build/kernels/sandbox uses configs/sandbox-linux.config as the WHOLE kernel config
# (BR2_LINUX_KERNEL_USE_CUSTOM_CONFIG), and buildroot runs `make olddefconfig` over it.
# olddefconfig SILENTLY DROPS an enabled option whose Kconfig dependency is unmet — so
# `CONFIG_VIRTIO_PCI=y` without `CONFIG_PCI=y` yields a kernel with NO virtio-pci at all
# (no /dev/vd*, no eth0 -> "/dev/vda: Can't lookup blockdev" -> panic). That shipped
# once (the hand-written config declared VIRTIO_PCI/VIRTIO_NET but omitted PCI/PCI_MSI
# and NETDEVICES/NET_CORE) and only surfaced on a live cluster, never in CI — the
# kernel isn't built per-PR and the released artifact was spike-built from a good config.
#
# This is a STATIC lint (no kernel build): for each driver the config enables, assert
# its load-bearing dependencies are enabled too. Run in CI: `make verify-sandbox-config`.
set -eu

CONFIG="${1:-$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/configs/sandbox-linux.config}"

if [ ! -f "$CONFIG" ]; then
	echo "verify-config: config not found: $CONFIG" >&2
	exit 2
fi

fail=0
enabled() { grep -q "^$1=y" "$CONFIG"; }

# require DRIVER DEP...: if DRIVER is =y, each DEP must be =y too.
require() {
	driver=$1
	shift
	enabled "$driver" || return 0
	for dep in "$@"; do
		if ! enabled "$dep"; then
			echo "verify-config: ERROR: $driver=y requires $dep=y (missing)" >&2
			fail=1
		fi
	done
}

require CONFIG_VIRTIO_PCI       CONFIG_PCI
require CONFIG_PCI_MSI          CONFIG_PCI
require CONFIG_VIRTIO_NET       CONFIG_NET CONFIG_NETDEVICES CONFIG_NET_CORE
require CONFIG_VIRTIO_BLK       CONFIG_BLK_DEV
require CONFIG_VIRTIO_CONSOLE   CONFIG_VIRTIO CONFIG_TTY
require CONFIG_VIRTIO_FS        CONFIG_VIRTIO CONFIG_FUSE_FS
require CONFIG_VIRTIO_VSOCKETS  CONFIG_VIRTIO CONFIG_VSOCKETS
require CONFIG_HW_RANDOM_VIRTIO CONFIG_VIRTIO

# The load-bearing invariant: virtio-blk (the sandbox rootfs disk) is useless without a
# virtio TRANSPORT. This is exactly the case that shipped broken.
if enabled CONFIG_VIRTIO_BLK && ! enabled CONFIG_VIRTIO_PCI && ! enabled CONFIG_VIRTIO_MMIO; then
	echo "verify-config: ERROR: CONFIG_VIRTIO_BLK=y but no virtio transport (need CONFIG_VIRTIO_PCI or CONFIG_VIRTIO_MMIO)" >&2
	fail=1
fi

if [ "$fail" -ne 0 ]; then
	echo "verify-config: FAILED — the sandbox kernel would drop drivers at olddefconfig" >&2
	exit 1
fi
echo "verify-config: OK ($CONFIG)"
