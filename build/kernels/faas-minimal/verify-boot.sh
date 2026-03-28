#!/bin/bash
# KubeSwift faas-minimal boot verification
# Run on a node with cloud-hypervisor v51.1 in PATH
# Usage: ./verify-boot.sh [path/to/bzImage] [path/to/initramfs.cpio.gz]
#
# Success: script exits 0, prints "BOOT VERIFIED"
# Failure: script exits 1, prints what was missing

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BZIMAGE="${1:-${SCRIPT_DIR}/output/images/bzImage}"
INITRAMFS="${2:-${SCRIPT_DIR}/output/images/rootfs.cpio.gz}"
TIMEOUT="${3:-30}"

echo "=== KubeSwift faas-minimal boot verification ==="
echo "bzImage:    $BZIMAGE"
echo "initramfs:  $INITRAMFS"
echo "timeout:    ${TIMEOUT}s"
echo ""

# Pre-checks
if [ ! -f "$BZIMAGE" ]; then
    echo "ERROR: bzImage not found at $BZIMAGE"
    echo "Run: make build"
    exit 1
fi

if [ ! -f "$INITRAMFS" ]; then
    echo "ERROR: initramfs not found at $INITRAMFS"
    echo "Run: make build"
    exit 1
fi

if ! command -v cloud-hypervisor &>/dev/null; then
    echo "ERROR: cloud-hypervisor not in PATH"
    exit 1
fi

CH_VERSION=$(cloud-hypervisor --version 2>&1 | head -1)
echo "cloud-hypervisor: $CH_VERSION"
echo ""

# Run cloud-hypervisor, capture serial output, look for shell prompt
TMPLOG=$(mktemp /tmp/ch-boot-XXXXXX.log)

echo "Starting cloud-hypervisor..."
timeout "$TIMEOUT" cloud-hypervisor \
    --kernel "$BZIMAGE" \
    --initramfs "$INITRAMFS" \
    --cmdline "console=ttyS0 root=/dev/ram0 rdinit=/init" \
    --memory size=256M \
    --cpus boot=1 \
    --serial tty \
    --console off \
    2>&1 | tee "$TMPLOG" &

CH_PID=$!

# Wait for shell prompt or "faas-minimal ready" in output
WAITED=0
SUCCESS=false
while [ "$WAITED" -lt "$TIMEOUT" ]; do
    if grep -q "faas-minimal ready\|# $\|sh-" "$TMPLOG" 2>/dev/null; then
        SUCCESS=true
        break
    fi
    sleep 1
    WAITED=$((WAITED + 1))
done

# Clean up
kill "$CH_PID" 2>/dev/null || true
wait "$CH_PID" 2>/dev/null || true

echo ""
echo "=== Boot log ==="
cat "$TMPLOG"
rm -f "$TMPLOG"
echo ""

if [ "$SUCCESS" = true ]; then
    echo "=== BOOT VERIFIED ==="
    echo "faas-minimal boots successfully on cloud-hypervisor"
    exit 0
else
    echo "=== BOOT FAILED ==="
    echo "Did not see shell prompt within ${TIMEOUT}s"
    echo "Check boot log above for errors"
    exit 1
fi
```

---

## Directory layout when done
```
build/kernels/faas-minimal/
├── Config.in
├── external.mk
├── Makefile
├── verify-boot.sh
├── configs/
│   ├── faas_minimal_defconfig
│   ├── faas-minimal-linux.config
│   └── faas-minimal-busybox.config
└── rootfs-overlay/
    └── init                  ← must be chmod +x