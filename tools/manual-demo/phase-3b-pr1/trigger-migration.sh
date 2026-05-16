#!/usr/bin/env bash
# Phase 3b PR 1 manual demo — drive a live migration end-to-end via
# direct annotation patches on the launcher pods (no controller).
#
# Validates the 4 PR 1 deliverables:
#   - Commit C: `migration-status: receive-ready` annotation emitted
#     by swiftletd-dst before vm.receive-migration HTTP issued.
#   - Commit D: `migration-status: sending` annotation emitted by
#     swiftletd-src before vm.send-migration HTTP issued.
#   - Commit D: `migration-progress-estimate` annotation emitted at
#     ~5s intervals during send; cap at 95.
#   - Wire format unchanged: `migration-pause-window-ms` annotation
#     on src at terminal complete (which the Phase 3b PR 1 Commit E
#     controller dual-writes; not exercised in this manual demo
#     because there's no SwiftMigration CR — see README).
set -euo pipefail

NS="${NS:-phase-3b-pr1-demo}"
SRC_POD="${SRC_POD:?must be set; see launch-pods.sh output}"
DST_POD="${DST_POD:?must be set; see launch-pods.sh output}"
GUEST_IP="${GUEST_IP:?must be set; see launch-pods.sh output}"
GUEST_RAM_MIB="${GUEST_RAM_MIB:-4096}"
DST_LISTEN_PORT="${DST_LISTEN_PORT:-6789}"

TS="$(date +%s)"
RECV_ID="demo-recv-${TS}"
SEND_ID="demo-send-${TS}"

step() { printf '\n[%s] %s\n' "$1" "$2"; }
ann()  { kubectl -n "${NS}" get pod "$1" -o jsonpath="{.metadata.annotations.kubeswift\\.io/${2}}" 2>/dev/null || true; }
wait_ann() {
  # wait_ann <pod> <key> <expected> [timeout_sec]
  local pod="$1" key="$2" expected="$3" timeout="${4:-90}"
  for i in $(seq 1 "${timeout}"); do
    v="$(ann "${pod}" "${key}")"
    if [ "${v}" = "${expected}" ]; then
      echo "  observed ${key}=${expected} on ${pod} (t+${i}s)"
      return 0
    fi
    sleep 1
  done
  echo "ERROR: timed out waiting for ${key}=${expected} on ${pod}" >&2
  return 1
}

# Get the destination node IP (where the receiver will listen). Since
# CH binds to 0.0.0.0:<port>, the dst pod's IP is what the source
# uses to connect.
DST_POD_IP="$(kubectl -n "${NS}" get pod "${DST_POD}" -o jsonpath='{.status.podIP}')"
if [ -z "${DST_POD_IP}" ]; then
  echo "ERROR: destination pod has no IP yet" >&2
  exit 1
fi

# ─── 1/8 ack annotations ───────────────────────────────────────────
step 1/8 "ack annotations on both pods"
kubectl -n "${NS}" annotate pod "${SRC_POD}" \
  kubeswift.io/migration-phase2-unsafe-plaintext=ack --overwrite >/dev/null
kubectl -n "${NS}" annotate pod "${DST_POD}" \
  kubeswift.io/migration-phase2-unsafe-plaintext=ack --overwrite >/dev/null

# ─── 2/8 dispatch receive on dst ───────────────────────────────────
step 2/8 "receive action on ${DST_POD} (id=${RECV_ID})"
RECV_ARGS=$(printf '{"listen_url":"tcp:0.0.0.0:%s","timeout_seconds":600,"guest_ip":"%s"}' "${DST_LISTEN_PORT}" "${GUEST_IP}")
kubectl -n "${NS}" annotate pod "${DST_POD}" \
  "kubeswift.io/migration-action=receive" \
  "kubeswift.io/migration-action-id=${RECV_ID}" \
  "kubeswift.io/migration-action-args=${RECV_ARGS}" \
  --overwrite >/dev/null

# ─── 3/8 wait for receive-ready (Commit C) ─────────────────────────
step 3/8 "wait for dst pre-dispatch annotation: migration-status=receive-ready"
wait_ann "${DST_POD}" "migration-status" "receive-ready" 30

# ─── 4/8 dispatch send on src ──────────────────────────────────────
step 4/8 "send action on ${SRC_POD} (id=${SEND_ID}, guest_ram_mib=${GUEST_RAM_MIB})"
SEND_ARGS=$(printf '{"target_url":"tcp:%s:%s","timeout_seconds":600,"guest_ram_mib":%s}' "${DST_POD_IP}" "${DST_LISTEN_PORT}" "${GUEST_RAM_MIB}")
kubectl -n "${NS}" annotate pod "${SRC_POD}" \
  "kubeswift.io/migration-action=send" \
  "kubeswift.io/migration-action-id=${SEND_ID}" \
  "kubeswift.io/migration-action-args=${SEND_ARGS}" \
  --overwrite >/dev/null

# ─── 5/8 wait for sending (Commit D) ───────────────────────────────
step 5/8 "wait for src pre-dispatch annotation: migration-status=sending"
wait_ann "${SRC_POD}" "migration-status" "sending" 30

# ─── 6/8 progress estimate samples (Commit D) ──────────────────────
step 6/8 "progress estimate samples on src (~5s cadence, cap 95)"
SAMPLE_START=$(date +%s)
LAST_PCT=""
for i in $(seq 1 30); do
  pct=$(ann "${SRC_POD}" "migration-progress-estimate")
  if [ -n "${pct}" ] && [ "${pct}" != "${LAST_PCT}" ]; then
    elapsed=$(( $(date +%s) - SAMPLE_START ))
    printf '  t+%3ds: migration-progress-estimate=%s\n' "${elapsed}" "${pct}"
    LAST_PCT="${pct}"
  fi
  # Stop sampling once src migration-status transitions terminal.
  status=$(ann "${SRC_POD}" "migration-status")
  if [ "${status}" = "complete" ] || [ "${status}" = "failed" ]; then
    break
  fi
  sleep 5
done

# ─── 7/8 wait for src complete ─────────────────────────────────────
step 7/8 "wait for src terminal annotations"
wait_ann "${SRC_POD}" "migration-status" "complete" 60
pause_ms=$(ann "${SRC_POD}" "migration-pause-window-ms")
echo "  migration-pause-window-ms=${pause_ms} (= status.observedTransferDuration in Phase 3b PR 1 Commit E when controller is in the loop)"

# ─── 8/8 wait for dst running ──────────────────────────────────────
step 8/8 "wait for dst terminal annotations"
wait_ann "${DST_POD}" "migration-status" "running" 30

echo
echo "MIGRATION COMPLETE"
echo "  total wall-clock (src dispatch → dst running): see step timestamps"
echo "  transfer duration (vm.send-migration RPC):     ${pause_ms} ms"
echo
echo "Next: cleanup.sh to tear down."
