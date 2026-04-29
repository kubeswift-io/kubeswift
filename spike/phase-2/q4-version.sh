#!/bin/bash
# Q4: Version mismatch test (one pair only — architect time-cap descope).
#
# Source: miles, CH v51.1 (deployed)
# Destination: boba, CH v50.2 (different minor — pre-built static binary)
#
# Q4a: capture the exact error message on version mismatch.
# Q4c: determine if mismatch is detected at handshake (no transfer) or
#      after partial transfer.
#
# Q4b's full sweep is descoped per user priority + architect time-cap.
set +e

BOBA=5.9.122.244
PORT=6789
WORK=/tmp/spike-phase2

CH_M=/usr/local/bin/cloud-hypervisor
CHR_M=/root/ch-remote
CH_B_OLD=/root/cloud-hypervisor-v50.2   # ← intentionally older
CHR_B=/root/ch-remote                    # use v51.1 ch-remote (compat-with-VMM is what matters)

clean_local() {
  for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
  rm -f $WORK/src.sock $WORK/dst.sock $WORK/recv.log $WORK/sendmig.log $WORK/src-ch.log $WORK/dst-ch.log
}
clean_boba() {
  ssh -o BatchMode=yes root@$BOBA '
    for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
    rm -f /tmp/spike-phase2/src.sock /tmp/spike-phase2/dst.sock /tmp/spike-phase2/recv.log /tmp/spike-phase2/dst-ch.log
  '
}
clean_local; clean_boba; sleep 1

# Confirm versions
echo "Source CH version: $($CH_M --version)"
echo "Dest CH version:   $(ssh root@$BOBA $CH_B_OLD --version)"
echo

# Boot a 256 MiB beacon guest on miles (using existing rootfs-beacon)
cat > $WORK/vm.json <<JSON
{
  "cpus": {"boot_vcpus": 1, "max_vcpus": 1},
  "memory": {"size": 268435456},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs-beacon.cpio.gz", "cmdline": "console=hvc0 init=/init reboot=k panic=0"},
  "console": {"mode": "Tty"},
  "serial": {"mode": "Off"}
}
JSON

echo "=== [src miles] start v51.1 source CH and boot guest ==="
nohup $CH_M -vvv --log-file $WORK/src-ch.log --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S $WORK/src.sock ] && break; sleep 0.5; done
$CHR_M --api-socket $WORK/src.sock create $WORK/vm.json >/dev/null
$CHR_M --api-socket $WORK/src.sock boot >/dev/null
sleep 5
echo "  src state: $(timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["state"])')"

echo
echo "=== [dst boba] start v50.2 destination CH (older) and arm receive-migration ==="
# NOTE: skip ch-remote info probe; v51.1 ch-remote vs v50.2 CH API may
# be incompatible (info call hung in earlier run). Use ss -tlnp to confirm
# the listener is up.
ssh -o BatchMode=yes root@$BOBA "
nohup $CH_B_OLD -vvv --log-file /tmp/spike-phase2/dst-ch.log --api-socket /tmp/spike-phase2/dst.sock > /tmp/spike-phase2/dst-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S /tmp/spike-phase2/dst.sock ] && break; sleep 0.5; done
$CHR_B --api-socket /tmp/spike-phase2/dst.sock receive-migration tcp:0.0.0.0:$PORT > /tmp/spike-phase2/recv.log 2>&1 &
sleep 2
ss -tlnp 2>/dev/null | grep $PORT && echo '  dst listening on $PORT' || echo '  dst NOT listening on $PORT'
"

echo
echo "=== [src miles] send-migration tcp:$BOBA:$PORT (mismatched-version target) ==="
T_START=$(date +%s.%N)
$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1
EXIT=$?
T_END=$(date +%s.%N)
WALLCLOCK=$(echo "$T_END - $T_START" | bc)
echo "  send-migration exit=$EXIT wallclock=${WALLCLOCK}s"
echo "  send-migration log:"
cat $WORK/sendmig.log | sed 's/^/    /'
sleep 2

echo
echo "=== Q4c: handshake vs post-transfer detection ==="
echo "  src CH log migration excerpts:"
grep -iE "migrat|version|snapshot|incompat|protocol" $WORK/src-ch.log 2>/dev/null | tail -20 | sed 's/^/    /'
echo
echo "  dst CH log migration excerpts:"
ssh root@$BOBA "grep -iE 'migrat|version|snapshot|incompat|protocol' /tmp/spike-phase2/dst-ch.log 2>/dev/null | tail -20" | sed 's/^/    /'
echo
echo "  dst recv.log:"
ssh root@$BOBA "cat /tmp/spike-phase2/recv.log 2>/dev/null" | sed 's/^/    /'

echo
echo "=== Final states ==="
echo "  src state: $(timeout 3 $CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)["state"])' 2>/dev/null || echo gone)"
echo "  dst state: $(ssh root@$BOBA "timeout 3 $CHR_B --api-socket /tmp/spike-phase2/dst.sock info 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"state\"])'" 2>/dev/null || echo gone)"

clean_local; clean_boba
echo "done"
