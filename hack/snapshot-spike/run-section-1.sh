#!/bin/sh
# Throwaway: section 1 of the snapshot spike — pause/snapshot/resume on a
# running spike VM, with verbatim output of every step.
#
# CRITICAL: the cloud-hypervisor process runs inside the launcher container,
# so the snapshot destination URL is interpreted relative to that container's
# filesystem. The runtime emptyDir is mounted at /var/lib/kubeswift/run inside
# the launcher AND visible from the debug pod via /var/lib/kubelet/pods/<uid>/
# volumes/kubernetes.io~empty-dir/run/. We snapshot into a sub-dir of that
# emptyDir, then copy out to a stable hostPath so it survives launcher-pod
# deletion (needed for Section 2 manual restore).
#
# Args: <guest-name> <debug-pod>
set -eu
guest="$1"
debug="$2"
ns=default

# Path INSIDE the launcher container — what we hand to ch via file://
launcher_snap_dir="/var/lib/kubeswift/run/default-${guest}/snap"
# Same path INSIDE the debug pod (via the kubelet emptyDir mount)
host_snap_dir_template="/host/kubelet-pods/UID/volumes/kubernetes.io~empty-dir/run/default-${guest}/snap"
# Stable persistence path (hostPath) for surviving launcher pod teardown
persist_dir="/tmp/snap/${guest}"

ts() { date -u +%Y-%m-%dT%H:%M:%S.%3NZ; }
section() { printf '\n========== %s ==========\n' "$1"; }

trap 'echo "(trap) re-trying resume in case pause is left dangling"; \
      kubectl exec "$debug" -n "$ns" -- /tmp/ch-api.sh "$UID_" "$guest" PUT /api/v1/vm.resume \
        >/dev/null 2>&1 || true' EXIT

section "guest pre-check"
kubectl get swiftguest "$guest" -n "$ns" \
  -o jsonpath='{.metadata.name}: phase={.status.phase} ip={.status.network.primaryIP}{"\n"}'

UID_=$(kubectl get pod "$guest" -n "$ns" -o jsonpath='{.metadata.uid}')
VM_IP=$(kubectl get swiftguest "$guest" -n "$ns" -o jsonpath='{.status.network.primaryIP}')
CH_PID=$(kubectl exec "$debug" -n "$ns" -- sh -c "
for pid in \$(pgrep -f cloud-hypervisor); do
  cmdline=\$(tr '\\0' ' ' </proc/\$pid/cmdline)
  if echo \"\$cmdline\" | grep -q 'default-${guest}'; then
    echo \$pid; break
  fi
done")
host_snap_dir=$(echo "$host_snap_dir_template" | sed "s/UID/$UID_/")
echo "pod-uid=$UID_  vm-ip=$VM_IP  ch-pid=$CH_PID"
echo "launcher snapshot dir (CH view):    $launcher_snap_dir"
echo "launcher snapshot dir (debug view): $host_snap_dir"

# --- pre-clean any prior writer state in guest ---
kubectl exec "$debug" -n "$ns" -- nsenter -t "$CH_PID" -n -- \
  ssh -i /root/.ssh/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR kubeswift@"$VM_IP" \
      'sudo pkill -f spike-ts 2>/dev/null; sudo rm -f /run/spike-ts.log; true' || true

# --- pre-clean snapshot dirs ---
kubectl exec "$debug" -n "$ns" -- sh -c "
  rm -rf '$host_snap_dir' '$persist_dir' 2>/dev/null || true
  mkdir -p '$host_snap_dir' '$persist_dir'
"

section "vm.info BEFORE pause"
kubectl exec "$debug" -n "$ns" -- /tmp/ch-api.sh "$UID_" "$guest" GET /api/v1/vm.info \
  | jq '{state, mem: .config.memory.size, cpus: .config.cpus.boot_vcpus}'

section "guest uptime BEFORE pause (boot-time anchor)"
kubectl exec "$debug" -n "$ns" -- nsenter -t "$CH_PID" -n -- \
  ssh -i /root/.ssh/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=15 -o LogLevel=ERROR \
      kubeswift@"$VM_IP" 'echo boot=$(uptime -s); echo now=$(date -u +%Y-%m-%dT%H:%M:%SZ); echo proc-uptime=$(cat /proc/uptime)'

section "start in-guest tmpfs timestamp writer (proves memory state survival)"
kubectl exec "$debug" -n "$ns" -- nsenter -t "$CH_PID" -n -- \
  ssh -i /root/.ssh/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR \
      kubeswift@"$VM_IP" 'sudo nohup sh -c "while true; do date -u +%s.%N >> /run/spike-ts.log; sleep 0.5; done" >/tmp/ts-writer.out 2>&1 &
sleep 1
sudo wc -l /run/spike-ts.log; sudo head -1 /run/spike-ts.log; sudo tail -1 /run/spike-ts.log'
sleep 3

section "PAUSE  $(ts)"
PAUSE_T0=$(date -u +%s.%N)
kubectl exec "$debug" -n "$ns" -- /tmp/ch-api.sh "$UID_" "$guest" PUT /api/v1/vm.pause
PAUSE_T1=$(date -u +%s.%N)
printf 'pause T0=%s T1=%s  delta=%.3fs\n' "$PAUSE_T0" "$PAUSE_T1" "$(echo "$PAUSE_T1 - $PAUSE_T0" | bc -l)"

section "SNAPSHOT to file://${launcher_snap_dir}/  $(ts)"
SNAP_T0=$(date -u +%s.%N)
kubectl exec "$debug" -n "$ns" -- /tmp/ch-api.sh "$UID_" "$guest" PUT /api/v1/vm.snapshot \
  "{\"destination_url\":\"file://${launcher_snap_dir}/\"}"
SNAP_T1=$(date -u +%s.%N)
printf 'snapshot T0=%s T1=%s  delta=%.3fs\n' "$SNAP_T0" "$SNAP_T1" "$(echo "$SNAP_T1 - $SNAP_T0" | bc -l)"

section "RESUME  $(ts)"
RESUME_T0=$(date -u +%s.%N)
kubectl exec "$debug" -n "$ns" -- /tmp/ch-api.sh "$UID_" "$guest" PUT /api/v1/vm.resume
RESUME_T1=$(date -u +%s.%N)
printf 'resume T0=%s T1=%s  delta=%.3fs\n' "$RESUME_T0" "$RESUME_T1" "$(echo "$RESUME_T1 - $RESUME_T0" | bc -l)"

section "PAUSE WINDOW SUMMARY (operator-visible downtime)"
awk -v p0="$PAUSE_T0" -v r1="$RESUME_T1" -v sn0="$SNAP_T0" -v sn1="$SNAP_T1" 'BEGIN{
  printf "pause→resume:    %.3fs  (full operator-visible downtime)\n", r1 - p0
  printf "snapshot only:   %.3fs  (capture phase)\n", sn1 - sn0
}'

