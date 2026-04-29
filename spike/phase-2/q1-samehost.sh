#!/bin/bash
# Q1 baseline: same-host migration on miles. Validates the wire protocol
# without cross-node networking concerns. Source VMM boots a kernel-only
# guest, destination VMM is launched empty and asked to receive the
# migration over a unix socket.
set +e

cleanup() {
  pkill -9 -f cloud-hypervisor
  pkill -9 -f ch-remote
  rm -f /tmp/spike-phase2/*.sock
}
cleanup
sleep 1

WORK=/tmp/spike-phase2
CH=/usr/local/bin/cloud-hypervisor
CHR=/root/ch-remote

mkdir -p $WORK
cat > $WORK/vm.json <<JSON
{
  "cpus": {"boot_vcpus": 1, "max_vcpus": 1},
  "memory": {"size": 268435456},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs.cpio.gz", "cmdline": "console=hvc0 init=/init reboot=k panic=1"},
  "serial": {"mode": "Off"},
  "console": {"mode": "Off"}
}
JSON

echo "=== Source VMM start ==="
$CH --api-socket $WORK/src.sock > $WORK/src.log 2>&1 &
sleep 2
$CHR --api-socket $WORK/src.sock create $WORK/vm.json
$CHR --api-socket $WORK/src.sock boot
sleep 3
echo "=== Source state ==="
$CHR --api-socket $WORK/src.sock info > $WORK/src-info.json
python3 -c "import json; d=json.load(open('$WORK/src-info.json')); print('state=', d['state'])"

echo "=== Destination VMM start ==="
$CH --api-socket $WORK/dst.sock > $WORK/dst.log 2>&1 &
sleep 2

echo "=== Destination receive-migration unix:/recv.sock (background) ==="
$CHR --api-socket $WORK/dst.sock receive-migration unix:/tmp/spike-phase2/recv.sock > $WORK/recv.log 2>&1 &
RPID=$!
sleep 1
ls -la /tmp/spike-phase2/recv.sock

echo "=== Source send-migration unix:/recv.sock ==="
T_START=$(date +%s.%N)
$CHR --api-socket $WORK/src.sock send-migration unix:/tmp/spike-phase2/recv.sock 2>&1 | head -10
T_END=$(date +%s.%N)
echo "send-migration wall time: $(echo "$T_END - $T_START" | bc)s"

# Wait for receiver to finish
wait $RPID 2>/dev/null
echo "=== Receiver log ==="
cat $WORK/recv.log

echo "=== Source post-migration info ==="
$CHR --api-socket $WORK/src.sock info 2>/dev/null > $WORK/src-info-post.json
python3 -c "import json; d=json.load(open('$WORK/src-info-post.json')); print('src state=', d.get('state'))" 2>&1 || echo "(src VMM gone)"

echo "=== Destination post-migration info ==="
$CHR --api-socket $WORK/dst.sock info 2>/dev/null > $WORK/dst-info-post.json
python3 -c "import json; d=json.load(open('$WORK/dst-info-post.json')); print('dst state=', d.get('state'))" 2>&1

echo "=== Destination resume ==="
$CHR --api-socket $WORK/dst.sock resume 2>&1 | head -3
sleep 1
$CHR --api-socket $WORK/dst.sock info > $WORK/dst-info-resumed.json 2>/dev/null
python3 -c "import json; d=json.load(open('$WORK/dst-info-resumed.json')); print('dst state after resume=', d.get('state'))"

echo "=== Source log tail ==="
tail -10 $WORK/src.log
echo "=== Dest log tail ==="
tail -10 $WORK/dst.log

cleanup
