#!/bin/sh
# Verify each guest kernel defconfig (sandbox and faas-minimal) pins its Linux
# version, with a checked tarball hash, and that the version is not a known-bad
# one.
#
# Buildroot's BR2_LINUX_KERNEL_LATEST_LTS_VERSION is not "latest": it is the
# version the vendored Buildroot release shipped with, frozen. In Buildroot
# 2024.02.6 that is 6.6.44, whose virtio-net receive path rejects valid GSO
# packets ("eth0: bad gso", rx_frame_errors) whenever skb->len % gso_size is
# small, stalling TCP into every guest by one retransmission timeout per drop
# (#766). Nothing reviewed that version; it came with the Buildroot bump. So
# the version is pinned in each defconfig, its hash is in that tree's
# patches/linux, and a known-bad one is refused here. Static: no kernel build.
# Run in CI from `make verify-sandbox-config`.
set -eu

DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
TREES="$DIR $(dirname "$DIR")/faas-minimal"

# 6.6.44 and 6.6.45 carry "net: drop bad gso csum_start and offset in
# virtio_net_hdr" without "net: tighten bad gso csum offset check in
# virtio_net_hdr" (6.6.46).
BAD_VERSIONS="6.6.44 6.6.45"

fail=0
for defconfig in $(for t in $TREES; do ls "$t"/configs/*_defconfig; done); do
	name=$(basename "$defconfig")
	HASHES="$(dirname "$(dirname "$defconfig")")/patches/linux/linux.hash"
	if grep -qE '^BR2_LINUX_KERNEL_LATEST(_LTS)?_VERSION=y' "$defconfig"; then
		echo "verify-kernel-version: ERROR: $name uses Buildroot's frozen latest kernel; pin BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE" >&2
		fail=1
		continue
	fi
	version=$(sed -n 's/^BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE="\(.*\)"$/\1/p' "$defconfig")
	if [ -z "$version" ] || ! grep -q '^BR2_LINUX_KERNEL_CUSTOM_VERSION=y' "$defconfig"; then
		echo "verify-kernel-version: ERROR: $name does not pin BR2_LINUX_KERNEL_CUSTOM_VERSION_VALUE" >&2
		fail=1
		continue
	fi
	for bad in $BAD_VERSIONS; do
		if [ "$version" = "$bad" ]; then
			echo "verify-kernel-version: ERROR: $name pins Linux $version, which drops valid GSO packets in virtio-net (#766)" >&2
			fail=1
		fi
	done
	if ! grep -qE "^sha256 +[0-9a-f]{64} +linux-$version\.tar\.xz$" "$HASHES" 2>/dev/null; then
		echo "verify-kernel-version: ERROR: no sha256 for linux-$version.tar.xz in $HASHES" >&2
		fail=1
	fi
	if ! grep -q '^BR2_DOWNLOAD_FORCE_CHECK_HASHES=y' "$defconfig"; then
		echo "verify-kernel-version: ERROR: $name does not set BR2_DOWNLOAD_FORCE_CHECK_HASHES=y" >&2
		fail=1
	fi
	[ "$fail" -eq 0 ] && echo "verify-kernel-version: $name pins Linux $version (hash checked)"
done
exit "$fail"
