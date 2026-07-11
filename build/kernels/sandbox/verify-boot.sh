#!/bin/bash
# KubeSwift sandbox kernel boot verification.
#
# Boots the sandbox bzImage + bridge-initramfs on cloud-hypervisor with a real
# OCI image as the root filesystem, and asserts the bridge mounted it read-only,
# layered a tmpfs overlay, and switch_root'd into the OCI userspace.
#
# Usage: ./verify-boot.sh [bzImage] [rootfs.cpio.gz] [oci-image] [timeout]
# Needs: cloud-hypervisor (v52+), docker (to materialize the OCI rootfs), mkfs.ext4.
#
# Success: exits 0, prints "BOOT VERIFIED". Failure: exits 1.

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BZIMAGE="${1:-${SCRIPT_DIR}/output/images/bzImage}"
INITRAMFS="${2:-${SCRIPT_DIR}/output/images/rootfs.cpio.gz}"
OCI_IMAGE="${3:-alpine:3.20}"
TIMEOUT="${4:-30}"

for f in "$BZIMAGE" "$INITRAMFS"; do
	[ -f "$f" ] || { echo "ERROR: not found: $f (run: make build)"; exit 1; }
done
command -v cloud-hypervisor >/dev/null || { echo "ERROR: cloud-hypervisor not in PATH"; exit 1; }
command -v docker >/dev/null || { echo "ERROR: docker not in PATH (needed to materialize the OCI rootfs)"; exit 1; }

echo "=== KubeSwift sandbox boot verification ==="
echo "bzImage:   $BZIMAGE"
echo "initramfs: $INITRAMFS"
echo "OCI image: $OCI_IMAGE"
echo "CH:        $(cloud-hypervisor --version 2>&1 | head -1)"
echo ""

WORK="$(mktemp -d /tmp/sandbox-verify-XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

# --- materialize the OCI image -> ext4 (unprivileged: docker export + mkfs.ext4 -d) ---
echo "Materializing $OCI_IMAGE -> ext4 ..."
mkdir -p "$WORK/tree"
cid="$(docker create "$OCI_IMAGE" /bin/sh)"
docker export "$cid" | tar -C "$WORK/tree" -xf - 2>/dev/null
docker rm "$cid" >/dev/null
# a verify entrypoint that proves the overlay is writable, the image is intact, then halts
cat > "$WORK/tree/sandbox-verify" <<'EOF'
#!/bin/sh
mount -t proc proc /proc 2>/dev/null
echo "SANDBOX-VERIFY: rootfs=$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release) kernel=$(uname -r)"
echo "SANDBOX-VERIFY: root mount = $(grep ' / ' /proc/mounts | awk '{print $3}')"
echo "test" > /overlay-write-probe && echo "SANDBOX-VERIFY: overlay upper is writable"
echo "SANDBOX-VERIFY: OK"
poweroff -f 2>/dev/null; sleep 3
EOF
chmod +x "$WORK/tree/sandbox-verify"
mkfs.ext4 -q -F -L sandbox-root -d "$WORK/tree" -b 4096 "$WORK/oci.ext4" 128M

# --- boot ---
echo "Booting sandbox kernel + bridge-initramfs + OCI rootfs ..."
timeout "$TIMEOUT" cloud-hypervisor \
	--kernel "$BZIMAGE" \
	--initramfs "$INITRAMFS" \
	--disk path="$WORK/oci.ext4",readonly=on,image_type=raw \
	--cmdline "console=ttyS0 kubeswift.rootfs=block kubeswift.entrypoint=/sandbox-verify printk.time=1 panic=1" \
	--cpus boot=1 --memory size=512M \
	--serial file="$WORK/serial.log" --console off \
	--api-socket path="$WORK/ch.sock" >/dev/null 2>&1 || true

echo ""
echo "=== Boot log ==="
cat "$WORK/serial.log" 2>/dev/null || true
echo ""

if grep -q "sandbox-bridge: OCI rootfs" "$WORK/serial.log" 2>/dev/null \
	&& grep -q "SANDBOX-VERIFY: overlay upper is writable" "$WORK/serial.log" 2>/dev/null \
	&& grep -q "SANDBOX-VERIFY: OK" "$WORK/serial.log" 2>/dev/null; then
	echo "=== BOOT VERIFIED ==="
	echo "sandbox kernel boots an OCI rootfs (RO base + tmpfs overlay) and switch_roots into it"
	exit 0
else
	echo "=== BOOT FAILED ==="
	echo "did not observe the bridge mounting the OCI rootfs + writable overlay within ${TIMEOUT}s"
	exit 1
fi