section "vm.info AFTER resume"
kubectl exec "$debug" -n "$ns" -- /tmp/ch-api.sh "$UID_" "$guest" GET /api/v1/vm.info \
  | jq '{state, mem: .config.memory.size}'

section "snapshot directory contents (file names + sizes — opaque to KubeSwift)"
kubectl exec "$debug" -n "$ns" -- sh -c "ls -lh '$host_snap_dir'; echo total: \$(du -sh '$host_snap_dir' | cut -f1)"

section "copy snapshot to stable hostPath ${persist_dir} (so it survives pod teardown for Section 2)"
kubectl exec "$debug" -n "$ns" -- sh -c "cp -a '$host_snap_dir/.' '$persist_dir/'; ls -lh '$persist_dir'; echo total: \$(du -sh '$persist_dir' | cut -f1)"

section "verify guest state survived (uptime continuity + writer file gap)"
kubectl exec "$debug" -n "$ns" -- nsenter -t "$CH_PID" -n -- \
  ssh -i /root/.ssh/id_ed25519 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=15 -o LogLevel=ERROR \
      kubeswift@"$VM_IP" '
echo "--- uptime AFTER resume (boot must match BEFORE) ---"
echo boot=$(uptime -s); echo now=$(date -u +%Y-%m-%dT%H:%M:%SZ); echo proc-uptime=$(cat /proc/uptime)
echo "--- timestamp writer file: line count + first/last + gap ---"
sudo wc -l /run/spike-ts.log
sudo head -1 /run/spike-ts.log; sudo tail -1 /run/spike-ts.log
sudo awk "BEGIN{prev=0;maxd=0;maxat=0}
          {if (prev>0){d=\$1-prev; if (d>maxd){maxd=d;maxat=prev}} prev=\$1}
          END{printf \"max-gap=%.3fs at sample %s\\n\", maxd, maxat}" /run/spike-ts.log
echo "--- writer process still alive? ---"
sudo pgrep -af "spike-ts" | head -3 || echo "(no match — writer died?)"
'
