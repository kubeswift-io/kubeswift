#!/bin/bash
# Q1e: virtio-net behavior across migration.
#
# We set up tap0 on both miles and boba (same name, same MAC). Boot a guest
# with --net "tap=tap0,mac=02:00:00:00:00:11" on miles. Migrate to boba.
# Observe:
#   - Does receive-migration succeed when a tap of the same name exists on dst?
#   - What does receive-migration do if NO tap exists on dst? (test variant)
#   - Does the migrated guest's MAC match what was set on source?
set +e

BOBA=5.9.122.244
PORT=6789
WORK=/tmp/spike-phase2

CH_M=/usr/local/bin/cloud-hypervisor
CHR_M=/root/ch-remote
CH_B=/root/cloud-hypervisor
CHR_B=/root/ch-remote
GUEST_MAC="02:00:00:00:00:11"

clean_local() {
  for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
  rm -f $WORK/src.sock $WORK/dst.sock $WORK/recv.log $WORK/sendmig.log
  ip link del kstap0 2>/dev/null
}
clean_boba() {
  ssh -o BatchMode=yes root@$BOBA '
    for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
    rm -f /tmp/spike-phase2/src.sock /tmp/spike-phase2/dst.sock /tmp/spike-phase2/recv.log
    ip link del kstap0 2>/dev/null
    iptables -F INPUT 2>/dev/null
  '
}

# Test 1: tap exists on both sides, migration should succeed
echo "============================== T1 =============================="
echo "T1: tap0 exists on both nodes, MAC=$GUEST_MAC, migrate"
clean_local
clean_boba

# Create tap on miles
ip tuntap add dev kstap0 mode tap user root
ip link set kstap0 up
echo "  miles kstap0 created: $(ip link show kstap0 | head -1 | grep -o 'state [A-Z]*')"

# Create tap on boba
ssh -o BatchMode=yes root@$BOBA "
ip tuntap add dev kstap0 mode tap user root
ip link set kstap0 up
ip link show kstap0 | head -1 | grep -o 'state [A-Z]*'
" | sed 's/^/  boba kstap0: /'

# Build VM config with virtio-net attached to kstap0
cat > $WORK/vm-net.json <<JSON
{
  "cpus": {"boot_vcpus": 1, "max_vcpus": 1},
  "memory": {"size": 268435456},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs-beacon.cpio.gz", "cmdline": "console=hvc0 init=/init reboot=k panic=0"},
  "console": {"mode": "Tty"},
  "serial": {"mode": "Off"},
  "net": [{"tap": "kstap0", "mac": "$GUEST_MAC", "id": "_net0"}]
}
JSON

# Start source
nohup $CH_M --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S $WORK/src.sock ] && break; sleep 0.5; done
$CHR_M --api-socket $WORK/src.sock create $WORK/vm-net.json | head -1
$CHR_M --api-socket $WORK/src.sock boot >/dev/null
sleep 5
echo "  src state: $($CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["state"])')"
echo "  src VM net config: $($CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["config"]["net"])')"

# Start destination
ssh -o BatchMode=yes root@$BOBA "
nohup $CH_B --api-socket /tmp/spike-phase2/dst.sock > /tmp/spike-phase2/dst-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S /tmp/spike-phase2/dst.sock ] && break; sleep 0.5; done
$CHR_B --api-socket /tmp/spike-phase2/dst.sock receive-migration tcp:0.0.0.0:$PORT > /tmp/spike-phase2/recv.log 2>&1 &
sleep 1
"

# Send migration
echo "  starting send-migration tcp:$BOBA:$PORT..."
T_START=$(date +%s.%N)
$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1
T_END=$(date +%s.%N)
SENDTIME=$(echo "$T_END - $T_START" | bc)
echo "  send-migration time: ${SENDTIME}s"
echo "  send-migration log:"
cat $WORK/sendmig.log | sed 's/^/    /'
sleep 3

# Check destination state
echo "  dst state: $(ssh root@$BOBA "$CHR_B --api-socket /tmp/spike-phase2/dst.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d[\"state\"])'" 2>/dev/null || echo gone)"
echo "  dst VM net config: $(ssh root@$BOBA "$CHR_B --api-socket /tmp/spike-phase2/dst.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d[\"config\"][\"net\"])'" 2>/dev/null || echo gone)"

# Check tap state on both sides
echo "  miles kstap0 (post): $(ip link show kstap0 2>&1 | head -1)"
echo "  boba kstap0 (post): $(ssh root@$BOBA 'ip link show kstap0 2>&1 | head -1')"

clean_local
clean_boba

# Test 2: tap missing on destination — what happens?
echo
echo "============================== T2 =============================="
echo "T2: tap0 missing on destination — receive-migration should fail"
clean_local
clean_boba

# Create tap only on miles
ip tuntap add dev kstap0 mode tap user root
ip link set kstap0 up

nohup $CH_M --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S $WORK/src.sock ] && break; sleep 0.5; done
$CHR_M --api-socket $WORK/src.sock create $WORK/vm-net.json | head -1
$CHR_M --api-socket $WORK/src.sock boot >/dev/null
sleep 5

# Start destination — no tap created
ssh -o BatchMode=yes root@$BOBA "
nohup $CH_B --api-socket /tmp/spike-phase2/dst.sock > /tmp/spike-phase2/dst-console.log 2>&1 < /dev/null &
for i in 1 2 3 4 5 6 7 8 9 10; do [ -S /tmp/spike-phase2/dst.sock ] && break; sleep 0.5; done
$CHR_B --api-socket /tmp/spike-phase2/dst.sock receive-migration tcp:0.0.0.0:$PORT > /tmp/spike-phase2/recv.log 2>&1 &
sleep 1
ip link show kstap0 2>&1 | head -1 || echo 'no kstap0 on boba (expected for T2)'
"

echo "  starting send-migration tcp:$BOBA:$PORT..."
$CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1
echo "  send-migration log:"
cat $WORK/sendmig.log | sed 's/^/    /'
echo "  dst recv log:"
ssh root@$BOBA cat /tmp/spike-phase2/recv.log | sed 's/^/    /'
echo "  dst state: $(ssh root@$BOBA "timeout 3 $CHR_B --api-socket /tmp/spike-phase2/dst.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d[\"state\"])'" 2>/dev/null || echo gone)"

clean_local
clean_boba
echo "done"
