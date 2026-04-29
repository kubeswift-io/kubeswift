#!/bin/bash
# Q1 cross-node with sentinel via console capture.
#
# Strategy: serial=Tty mode redirects guest console to a file. We boot
# the source guest with an init that prints "BEACON <uptime> <counter>"
# every 1s. We capture src console to a log file and dst console to a
# different log file. After migration, the destination's log should
# continue the BEACON sequence with monotonic uptime/counter — proving
# that the guest's running process state survived migration.
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
  for host in $MILES $BOBA; do
    ssh -o BatchMode=yes root@$host '
      pkill -9 -f cloud-hypervisor 2>/dev/null
      pkill -9 -f ch-remote 2>/dev/null
      sleep 1
      rm -f /tmp/spike-phase2/*.sock /tmp/spike-phase2/*.log
      ls /tmp/spike-phase2/*.sock 2>/dev/null && echo "SOCKETS LEFT on '$host'" || true
    ' >/dev/null 2>&1
  done
}
cleanup_all
sleep 2

# Build a custom initramfs on miles that prints BEACON every 1s.
# We re-pack faas-minimal's rootfs with a modified /init.
echo "=== [src miles] re-pack initramfs with BEACON loop ==="
run_miles "
cd /tmp && rm -rf rootfs-beacon && mkdir rootfs-beacon
cd rootfs-beacon && zcat /tmp/spike-phase2/rootfs.cpio.gz | cpio -idm 2>/dev/null
# Replace init with a beacon-emitting version
cat > /tmp/rootfs-beacon/init <<'INIT'
#!/bin/sh
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev 2>/dev/null || mount -t tmpfs none /dev
mkdir -p /dev/pts
mount -t devpts none /dev/pts 2>/dev/null || true
ip link set lo up
echo \"=== BEACON-INIT start \$(cat /proc/uptime | cut -d' ' -f1) ===\"
COUNTER=0
while true; do
  COUNTER=\$((COUNTER+1))
  UPTIME=\$(cat /proc/uptime | cut -d' ' -f1)
  echo \"BEACON counter=\$COUNTER uptime=\$UPTIME\"
  sleep 1
done
INIT
chmod +x /tmp/rootfs-beacon/init
cd /tmp/rootfs-beacon && find . | cpio -o -H newc 2>/dev/null | gzip > /tmp/spike-phase2/rootfs-beacon.cpio.gz
ls -la /tmp/spike-phase2/rootfs-beacon.cpio.gz
"

# Push the custom initramfs to boba
run_miles "cat /tmp/spike-phase2/rootfs-beacon.cpio.gz" | run_boba "cat > /tmp/spike-phase2/rootfs-beacon.cpio.gz"
run_boba "ls -la /tmp/spike-phase2/rootfs-beacon.cpio.gz"

# Note: serial.mode=Tty redirects to CH process's stdout. We start CH
# with stdout > log so the BEACON output ends up in the log.
echo "=== [src miles] write VM config (serial=Tty, beacon initramfs) ==="
run_miles "cat > $WORK/vm.json <<JSON
{
  \"cpus\": {\"boot_vcpus\": 1, \"max_vcpus\": 1},
  \"memory\": {\"size\": 268435456},
  \"payload\": {\"kernel\": \"$WORK/bzImage\", \"initramfs\": \"$WORK/rootfs-beacon.cpio.gz\", \"cmdline\": \"console=hvc0 init=/init reboot=k panic=0\"},
  \"serial\": {\"mode\": \"Off\"},
  \"console\": {\"mode\": \"Tty\"}
}
JSON"

echo "=== [src miles] start VMM with stdout > src-console.log ==="
run_miles "$CH_M --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 &"
sleep 2
run_miles "$CHR_M --api-socket $WORK/src.sock create $WORK/vm.json"
run_miles "$CHR_M --api-socket $WORK/src.sock boot"
sleep 5
echo "--- src-console.log so far: ---"
run_miles "tail -5 $WORK/src-console.log"

echo "=== [dst boba] start empty VMM with stdout > dst-console.log ==="
run_boba "$CH_B --api-socket $WORK/dst.sock > $WORK/dst-console.log 2>&1 &"
sleep 2
run_boba "$CHR_B --api-socket $WORK/dst.sock ping"

echo "=== [dst boba] receive-migration tcp:0.0.0.0:$PORT (background) ==="
run_boba "$CHR_B --api-socket $WORK/dst.sock receive-migration tcp:0.0.0.0:$PORT > $WORK/recv.log 2>&1 &"
sleep 1

# Grab the last beacon line from source BEFORE migration
SRC_PRE=$(run_miles "tail -1 $WORK/src-console.log")
echo "src last line PRE: $SRC_PRE"

echo "=== [src miles] send-migration tcp:$BOBA:$PORT ==="
T_START=$(date +%s.%N)
run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT 2>&1 | head -5"
T_END=$(date +%s.%N)
echo "send-migration wall time: $(echo "$T_END - $T_START" | bc)s"
sleep 5

echo "=== [src miles] src state post ==="
run_miles "$CHR_M --api-socket $WORK/src.sock info 2>/dev/null > $WORK/src-info-post.json; if [ -s $WORK/src-info-post.json ]; then python3 -c \"import json; d=json.load(open('$WORK/src-info-post.json')); print('src state=', d.get('state'))\"; else echo '(src VMM exited)'; fi"

echo "=== [dst boba] dst state post ==="
run_boba "$CHR_B --api-socket $WORK/dst.sock info 2>&1 | python3 -c \"import json,sys;d=json.load(sys.stdin);print('dst state=', d.get('state'),'mem=',d.get('config',{}).get('memory',{}).get('size'))\""

# Capture dst console output post-migration (give it time to print a few BEACONs)
echo "=== [dst boba] dst console tail (post-migration BEACONS) ==="
run_boba "tail -10 $WORK/dst-console.log"

echo "=== [src miles] src console tail (last beacons before stop) ==="
run_miles "tail -5 $WORK/src-console.log"

# Validate that the destination produced beacon lines AFTER the source's last
echo "=== Sentinel check: dst counter > src counter? ==="
SRC_LAST=$(run_miles "grep BEACON $WORK/src-console.log | tail -1 | sed 's/.*counter=\\([0-9]*\\).*/\\1/'")
DST_FIRST_AFTER_BLANK=$(run_boba "tail -20 $WORK/dst-console.log | grep BEACON | head -1 | sed 's/.*counter=\\([0-9]*\\).*/\\1/'")
DST_LAST=$(run_boba "grep BEACON $WORK/dst-console.log | tail -1 | sed 's/.*counter=\\([0-9]*\\).*/\\1/'")
echo "src last counter: $SRC_LAST"
echo "dst first counter (post-migration): $DST_FIRST_AFTER_BLANK"
echo "dst last counter: $DST_LAST"
if [ -n "$SRC_LAST" ] && [ -n "$DST_LAST" ] && [ "$DST_LAST" -gt "$SRC_LAST" ]; then
  echo "RESULT: PASS — dst continued the beacon sequence ($SRC_LAST → $DST_LAST)"
else
  echo "RESULT: FAIL — beacon sequence did not continue"
fi

echo "=== Cleanup ==="
cleanup_all
echo "done"
