#!/bin/sh
# Throwaway: section 2 — manual --restore on a fresh cloud-hypervisor process.
# Runs entirely INSIDE the spike-restore-4g pod via kubectl exec.
set -eu

POD=spike-restore-4g
NS=default

run_in_pod() {
  kubectl exec "$POD" -n "$NS" -- sh -c "$1"
}

section() { printf '\n========== %s ==========\n' "$1"; }

section "verify mounts and CH binary inside restore pod"
run_in_pod 'cloud-hypervisor --version
ls -lh /spike-snapshot/ /var/lib/kubeswift/disks/root/'

section "spawn cloud-hypervisor with --restore"
# The original VM had its seed.iso at /var/lib/kubeswift/run/default-spike-vm-4g/seed.iso.
# Setup script (entrypoint above) already placed seed.iso at that path.
# The CH config in the snapshot references the same disk paths.
RESTORE_LOG=/tmp/restore.log
run_in_pod "
mkdir -p /tmp/restore
nohup cloud-hypervisor \\
  --api-socket path=/tmp/restore/ch.sock \\
  --restore source_url=file:///spike-snapshot \\
  >$RESTORE_LOG 2>&1 &
echo PID=\$!
sleep 6
ls -la /tmp/restore/
echo '--- last 30 lines of restore log ---'
tail -30 $RESTORE_LOG"

section "vm.info on restored VM (should be Paused)"
run_in_pod 'curl --silent --unix-socket /tmp/restore/ch.sock -X GET http://localhost/api/v1/vm.info | jq "{state, mem: .config.memory.size, cpus: .config.cpus.boot_vcpus}"'

section "RESUME the restored VM"
T0=$(date -u +%s.%N)
run_in_pod 'curl --silent --unix-socket /tmp/restore/ch.sock -X PUT http://localhost/api/v1/vm.resume -w "HTTP=%{http_code}\n"'
T1=$(date -u +%s.%N)
printf 'resume took %.3fs\n' "$(echo "$T1 - $T0" | bc -l)"

section "vm.info AFTER resume (should be Running)"
run_in_pod 'curl --silent --unix-socket /tmp/restore/ch.sock -X GET http://localhost/api/v1/vm.info | jq ".state"'

section "wait 8s for guest to settle, then verify in-guest state survived"
sleep 8
# Need to ssh into the restored VM. Restored CH spawned its own tap. Find PID.
RESTORED_PID=$(run_in_pod 'pgrep -f "cloud-hypervisor.*--restore" | head -1' | tr -d '[:space:]')
echo "restored CH PID: $RESTORED_PID"

section "find restored VM IP (dnsmasq.leases on br0 inside restore pod's netns)"
# CH on restore brings up the same MAC as the original snapshot. The host
# launcher pod is gone; this restore pod has its own network namespace and
# tap0 (created by CH). dnsmasq is NOT running here, so the VM has the
# previous DHCP lease cached only — it'll keep its old IP if the lease wasn't
# expired. Try a known-good IP from the snapshot config.
ORIG_IP=$(run_in_pod 'cat /spike-snapshot/config.json | jq -r ".net[0].mac" 2>/dev/null || echo unknown')
echo "original MAC from snapshot: $ORIG_IP"
run_in_pod '
echo "--- ip a inside the restore pod ---"
ip a 2>/dev/null | head -30 || (apt-get install -qq -y iproute2 2>/dev/null; ip a 2>/dev/null) | head -30
echo "--- arp table — VM may show up ---"
ip neigh show 2>/dev/null || arp -an 2>/dev/null
'

section "scan tap interfaces for the restored VM IP"
run_in_pod '
for IP in 10.244.125.10 10.244.125.11 10.244.125.12 10.244.125.13 10.244.125.14 10.244.125.18 10.244.125.20; do
  if ping -c 1 -W 1 $IP >/dev/null 2>&1; then
    echo "restored VM responds at: $IP"
  fi
done
true'

section "summary"
echo "see /tmp/restore.log inside the pod for full CH output"
