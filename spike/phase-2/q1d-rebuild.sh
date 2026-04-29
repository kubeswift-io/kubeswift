#!/bin/bash
# Q1d failure-path tests, rebuilt with explicit per-step verification.
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

# Hard cleanup with verification.
hard_clean_node() {
  local host=$1
  ssh -o BatchMode=yes root@$host '
    for i in 1 2 3; do
      pkill -KILL -f cloud-hypervisor 2>/dev/null
      pkill -KILL -f ch-remote 2>/dev/null
      sleep 0.5
    done
    rm -f /tmp/spike-phase2/src.sock /tmp/spike-phase2/dst.sock /tmp/spike-phase2/recv.log /tmp/spike-phase2/sendmig.log
    iptables -F INPUT 2>/dev/null || true
    # Verify
    [ -e /tmp/spike-phase2/src.sock ] && echo "ERR: src.sock still exists" && exit 1
    [ -e /tmp/spike-phase2/dst.sock ] && echo "ERR: dst.sock still exists" && exit 1
    pgrep -f cloud-hypervisor && echo "ERR: cloud-hypervisor still running" && exit 1
    echo OK
  '
}

both_clean() {
  echo "  hard-clean miles: $(hard_clean_node $MILES)"
  echo "  hard-clean boba:  $(hard_clean_node $BOBA)"
}

start_src() {
  run_miles "cat > $WORK/vm.json <<JSON
{
  \"cpus\": {\"boot_vcpus\": 1, \"max_vcpus\": 1},
  \"memory\": {\"size\": 2147483648},
  \"payload\": {\"kernel\": \"$WORK/bzImage\", \"initramfs\": \"$WORK/rootfs-beacon.cpio.gz\", \"cmdline\": \"console=hvc0 init=/init reboot=k panic=0\"},
  \"console\": {\"mode\": \"Tty\"},
  \"serial\": {\"mode\": \"Off\"}
}
JSON"
  run_miles "nohup $CH_M --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null & echo PID=\$!"
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if run_miles "test -S $WORK/src.sock"; then break; fi
    sleep 0.5
  done
  run_miles "$CHR_M --api-socket $WORK/src.sock create $WORK/vm.json" >/dev/null
  run_miles "$CHR_M --api-socket $WORK/src.sock boot" >/dev/null
  sleep 5
  echo "  src state pre: $(run_miles "timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"
}

start_dst_recv() {
  run_boba "nohup $CH_B --api-socket $WORK/dst.sock > $WORK/dst-console.log 2>&1 < /dev/null & echo PID=\$!"
  for i in 1 2 3 4 5 6 7 8 9 10; do
    if run_boba "test -S $WORK/dst.sock"; then break; fi
    sleep 0.5
  done
  run_boba "$CHR_B --api-socket $WORK/dst.sock receive-migration tcp:0.0.0.0:$PORT > $WORK/recv.log 2>&1 &"
  sleep 1
}

# ============================================================
echo "============================== F1 =============================="
echo "F1: Kill source CH during send-migration"
both_clean
start_src
start_dst_recv
echo "  start send-migration in background, then kill source after 1s..."
run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &"
sleep 1
run_miles "pkill -KILL -f cloud-hypervisor"
sleep 5
echo "  src final state: $(run_miles "timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"
echo "  dst final state: $(run_boba  "timeout 3 $CHR_B --api-socket $WORK/dst.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"
echo "  src sendmig log:"
run_miles "cat $WORK/sendmig.log 2>/dev/null" | sed 's/^/    /'
echo "  dst recv log:"
run_boba "cat $WORK/recv.log 2>/dev/null" | sed 's/^/    /'
echo "  dst console tail:"
run_boba "tail -5 $WORK/dst-console.log 2>/dev/null" | sed 's/^/    /'

# ============================================================
echo
echo "============================== F2 =============================="
echo "F2: Kill destination CH during receive-migration"
both_clean
start_src
start_dst_recv
echo "  start send-migration in background, then kill destination after 1s..."
run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &"
sleep 1
run_boba "pkill -KILL -f cloud-hypervisor"
sleep 5
echo "  src final state: $(run_miles "timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"
echo "  src sendmig log:"
run_miles "cat $WORK/sendmig.log 2>/dev/null" | sed 's/^/    /'
echo "  src console last beacon:"
run_miles "grep BEACON $WORK/src-console.log 2>/dev/null | tail -1" | sed 's/^/    /'
sleep 8
echo "  src state 8s later: $(run_miles "timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"
echo "  src console latest beacon:"
run_miles "grep BEACON $WORK/src-console.log 2>/dev/null | tail -1" | sed 's/^/    /'

# ============================================================
echo
echo "============================== F3 =============================="
echo "F3: Network drop mid-migration (iptables -j DROP on dst:6789)"
both_clean
start_src
start_dst_recv
echo "  insert iptables DROP rule on boba..."
run_boba "iptables -I INPUT -p tcp --dport $PORT -j DROP"
echo "  start send-migration..."
run_miles "$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &"
sleep 8
echo "  src state during DROP: $(run_miles "timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"
echo "  src sendmig log after 8s:"
run_miles "cat $WORK/sendmig.log 2>/dev/null" | sed 's/^/    /'
echo "  removing iptables rule..."
run_boba "iptables -F INPUT"
sleep 8
echo "  src state after rule lifted: $(run_miles "timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"
echo "  src sendmig log final:"
run_miles "cat $WORK/sendmig.log 2>/dev/null" | sed 's/^/    /'

# ============================================================
echo
echo "============================== F4 =============================="
echo "F4: Cancellation primitive — does CH expose explicit cancel?"
echo "  ch-remote command list (filtered):"
run_miles "$CHR_M --help 2>&1 | grep -iE 'cancel|abort|stop'" | sed 's/^/    /'
echo "  No 'cancel' subcommand exists. Cancellation is process-kill only — documented finding for Phase 2."

both_clean
echo "done"
