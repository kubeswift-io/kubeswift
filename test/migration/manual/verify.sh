#!/bin/bash
# verify.sh — Phase 2 manual demo: post-migration sentinel verification.
#
# Connects to the destination launcher pod's serial console (CH's
# unix-socket serial) and reads `/root/sentinel.txt` from the migrated
# guest. The operator must have written a known marker file to
# /root/sentinel.txt before kicking off the migration; this script
# captures the post-migration value so the operator can compare md5s.
#
# Required state (from STATE_FILE, written by source.sh + run.sh):
#   DST_POD, NAMESPACE
#
# Optional env:
#   STATE_FILE — defaults to /tmp/kubeswift-migration-phase2-manual/state.env
#
# Output:
#   /tmp/kubeswift-migration-phase2-manual/sentinel-post-migration.txt
#   contains the sentinel content + md5sum from the migrated guest.
#
# Verification recipe:
#   1. Pre-migration:
#        echo SPIKE-PRE-MIGRATION-$(date +%s) | sudo tee /root/sentinel.txt
#        sudo md5sum /root/sentinel.txt    # record this
#   2. Run source.sh + destination.sh + run.sh
#   3. ./verify.sh   # reads sentinel from migrated guest
#   4. Compare md5sum from step 3 against step 1.
#
# Approach: socat to the guest's serial socket inside the dst launcher
# container. Write a `cat /root/sentinel.txt && md5sum /root/sentinel.txt`
# command, capture the response.

set -euo pipefail

STATE_FILE="${STATE_FILE:-/tmp/kubeswift-migration-phase2-manual/state.env}"
if [[ ! -f "$STATE_FILE" ]]; then
    echo "ERROR: $STATE_FILE not found." >&2
    exit 1
fi
# shellcheck source=/dev/null
. "$STATE_FILE"

: "${DST_POD:?STATE_FILE missing DST_POD}"
: "${NAMESPACE:?STATE_FILE missing NAMESPACE}"

OUT="$WORKDIR/sentinel-post-migration.txt"

echo "==> Connecting to dst pod's serial socket and reading /root/sentinel.txt..."
echo "    (the guest must have a previously-written sentinel; the operator"
echo "     records the pre-migration md5 manually before run.sh fires)"
echo

# socat may not be in the launcher container by default; fall back to
# `kubectl exec` with python3's socket lib if so.
if kubectl exec "$DST_POD" -n "$NAMESPACE" -c launcher -- which socat >/dev/null 2>&1; then
    timeout 30 kubectl exec -i "$DST_POD" -n "$NAMESPACE" -c launcher -- \
        socat - UNIX-CONNECT:/var/lib/kubeswift/runtime/serial.sock \
        > "$OUT" 2>&1 <<<"cat /root/sentinel.txt && md5sum /root/sentinel.txt" || true
else
    echo "WARNING: socat not present in launcher container; falling back to python"
    timeout 30 kubectl exec -i "$DST_POD" -n "$NAMESPACE" -c launcher -- \
        python3 -c '
import socket, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect("/var/lib/kubeswift/runtime/serial.sock")
s.sendall(sys.stdin.buffer.read())
import time; time.sleep(2)
s.shutdown(socket.SHUT_WR)
while True:
    d = s.recv(4096)
    if not d: break
    sys.stdout.buffer.write(d)
' > "$OUT" 2>&1 <<<"cat /root/sentinel.txt && md5sum /root/sentinel.txt" || true
fi

echo "==> Captured serial output to $OUT:"
echo "----- begin output -----"
cat "$OUT"
echo "----- end output -----"
echo
echo "Compare the md5sum above against the pre-migration value to verify"
echo "the guest's disk content survived the migration."
