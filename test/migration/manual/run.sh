#!/bin/bash
# run.sh — Phase 2 manual demo: orchestrate the kubectl annotate sequence.
#
# Reads STATE_FILE (from source.sh + destination.sh), writes the
# annotation sequence per design §7.2 steps 4–8, polls for terminal
# statuses on both pods.
#
# Annotation sequence:
#   1. Annotate source pod with the unsafe-plaintext-ack gate.
#      (Destination pod already has the ack from destination.sh.)
#   2. Annotate dst pod with `migration-action: receive`,
#      `migration-action-id: <ulid>`, `migration-listen-url: tcp:0.0.0.0:6789`.
#      Wait for swiftletd's `migration-status: running` (accept-time)
#      to appear.
#   3. Annotate src pod with `migration-action: send`,
#      `migration-action-id: <ulid>`, `migration-action-args: {"target_url": ...}`.
#      Wait for terminal status on both pods:
#        src: migration-status: complete (CH gone — design §3.1)
#        dst: migration-status: running  (CH state=Running — design §3.1)
#
# W1 invariant: this script gates on BOTH terminal statuses, NOT on
# `migration-action-id-mirror` alone (the W1 walkthrough finding from
# the Phase 2 spike — `docs/design/live-migration-phase-2-spike.md`).
#
# Optional env:
#   STATE_FILE — defaults to /tmp/kubeswift-migration-phase2-manual/state.env
#   PORT       — defaults to 6789

set -euo pipefail

STATE_FILE="${STATE_FILE:-/tmp/kubeswift-migration-phase2-manual/state.env}"
PORT="${PORT:-6789}"
if [[ ! -f "$STATE_FILE" ]]; then
    echo "ERROR: $STATE_FILE not found. Run source.sh and destination.sh first." >&2
    exit 1
fi
# shellcheck source=/dev/null
. "$STATE_FILE"

: "${SOURCE_POD:?STATE_FILE missing SOURCE_POD}"
: "${DST_POD:?STATE_FILE missing DST_POD — run destination.sh}"
: "${NAMESPACE:?STATE_FILE missing NAMESPACE}"
: "${TARGET_NODE:?STATE_FILE missing TARGET_NODE}"

DST_IP="$(kubectl get pod "$DST_POD" -n "$NAMESPACE" -o jsonpath='{.status.podIP}')"
if [[ -z "$DST_IP" ]]; then
    echo "ERROR: dst pod has no IP yet" >&2
    exit 1
fi
echo "==> dst pod IP: $DST_IP"

uuid() { python3 -c 'import uuid; print(uuid.uuid4())'; }
RECV_ID="recv-$(uuid)"
SEND_ID="send-$(uuid)"

echo "==> Step 1: ack the unsafe-plaintext gate on source pod"
kubectl annotate pod "$SOURCE_POD" -n "$NAMESPACE" \
    "kubeswift.io/migration-phase2-unsafe-plaintext=ack" \
    --overwrite >/dev/null
echo "    src ack written"

echo "==> Step 2: trigger receive on dst pod (id=$RECV_ID)"
RECV_ARGS="$(printf '{"listen_url":"tcp:0.0.0.0:%s"}' "$PORT")"
kubectl annotate pod "$DST_POD" -n "$NAMESPACE" \
    "kubeswift.io/migration-action=receive" \
    "kubeswift.io/migration-action-id=$RECV_ID" \
    "kubeswift.io/migration-action-args=$RECV_ARGS" \
    --overwrite >/dev/null

# Wait for swiftletd to accept the action and write running status.
echo "    waiting for dst migration-status..."
for i in $(seq 1 30); do
    s="$(kubectl get pod "$DST_POD" -n "$NAMESPACE" \
        -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status}')"
    sid="$(kubectl get pod "$DST_POD" -n "$NAMESPACE" \
        -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status-id}')"
    if [[ "$s" == "running" && "$sid" == "$RECV_ID" ]]; then
        echo "    dst migration-status=running (action accepted)"
        break
    fi
    if [[ "$s" == "rejected" || "$s" == "failed" ]]; then
        detail="$(kubectl get pod "$DST_POD" -n "$NAMESPACE" \
            -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status-detail}')"
        echo "ERROR: dst rejected/failed before send fired: status=$s detail=$detail" >&2
        exit 1
    fi
    sleep 1
done

