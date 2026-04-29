#!/bin/bash
# Q1 cross-node: orchestrated from local. Uses local SSH keys to both
# miles and boba (avoids inter-node SSH trust setup).
set +e

MILES=138.201.122.234
BOBA=5.9.122.244
PORT=6789
WORK=/tmp/spike-phase2

CH_M=/usr/local/bin/cloud-hypervisor
CHR_M=/root/ch-remote
CH_B=/root/cloud-hypervisor
CHR_B=/root/ch-remote

run_miles() { ssh -o BatchMode=yes root@$MILES "$@"; }
run_boba()  { ssh -o BatchMode=yes root@$BOBA  "$@"; }

cleanup_all() {
  run_miles 'pkill -9 -f cloud-hypervisor 2>/dev/null; pkill -9 -f ch-remote 2>/dev/null; rm -f /tmp/spike-phase2/*.sock' >/dev/null 2>&1
  run_boba  'pkill -9 -f cloud-hypervisor 2>/dev/null; pkill -9 -f ch-remote 2>/dev/null; rm -f /tmp/spike-phase2/*.sock' >/dev/null 2>&1
}
cleanup_all
sleep 1

echo "=== [src miles] write VM config ==="
run_miles "cat > $WORK/vm.json <<JSON
{
  \"cpus\": {\"boot_vcpus\": 1, \"max_vcpus\": 1},
  \"memory\": {\"size\": 268435456},
  \"payload\": {\"kernel\": \"$WORK/bzImage\", \"initramfs\": \"$WORK/rootfs.cpio.gz\", \"cmdline\": \"console=hvc0 init=/init reboot=k panic=0\"},
  \"serial\": {\"mode\": \"Off\"},
  \"console\": {\"mode\": \"Pty\"}
}
JSON"

echo "=== [src miles] start VMM and boot guest ==="
run_miles "$CH_M --api-socket $WORK/src.sock > $WORK/src.log 2>&1 &"
sleep 2
run_miles "$CHR_M --api-socket $WORK/src.sock create $WORK/vm.json"
run_miles "$CHR_M --api-socket $WORK/src.sock boot"
sleep 3
SRC_STATE=$(run_miles "$CHR_M --api-socket $WORK/src.sock info | python3 -c \"import json,sys;print(json.load(sys.stdin)['state'])\"")
echo "src state=$SRC_STATE"

echo "=== [dst boba] start empty VMM ==="
run_boba "$CH_B --api-socket $WORK/dst.sock > $WORK/dst.log 2>&1 &"
sleep 2
run_boba "$CHR_B --api-socket $WORK/dst.sock ping"

echo "=== [dst boba] receive-migration tcp:0.0.0.0:$PORT (background) ==="
run_boba "$CHR_B --api-socket $WORK/dst.sock receive-migration tcp:0.0.0.0:$PORT > $WORK/recv.log 2>&1 &"
sleep 1
run_boba "ss -tlnp 2>/dev/null | grep $PORT"

echo "=== [src miles] send-migration tcp:$BOBA:$PORT ==="
T_START=$(date +%s.%N)
run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT 2>&1 | head -10"
T_END=$(date +%s.%N)
echo "send-migration wall time: $(echo "$T_END - $T_START" | bc)s"
sleep 2

echo "=== [src miles] post-migration source state ==="
run_miles "$CHR_M --api-socket $WORK/src.sock info 2>/dev/null > $WORK/src-info-post.json; if [ -s $WORK/src-info-post.json ]; then python3 -c \"import json; d=json.load(open('$WORK/src-info-post.json')); print('src state=', d.get('state'))\"; else echo '(src VMM exited)'; fi"

echo "=== [dst boba] post-migration destination state ==="
run_boba "$CHR_B --api-socket $WORK/dst.sock info 2>&1 | python3 -c \"import json,sys;d=json.load(sys.stdin);print('dst state=', d.get('state'),'mem=',d.get('config',{}).get('memory',{}).get('size'))\""

echo "=== [dst boba] receive-migration log ==="
run_boba "cat $WORK/recv.log"

echo "=== [src miles] src log tail ==="
run_miles "tail -20 $WORK/src.log"

echo "=== [dst boba] dst log tail ==="
run_boba "tail -20 $WORK/dst.log"

echo "=== Cleanup ==="
cleanup_all
echo "done"
