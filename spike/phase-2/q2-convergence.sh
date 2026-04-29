#!/bin/bash
# Q2: pre-copy convergence under memory dirtying.
#
# Two dirty rates per architect descope:
#   LOW : ~10% of guest RAM/sec
#   HIGH: ~50% of guest RAM/sec
#
# Hard wall-clock fail cap: 60s. If send-migration doesn't complete in 60s,
# we declare "failed to converge" and abort.
#
# Stop-and-copy window measured via BEACON pause-gap: source emits BEACONs
# every 1s while running, last beacon pre-pause + first beacon post-resume
# bracket the pause window from the guest's perspective.
#
# Runs entirely on miles (uses miles->boba ssh trust).
set +e

BOBA=5.9.122.244
PORT=6789
WORK=/tmp/spike-phase2

CH_M=/usr/local/bin/cloud-hypervisor
CHR_M=/root/ch-remote
CH_B=/root/cloud-hypervisor
CHR_B=/root/ch-remote

GUEST_MEM_MIB=1024  # 1 GiB guest
GUEST_MEM_BYTES=$((GUEST_MEM_MIB * 1024 * 1024))
DIRTY_REGION_BYTES=$((512 * 1024 * 1024))  # dirtier touches 512 MiB region
PAUSE_WALL_CLOCK_CAP=60  # seconds

clean_local() {
  for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
  rm -f $WORK/src.sock $WORK/dst.sock $WORK/recv.log $WORK/sendmig.log
}
clean_boba() {
  ssh -o BatchMode=yes root@$BOBA '
    for i in 1 2 3; do pkill -KILL -f cloud-hypervisor 2>/dev/null; pkill -KILL -f ch-remote 2>/dev/null; sleep 0.3; done
    rm -f /tmp/spike-phase2/src.sock /tmp/spike-phase2/dst.sock /tmp/spike-phase2/recv.log
  '
}
clean_both() { clean_local; clean_boba; sleep 1; }

# Build initramfs with dirtier baked in. RATE label "low" or "high" sets sleep_us.
build_rootfs() {
  local label=$1
  local pages_per_iter=$2
  local sleep_us=$3
  rm -rf /tmp/rootfs-q2
  mkdir /tmp/rootfs-q2
  cd /tmp/rootfs-q2
  zcat $WORK/rootfs.cpio.gz | cpio -idm 2>/dev/null
  mkdir -p /tmp/rootfs-q2/usr/bin
  cp $WORK/dirtier /tmp/rootfs-q2/usr/bin/
  chmod +x /tmp/rootfs-q2/usr/bin/dirtier
  cat > /tmp/rootfs-q2/init <<INIT
#!/bin/sh
mount -t proc none /proc
mount -t sysfs none /sys
mount -t devtmpfs none /dev 2>/dev/null || mount -t tmpfs none /dev
mkdir -p /dev/pts
mount -t devpts none /dev/pts 2>/dev/null || true
ip link set lo up
echo "DIRTIER-START label=$label pages_per_iter=$pages_per_iter sleep_us=$sleep_us"
echo "memtotal=\$(grep MemTotal /proc/meminfo | awk '{print \$2}') KiB"
# Start dirtier in background. Region size $DIRTY_REGION_BYTES.
/usr/bin/dirtier $DIRTY_REGION_BYTES $((pages_per_iter * 4096)) $sleep_us > /dev/console 2>&1 &
sleep 1
echo "DIRTIER-PID=\$!"
COUNTER=0
while true; do
  COUNTER=\$((COUNTER + 1))
  UPTIME=\$(cat /proc/uptime | cut -d' ' -f1)
  echo "BEACON counter=\$COUNTER uptime=\$UPTIME"
  sleep 1
done
INIT
  chmod +x /tmp/rootfs-q2/init
  cd /tmp/rootfs-q2 && find . | cpio -o -H newc 2>/dev/null | gzip > $WORK/rootfs-q2-$label.cpio.gz
  ls -la $WORK/rootfs-q2-$label.cpio.gz
}

# Push the rootfs to boba
push_rootfs_to_boba() {
  local label=$1
  cat $WORK/rootfs-q2-$label.cpio.gz | ssh -o BatchMode=yes root@$BOBA "cat > /tmp/spike-phase2/rootfs-q2-$label.cpio.gz"
  ssh -o BatchMode=yes root@$BOBA "ls -la /tmp/spike-phase2/rootfs-q2-$label.cpio.gz"
}

