#!/bin/bash
# Probes CH receive-migration semantics. Run on miles via ssh.
# Output: which URL schemes are accepted, whether receive-migration
# requires a pre-armed CH instance, what error is returned on bad input.
set +e

cleanup() {
  pkill -9 cloud-hypervisor 2>/dev/null
  pkill -9 ch-remote 2>/dev/null
  rm -f /tmp/spike-phase2/*.sock
}
trap cleanup EXIT

WORK=/tmp/spike-phase2
mkdir -p "$WORK"
cd "$WORK"

CH=/usr/local/bin/cloud-hypervisor
CHR=/root/ch-remote

cleanup
sleep 1

echo "=== TEST 1: ch-remote receive-migration without VMM running ==="
$CHR --api-socket /tmp/spike-phase2/no-vmm.sock receive-migration tcp:0.0.0.0:6789 2>&1 | head -5

echo
echo "=== TEST 2: Launch empty VMM, then receive-migration tcp ==="
$CH --api-socket "$WORK/vmm1.sock" >"$WORK/vmm1.log" 2>&1 &
VMM_PID=$!
sleep 2
$CHR --api-socket "$WORK/vmm1.sock" ping 2>&1 | head -3
echo "--- attempting receive-migration tcp:0.0.0.0:6789 ---"
$CHR --api-socket "$WORK/vmm1.sock" receive-migration tcp:0.0.0.0:6789 &
RPID=$!
sleep 2
echo "--- TCP listener status: ---"
ss -tlnp 2>/dev/null | grep 6789 || echo "(no TCP listener on 6789)"
echo "--- VMM log so far: ---"
tail -20 "$WORK/vmm1.log" 2>/dev/null
kill $RPID 2>/dev/null
wait $RPID 2>/dev/null
echo "--- ch-remote receive-migration exit: $? ---"

echo
echo "=== TEST 3: receive-migration unix:/path ==="
$CHR --api-socket "$WORK/vmm1.sock" receive-migration unix:/tmp/spike-phase2/recv.sock &
RPID=$!
sleep 2
ls -la /tmp/spike-phase2/recv.sock 2>&1
kill $RPID 2>/dev/null
wait $RPID 2>/dev/null

echo
echo "=== TEST 4: Boot a tiny VM (kernel boot), then 'send-migration --local' to file ==="
kill $VMM_PID 2>/dev/null
wait $VMM_PID 2>/dev/null
sleep 1

# Fresh VMM with a guest booted
$CH --api-socket "$WORK/vmm-src.sock" >"$WORK/vmm-src.log" 2>&1 &
SRC_PID=$!
sleep 2
$CHR --api-socket "$WORK/vmm-src.sock" create <(cat <<EOF
{
  "cpus": {"boot_vcpus": 1, "max_vcpus": 1},
  "memory": {"size": 268435456},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs.cpio.gz", "cmdline": "console=ttyS0 init=/init reboot=k panic=1"},
  "serial": {"mode": "Off"},
  "console": {"mode": "Off"}
}
EOF
) 2>&1 | head -3
$CHR --api-socket "$WORK/vmm-src.sock" boot 2>&1 | head -3
sleep 3
echo "--- VM info: ---"
$CHR --api-socket "$WORK/vmm-src.sock" info 2>&1 | python3 -c "import json,sys; d=json.load(sys.stdin); print(f\"state={d.get('state')}, mem={d['config']['memory']['size']}, kernel={d['config']['payload']['kernel']}\")" 2>&1 || echo "(info parse failed)"
echo "--- send-migration --local to file: ---"
$CHR --api-socket "$WORK/vmm-src.sock" send-migration --local "unix:/tmp/spike-phase2/local-mig.sock" 2>&1 | head -10

cleanup
