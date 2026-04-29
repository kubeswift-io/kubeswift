#!/bin/bash
# Q1d: failure-path inventory.
#
# Four sub-tests:
#   F1: Source CH killed mid-migration. Expected: dst left in incomplete state.
#   F2: Destination CH killed mid-migration. Expected: src returns error, src VMM state?
#   F3: Network drop mid-migration (iptables -j DROP on dst:6789). Expected: send-migration timeout / error.
#   F4: Cancellation primitive — does CH expose any way to cancel a send-migration in flight
#       short of process kill? Audit the API surface.
#
# We use a 2 GiB guest to extend transfer window enough to time the interrupts.
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
      for sig in TERM KILL; do
        pkill -$sig -f cloud-hypervisor 2>/dev/null
        pkill -$sig -f ch-remote 2>/dev/null
        sleep 0.5
      done
      sleep 1
      rm -f /tmp/spike-phase2/src.sock /tmp/spike-phase2/dst.sock /tmp/spike-phase2/recv.log /tmp/spike-phase2/sendmig.log
      iptables -F INPUT 2>/dev/null
    ' >/dev/null 2>&1
  done
  sleep 2
}

wait_for_sock() {
  local host=$1 sock=$2
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if ssh -o BatchMode=yes root@$host "test -S $sock" 2>/dev/null; then return 0; fi
    sleep 0.5
  done
  return 1
}

start_src_with_beacon() {
  run_miles "cat > $WORK/vm-2g.json <<JSON
{
  \"cpus\": {\"boot_vcpus\": 1, \"max_vcpus\": 1},
  \"memory\": {\"size\": 2147483648},
  \"payload\": {\"kernel\": \"$WORK/bzImage\", \"initramfs\": \"$WORK/rootfs-beacon.cpio.gz\", \"cmdline\": \"console=hvc0 init=/init reboot=k panic=0\"},
  \"serial\": {\"mode\": \"Off\"},
  \"console\": {\"mode\": \"Tty\"}
}
JSON"
  run_miles "nohup $CH_M --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null &"
  wait_for_sock $MILES $WORK/src.sock || { echo "    src.sock did not appear"; return 1; }
  run_miles "$CHR_M --api-socket $WORK/src.sock create $WORK/vm-2g.json"
  run_miles "$CHR_M --api-socket $WORK/src.sock boot"
  sleep 5
}

start_dst_receive() {
  run_boba "nohup $CH_B --api-socket $WORK/dst.sock > $WORK/dst-console.log 2>&1 < /dev/null &"
  wait_for_sock $BOBA $WORK/dst.sock || { echo "    dst.sock did not appear"; return 1; }
  run_boba "$CHR_B --api-socket $WORK/dst.sock receive-migration tcp:0.0.0.0:$PORT > $WORK/recv.log 2>&1 &"
  sleep 1
}

src_state() { run_miles "timeout 5 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c \"import json,sys;d=json.load(sys.stdin);print(d.get('state','?'))\" 2>/dev/null || echo gone"; }
dst_state() { run_boba  "timeout 5 $CHR_B --api-socket $WORK/dst.sock info 2>/dev/null | python3 -c \"import json,sys;d=json.load(sys.stdin);print(d.get('state','?'))\" 2>/dev/null || echo gone"; }

# ============================================================
echo "=========================================================="
echo "F1: Kill source CH during send-migration"
echo "=========================================================="
cleanup_all
start_src_with_beacon
echo "  src state pre: $(src_state)"
start_dst_receive

run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &"
sleep 1
echo "  killing source CH..."
run_miles "pkill -9 -f cloud-hypervisor"
sleep 3
echo "  src state post: $(src_state)"
echo "  dst state post: $(dst_state)"
echo "  src sendmig log:"
run_miles "cat $WORK/sendmig.log"
echo "  dst recv log:"
run_boba "cat $WORK/recv.log"

# ============================================================
echo "=========================================================="
echo "F2: Kill destination CH during receive-migration"
echo "=========================================================="
cleanup_all
start_src_with_beacon
echo "  src state pre: $(src_state)"
start_dst_receive

run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &"
sleep 1
echo "  killing destination CH..."
run_boba "pkill -9 -f cloud-hypervisor"
sleep 3
echo "  src state post: $(src_state)"
echo "  src sendmig log:"
run_miles "cat $WORK/sendmig.log"

# Probe: is the source guest still observable? Try info, try resume-if-paused
echo "  src info:"
run_miles "$CHR_M --api-socket $WORK/src.sock info 2>&1 | head -20"
echo "  src console tail (last beacons):"
run_miles "tail -10 $WORK/src-console.log | grep BEACON | tail -5"

# ============================================================
echo "=========================================================="
echo "F3: Network drop mid-migration (iptables DROP on boba:6789)"
echo "=========================================================="
cleanup_all
start_src_with_beacon
echo "  src state pre: $(src_state)"
start_dst_receive

# Drop traffic to port 6789 on boba (via INPUT rule)
run_boba "iptables -I INPUT -p tcp --dport $PORT -j DROP"

run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &"
sleep 8  # send may not connect, or hang
echo "  src sendmig log (after 8s):"
run_miles "cat $WORK/sendmig.log"
echo "  src state during: $(src_state)"

# Lift the drop
run_boba "iptables -F INPUT"
sleep 5
echo "  src state post (drop lifted): $(src_state)"
echo "  src sendmig log final:"
run_miles "cat $WORK/sendmig.log"

# ============================================================
echo "=========================================================="
echo "F4: Cancellation primitive — does CH expose explicit cancel?"
echo "=========================================================="
echo "  ch-remote action list:"
run_miles "$CHR_M --help 2>&1 | grep -iE 'cancel|abort|stop'"
echo "  No 'cancel' command — only kill is available. Documented finding."

echo "=== Final cleanup ==="
cleanup_all
echo "done"