# Run a single migration test. Args: label, pages_per_iter, sleep_us
run_test() {
  local label=$1
  local pages_per_iter=$2
  local sleep_us=$3

  echo "==================== Q2 test: $label ===================="
  echo "  pages_per_iter=$pages_per_iter, sleep_us=$sleep_us"
  echo "  expected pages/sec ~ $((pages_per_iter * 1000000 / sleep_us))"
  echo "  expected dirty rate ~ $((pages_per_iter * 4096 * 1000000 / sleep_us / 1048576)) MiB/s"

  build_rootfs $label $pages_per_iter $sleep_us
  push_rootfs_to_boba $label

  clean_both

  cat > $WORK/vm-q2.json <<JSON
{
  "cpus": {"boot_vcpus": 2, "max_vcpus": 2},
  "memory": {"size": $GUEST_MEM_BYTES},
  "payload": {"kernel": "$WORK/bzImage", "initramfs": "$WORK/rootfs-q2-$label.cpio.gz", "cmdline": "console=hvc0 init=/init reboot=k panic=0"},
  "console": {"mode": "Tty"},
  "serial": {"mode": "Off"}
}
JSON

  # Start source with verbose logging
  echo "  start source CH..."
  nohup $CH_M -vvv --log-file $WORK/src-ch.log --api-socket $WORK/src.sock > $WORK/src-console.log 2>&1 < /dev/null &
  for i in 1 2 3 4 5 6 7 8 9 10; do [ -S $WORK/src.sock ] && break; sleep 0.5; done
  $CHR_M --api-socket $WORK/src.sock create $WORK/vm-q2.json >/dev/null
  $CHR_M --api-socket $WORK/src.sock boot >/dev/null

  # Wait for the dirtier to be running and establish baseline
  sleep 12
  echo "  src last 3 BEACONs:"
  grep BEACON $WORK/src-console.log | tail -3 | sed 's/^/    /'
  echo "  src last DIRTYRATE report:"
  grep DIRTYRATE $WORK/src-console.log | tail -2 | sed 's/^/    /'

  # Start destination with verbose logging
  ssh -o BatchMode=yes root@$BOBA "
    nohup $CH_B -vvv --log-file /tmp/spike-phase2/dst-ch.log --api-socket /tmp/spike-phase2/dst.sock > /tmp/spike-phase2/dst-console.log 2>&1 < /dev/null &
    for i in 1 2 3 4 5 6 7 8 9 10; do [ -S /tmp/spike-phase2/dst.sock ] && break; sleep 0.5; done
    $CHR_B --api-socket /tmp/spike-phase2/dst.sock receive-migration tcp:0.0.0.0:$PORT > /tmp/spike-phase2/recv.log 2>&1 &
    sleep 1
  "

  # Capture last BEACON pre-migration
  PRE_BEACON=$(grep BEACON $WORK/src-console.log | tail -1)
  echo "  PRE last BEACON: $PRE_BEACON"

  # Issue send-migration with wall-clock cap
  T_START=$(date +%s.%N)
  timeout $PAUSE_WALL_CLOCK_CAP $CHR_M --api-socket $WORK/src.sock send-migration tcp:$BOBA:$PORT > $WORK/sendmig.log 2>&1
  EXIT=$?
  T_END=$(date +%s.%N)
  WALLCLOCK=$(echo "$T_END - $T_START" | bc)
  echo "  send-migration exit=$EXIT wallclock=${WALLCLOCK}s (cap=${PAUSE_WALL_CLOCK_CAP}s)"
  if [ $EXIT -eq 124 ]; then
    echo "  FAILED-TO-CONVERGE: send-migration exceeded $PAUSE_WALL_CLOCK_CAP s wall-clock cap"
    echo "  src state: $($CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["state"])' 2>/dev/null || echo gone)"
  else
    echo "  send-migration log:"
    cat $WORK/sendmig.log | sed 's/^/    /'
  fi
  sleep 5

  # Capture first post-migration BEACON on destination
  echo "  src state final: $($CHR_M --api-socket $WORK/src.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["state"])' 2>/dev/null || echo gone)"
  echo "  dst state final: $(ssh root@$BOBA "$CHR_B --api-socket /tmp/spike-phase2/dst.sock info 2>/dev/null | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d[\"state\"])'" 2>/dev/null || echo gone)"
  POST_BEACON=$(ssh root@$BOBA "grep BEACON /tmp/spike-phase2/dst-console.log 2>/dev/null | head -3")
  echo "  POST first 3 BEACONs (dst):"
  echo "$POST_BEACON" | sed 's/^/    /'

  # Compute stop-and-copy window from BEACON gap
  PRE_UPTIME=$(echo "$PRE_BEACON" | sed -n 's/.*uptime=\([0-9.]*\).*/\1/p')
  POST_UPTIME=$(echo "$POST_BEACON" | head -1 | sed -n 's/.*uptime=\([0-9.]*\).*/\1/p')
  if [ -n "$PRE_UPTIME" ] && [ -n "$POST_UPTIME" ]; then
    STOP_WINDOW=$(echo "$POST_UPTIME - $PRE_UPTIME" | bc)
    echo "  STOP-AND-COPY window (BEACON-gap): pre=${PRE_UPTIME}s post=${POST_UPTIME}s window=${STOP_WINDOW}s"
  fi

  # Migration logs from CH for iteration data
  echo "  src CH log migration excerpts:"
  grep -i "migrat\|pre-copy\|dirty\|iter" $WORK/src-ch.log 2>/dev/null | tail -20 | sed 's/^/    /'
  echo "  dst CH log migration excerpts:"
  ssh root@$BOBA "grep -i 'migrat\|pre-copy\|dirty\|iter' /tmp/spike-phase2/dst-ch.log 2>/dev/null | tail -20" | sed 's/^/    /'

  clean_both
}

# ========================================================================
# Test variants
# ========================================================================
# LOW rate: ~50 MiB/s dirty rate (~5% of 1 GiB/sec)
# 12800 pages * 4 KiB = 50 MiB; 1 iter every 1ms = 50 MiB/s
# Targets:
#   LOW : ~50 MiB/s  ≈ 12,800 pages/sec ≈ 5% of guest RAM/sec
#   HIGH: ~500 MiB/s ≈ 128,000 pages/sec ≈ 50% of guest RAM/sec
#
# To achieve the rate while avoiding CPU-bound dirty (which would be many GiB/s),
# we use sleep_us=10000 (10 ms per iteration → 100 iter/sec). At 100 iter/sec:
#   LOW:  pages_per_iter=128 → 12,800 pages/sec → 50 MiB/s
#   HIGH: pages_per_iter=1280 → 128,000 pages/sec → 500 MiB/s

echo "===== LOW dirty rate (~50 MiB/s) ====="
run_test low 128 10000

echo
echo "===== HIGH dirty rate (~500 MiB/s) ====="
run_test high 1280 10000

echo "done"
