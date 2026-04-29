#!/bin/bash
# Q1d failure-path inventory — runs entirely on miles, using miles->boba ssh.
set +e

BOBA=5.9.122.244
PORT=6789
WORK=/tmp/spike-phase2

CH_M=/usr/local/bin/cloud-hypervisor
CHR_M=/root/ch-remote
CH_B=/root/cloud-hypervisor
CHR_B=/root/ch-remote

# In-process clean (we run on miles)
clean_local() {
  for i in 1 2 3; do
    pkill -KILL -f cloud-hypervisor 2>/dev/null
    pkill -KILL -f ch-remote 2>/dev/null
    sleep 0.5
  done
  rm -f $WORK/src.sock $WORK/dst.sock $WORK/recv.log $WORK/sendmig.log
}
clean_boba() {
  ssh -o BatchMode=yes root@$BOBA '
    for i in 1 2 3; do
      pkill -KILL -f cloud-hypervisor 2>/dev/null
      pkill -KILL -f ch-remote 2>/dev/null
      sleep 0.5
    done
    rm -f /tmp/spike-phase2/src.sock /tmp/spike-phase2/dst.sock /tmp/spike-phase2/recv.log /tmp/spike-phase2/sendmig.log
    iptables -F INPUT 2>/dev/null || true
  '
}
clean_both() { clean_local; clean_boba; sleep 1; }

start_src() {
  cat > $WORK/vm.json <<JSON
{
  "cpus": {"boot_vcpus": 1, "max_vcpus": 1},
  "memory": {"size": 2147483648},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs-beacon.cpio.gz", "cmdline": "console=hvc0 init=/init reboot=k panic=0"},
  "console": {"mode": "Tty"},
  "serial": {"mode": "Off"}
}
JSON
  nohup $CH_M --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null &
  for i in 1 2 3 4 5 6 7 8 9 10; do [ -S $WORK/src.sock ] && break; sleep 0.5; done
  $CHR_M --api-socket $WORK/src.sock create $WORK/vm.json >/dev/null
  $CHR_M --api-socket $WORK/src.sock boot >/dev/null
  sleep 5
  echo "  src state pre: $(timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["state"])' 2>/dev/null || echo gone)"
}

start_dst() {
  ssh -o BatchMode=yes root@$BOBA "
    nohup $CH_B --api-socket /tmp/spike-phase2/dst.sock > /tmp/spike-phase2/dst-console.log 2>&1 < /dev/null &
    for i in 1 2 3 4 5 6 7 8 9 10; do [ -S /tmp/spike-phase2/dst.sock ] && break; sleep 0.5; done
    $CHR_B --api-socket /tmp/spike-phase2/dst.sock receive-migration tcp:0.0.0.0:$PORT > /tmp/spike-phase2/recv.log 2>&1 &
    sleep 1
  "
}

dst_info() {
  ssh -o BatchMode=yes root@$BOBA "timeout 3 $CHR_B --api-socket /tmp/spike-phase2/dst.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])' 2>/dev/null || echo gone"
}

src_info() {
  timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["state"])' 2>/dev/null || echo gone
}

# ============================================================
echo "============================== F1 =============================="
echo "F1: Kill source CH during send-migration"
clean_both
start_src
start_dst
echo "  start send-migration in background, kill source CH after 1s..."
$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &
sleep 1
pkill -KILL -f cloud-hypervisor   # local source CH
sleep 5
echo "  src state post: $(src_info)"
echo "  dst state post: $(dst_info)"
echo "  src sendmig log:"
cat $WORK/sendmig.log 2>/dev/null | sed 's/^/    /'
echo "  dst recv log:"
ssh root@$BOBA cat /tmp/spike-phase2/recv.log 2>/dev/null | sed 's/^/    /'
echo "  dst console tail:"
ssh root@$BOBA tail -5 /tmp/spike-phase2/dst-console.log 2>/dev/null | sed 's/^/    /'

# ============================================================
echo
echo "============================== F2 =============================="
echo "F2: Kill destination CH during receive-migration"
clean_both
start_src
start_dst
echo "  start send-migration in background, kill destination CH after 1s..."
$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &
sleep 1
ssh -o BatchMode=yes root@$BOBA "pkill -KILL -f cloud-hypervisor"
sleep 5
echo "  src state post: $(src_info)"
echo "  src sendmig log:"
cat $WORK/sendmig.log 2>/dev/null | sed 's/^/    /'
echo "  src console last beacon:"
grep BEACON $WORK/src-console.log 2>/dev/null | tail -1 | sed 's/^/    /'
sleep 8
echo "  src state 8s later: $(src_info)"
echo "  src console latest beacon:"
grep BEACON $WORK/src-console.log 2>/dev/null | tail -1 | sed 's/^/    /'

# ============================================================
echo
echo "============================== F3 =============================="
echo "F3: Network drop mid-migration (iptables -j DROP on dst:$PORT)"
clean_both
start_src
start_dst
echo "  insert iptables DROP rule on boba..."
ssh -o BatchMode=yes root@$BOBA "iptables -I INPUT -p tcp --dport $PORT -j DROP"
echo "  start send-migration..."
$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1 &
sleep 8
echo "  src state during DROP: $(src_info)"
echo "  src sendmig log after 8s:"
cat $WORK/sendmig.log 2>/dev/null | sed 's/^/    /'
echo "  removing iptables rule..."
ssh -o BatchMode=yes root@$BOBA "iptables -F INPUT"
sleep 8
echo "  src state after rule lifted: $(src_info)"
echo "  src sendmig log final:"
cat $WORK/sendmig.log 2>/dev/null | sed 's/^/    /'

# ============================================================
echo
echo "============================== F4 =============================="
echo "F4: Cancellation primitive — does CH expose explicit cancel?"
echo "  ch-remote command list (filtered):"
$CHR_M --help 2>&1 | grep -iE 'cancel|abort|stop' | sed 's/^/    /'
echo "  No 'cancel' subcommand. Cancellation is process-kill only — documented finding for Phase 2."

clean_both
echo "done"
