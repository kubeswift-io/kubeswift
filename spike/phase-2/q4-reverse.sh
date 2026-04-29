#!/bin/bash
# Q4 reverse: v50.2 source on miles, v51.1 dest on boba.
# (Reverse of q4-version.sh which was v51.1 src, v50.2 dst.)
set +e

BOBA=5.9.122.244
PORT=6789
WORK=/tmp/spike-phase2

# Need v50.2 binary on miles (we already have v51.1 there). Push from local.
# Then v51.1 on boba (already there).
CH_M_OLD=/root/cloud-hypervisor-v50.2
CHR_M=/root/ch-remote
CH_B=/root/cloud-hypervisor   # v51.1 on boba (the prod-deployed one... actually we copied v51.1 to /root/cloud-hypervisor on boba earlier)
CHR_B=/root/ch-remote

clean_local() {
  for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
  rm -f $WORK/src.sock $WORK/dst.sock $WORK/recv.log $WORK/sendmig.log $WORK/src-ch.log
}
clean_boba() {
  ssh -o BatchMode=yes root@$BOBA '
    for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
    rm -fv /tmp/spike-phase2/src.sock /tmp/spike-phase2/dst.sock /tmp/spike-phase2/recv.log /tmp/spike-phase2/dst-ch.log
  '
}
clean_local; clean_boba; sleep 1

echo "Source CH version: $($CH_M_OLD --version)"
echo "Dest CH version:   $(ssh root@$BOBA $CH_B --version)"
echo

cat > $WORK/vm.json <<JSON
{
  "cpus": {"boot_vcpus": 1, "max_vcpus": 1},
  "memory": {"size": 268435456},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs-beacon.cpio.gz", "cmdline": "console=hvc0 init=/init reboot=k panic=0"},
  "console": {"mode": "Tty"},
  "serial": {"mode": "Off"}
}
JSON

echo "=== [src miles] start v50.2 source CH and boot guest ==="
nohup $CH_M_OLD -vvv --log-file $WORK/src-ch.log --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S $WORK/src.sock ] && break; sleep 0.5; done
$CHR_M --api-socket $WORK/src.sock create $WORK/vm.json >/dev/null
$CHR_M --api-socket $WORK/src.sock boot >/dev/null
sleep 5
echo "  src created+booted (no info-probe due to ch-remote vs CH version skew)"

echo
echo "=== [dst boba] start v51.1 destination CH and arm receive-migration ==="
ssh -o BatchMode=yes root@$BOBA "
nohup $CH_B -vvv --log-file /tmp/spike-phase2/dst-ch.log --api-socket /tmp/spike-phase2/dst.sock > /tmp/spike-phase2/dst-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S /tmp/spike-phase2/dst.sock ] && break; sleep 0.5; done
$CHR_B --api-socket /tmp/spike-phase2/dst.sock receive-migration tcp:0.0.0.0:$PORT > /tmp/spike-phase2/recv.log 2>&1 &
sleep 2
ss -tlnp 2>/dev/null | grep $PORT && echo '  dst listening on $PORT' || echo '  dst NOT listening on $PORT'
"

echo
echo "=== [src miles] send-migration tcp:$BOBA:$PORT ==="
T_START=$(date +%s.%N)
$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1
EXIT=$?
T_END=$(date +%s.%N)
WALLCLOCK=$(echo "$T_END - $T_START" | bc)
echo "  send-migration exit=$EXIT wallclock=${WALLCLOCK}s"
echo "  send-migration log:"
cat $WORK/sendmig.log | sed 's/^/    /'
sleep 3

echo
echo "=== Logs ==="
echo "  src CH log:"
grep -iE "migrat|version|snapshot|incompat|protocol|error" $WORK/src-ch.log 2>/dev/null | tail -15 | sed 's/^/    /'
echo
echo "  dst CH log:"
ssh root@$BOBA "grep -iE 'migrat|version|snapshot|incompat|protocol|error' /tmp/spike-phase2/dst-ch.log 2>/dev/null | tail -15" | sed 's/^/    /'
echo
echo "  dst recv.log:"
ssh root@$BOBA "cat /tmp/spike-phase2/recv.log 2>/dev/null" | sed 's/^/    /'
echo
echo "=== Final states ==="
echo "  src CH process running on miles? $(ps aux | grep -F cloud-hypervisor-v50.2 | grep -v grep | head -1 | awk '{print $2}' || echo no)"
echo "  dst CH state on boba (via v51.1 ch-remote): $(ssh root@$BOBA "$CHR_B --api-socket /tmp/spike-phase2/dst.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"

clean_local; clean_boba
echo "done"