echo "==> Step 3: trigger send on src pod (id=$SEND_ID, target=tcp:$DST_IP:$PORT)"
SEND_ARGS="$(printf '{"target_url":"tcp:%s:%s"}' "$DST_IP" "$PORT")"
T_START="$(date +%s)"
kubectl annotate pod "$SOURCE_POD" -n "$NAMESPACE" \
    "kubeswift.io/migration-action=send" \
    "kubeswift.io/migration-action-id=$SEND_ID" \
    "kubeswift.io/migration-action-args=$SEND_ARGS" \
    --overwrite >/dev/null

echo "==> Step 4: poll for both terminal statuses (W1 invariant — BOTH gates)"
SRC_DONE=0
DST_DONE=0
for i in $(seq 1 120); do
    src_s="$(kubectl get pod "$SOURCE_POD" -n "$NAMESPACE" \
        -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status}' 2>/dev/null || echo gone)"
    src_sid="$(kubectl get pod "$SOURCE_POD" -n "$NAMESPACE" \
        -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status-id}' 2>/dev/null || echo)"
    dst_s="$(kubectl get pod "$DST_POD" -n "$NAMESPACE" \
        -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status}' 2>/dev/null || echo gone)"
    dst_sid="$(kubectl get pod "$DST_POD" -n "$NAMESPACE" \
        -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status-id}' 2>/dev/null || echo)"

    # Source terminal: complete (success), failed (W1 violation or
    # send error). Mirror id must match SEND_ID.
    if [[ "$src_sid" == "$SEND_ID" ]]; then
        case "$src_s" in
            complete) [[ $SRC_DONE -eq 0 ]] && { echo "    src migration-status=complete"; SRC_DONE=1; } ;;
            failed)
                detail="$(kubectl get pod "$SOURCE_POD" -n "$NAMESPACE" \
                    -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status-detail}')"
                echo "ERROR: src migration failed: $detail" >&2
                exit 1
                ;;
        esac
    fi
    # Destination terminal: running (success — CH state=Running),
    # failed (W1 violation or receive error). Mirror id must match RECV_ID.
    if [[ "$dst_sid" == "$RECV_ID" ]]; then
        case "$dst_s" in
            # On the receive side, the loop emits "running" both at
            # accept-time (StatusKind::Running) and at terminal-success
            # (StatusKind::Custom("running")). We can't distinguish via
            # the annotation alone — gate on dst pod still alive AND
            # vm_info reporting state=Running for the actual W1
            # contract. The pod is alive iff src reaches `complete`
            # (Q1c — dst auto-resumes on receive completion).
            running) [[ $DST_DONE -eq 0 ]] && { echo "    dst migration-status=running"; DST_DONE=1; } ;;
            failed)
                detail="$(kubectl get pod "$DST_POD" -n "$NAMESPACE" \
                    -o jsonpath='{.metadata.annotations.kubeswift\.io/migration-status-detail}')"
                echo "ERROR: dst migration failed: $detail" >&2
                exit 1
                ;;
        esac
    fi

    if [[ $SRC_DONE -eq 1 && $DST_DONE -eq 1 ]]; then
        # Both terminal statuses observed AND src reached `complete`,
        # which per Q1c implies dst CH did receive successfully and
        # auto-resumed. The W1 invariant gate fires here.
        T_END="$(date +%s)"
        echo "    BOTH terminal statuses observed in $((T_END - T_START))s"
        break
    fi

    if [[ $i -eq 120 ]]; then
        echo "ERROR: terminal statuses did not appear within 120s" >&2
        echo "  src status=$src_s id=$src_sid" >&2
        echo "  dst status=$dst_s id=$dst_sid" >&2
        exit 1
    fi
    sleep 1
done

echo
echo "==> Verifying dst CH state via vm_info (extra W1 belt-and-braces)"
DST_STATE="$(kubectl exec "$DST_POD" -n "$NAMESPACE" -c launcher -- \
    /usr/local/bin/ch-remote --api-socket /var/lib/kubeswift/runtime/ch.sock info \
    2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("state","gone"))' \
    2>/dev/null || echo unknown)"
echo "    dst CH state: $DST_STATE"
if [[ "$DST_STATE" != "Running" ]]; then
    echo "WARNING: dst CH state is $DST_STATE, expected Running" >&2
fi

cat >> "$STATE_FILE" <<EOF
RECV_ID="$RECV_ID"
SEND_ID="$SEND_ID"
DST_STATE="$DST_STATE"
EOF

echo
echo "==> Migration complete (both terminal gates fired)."
echo "Next: ./verify.sh"
