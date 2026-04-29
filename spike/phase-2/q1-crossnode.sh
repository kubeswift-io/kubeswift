#!/bin/bash
# Q1 cross-node: miles (source) -> boba (destination) via TCP.
# Source SSH-orchestrates from this script; running on miles.
#
# Setup expectation: kernel + initramfs present at /tmp/spike-phase2/ on
# both nodes. CH binary at /usr/local/bin/cloud-hypervisor on miles, at
# /root/cloud-hypervisor on boba.
set +e

SRC_HOST=miles
DST_HOST=5.9.122.244          # boba IP (single-NIC public)
DST_INTERNAL_IP=5.9.122.244   # boba IP we'll listen on / send to
WORK=/tmp/spike-phase2
PORT=6789

CH_SRC=/usr/local/bin/cloud-hypervisor
CHR_SRC=/root/ch-remote
CH_DST=/root/cloud-hypervisor
CHR_DST=/root/ch-remote

cleanup_dst() {
  ssh -o BatchMode=yes root@$DST_HOST 'pkill -9 -f cloud-hypervisor; pkill -9 -f ch-remote; rm -f /tmp/spike-phase2/*.sock' 2>&1
}
cleanup_src() {
  pkill -9 -f cloud-hypervisor 2>/dev/null
  pkill -9 -f ch-remote 2>/dev/null
  rm -f $WORK/*.sock
}

cleanup_src
cleanup_dst
sleep 1

cat > $WORK/vm.json <<JSON
{
  "cpus": {"boot_vcpus": 1, "max_vcpus": 1},
  "memory": {"size": 268435456},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs.cpio.gz", "cmdline": "console=hvc0 init=/init reboot=k panic=1"},
  "serial": {"mode": "Off"},
  "console": {"mode": "Off"}
}
JSON

echo "=== [src $SRC_HOST] start VMM and boot guest ==="
$CH_SRC --api-socket $WORK/src.sock > $WORK/src.log 2>&1 &
sleep 2
$CHR_SRC --api-socket $WORK/src.sock create $WORK/vm.json
$CHR_SRC --api-socket $WORK/src.sock boot
sleep 3
$CHR_SRC --api-socket $WORK/src.sock info > $WORK/src-info.json
SRC_STATE=$(python3 -c "import json; print(json.load(open('$WORK/src-info.json'))['state'])")
echo "src state=$SRC_STATE"

echo "=== [dst boba] start empty VMM via ssh ==="
ssh -o BatchMode=yes root@$DST_HOST "
mkdir -p /tmp/spike-phase2 && cd /tmp/spike-phase2
pkill -9 -f cloud-hypervisor 2>/dev/null; sleep 1
$CH_DST --api-socket /tmp/spike-phase2/dst.sock > /tmp/spike-phase2/dst.log 2>&1 &
sleep 2
$CHR_DST --api-socket /tmp/spike-phase2/dst.sock ping
" 2>&1 | tail -5

echo "=== [dst boba] receive-migration tcp:0.0.0.0:$PORT (background) ==="
ssh -o BatchMode=yes root@$DST_HOST "
$CHR_DST --api-socket /tmp/spike-phase2/dst.sock receive-migration tcp:0.0.0.0:$PORT > /tmp/spike-phase2/recv.log 2>&1 &
sleep 1
ss -tlnp 2>/dev/null | grep $PORT || echo '(no listener)'
" 2>&1 | tail -3

echo "=== [src $SRC_HOST] send-migration tcp:$DST_INTERNAL_IP:$PORT ==="
T_START=$(date +%s.%N)
$CHR_SRC --api-socket $WORK/src.sock send-migration tcp:$DST_INTERNAL_IP:$PORT 2>&1 | head -10
T_END=$(date +%s.%N)
echo "send-migration wall time: $(echo "$T_END - $T_START" | bc)s"
sleep 2

echo "=== [src $SRC_HOST] post-migration source state ==="
$CHR_SRC --api-socket $WORK/src.sock info 2>/dev/null > $WORK/src-info-post.json
if [ -s $WORK/src-info-post.json ]; then
  python3 -c "import json; d=json.load(open('$WORK/src-info-post.json')); print('src state=', d.get('state'))"
else
  echo "(src VMM exited)"
fi

echo "=== [dst boba] post-migration destination state ==="
ssh -o BatchMode=yes root@$DST_HOST "
$CHR_DST --api-socket /tmp/spike-phase2/dst.sock info 2>&1 | head -200
echo --- recv log ---
cat /tmp/spike-phase2/recv.log
echo --- dst log tail ---
tail -10 /tmp/spike-phase2/dst.log
" 2>&1

echo "=== Cleanup ==="
cleanup_src
cleanup_dst
echo "done"
