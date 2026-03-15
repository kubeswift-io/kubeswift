#!/bin/sh
# Launcher entrypoint: when network enabled, start dnsmasq then exec swiftletd.
# Otherwise exec swiftletd directly.
set -e

INTENT_PATH="${KUBESWIFT_INTENT_PATH:-/var/lib/kubeswift/intent/runtime-intent.json}"
RUN_DIR="${KUBESWIFT_RUN_DIR:-/var/lib/kubeswift/run}"
BRIDGE="${BRIDGE_NAME:-br0}"

# Check if network is enabled (has seed; network defaults true when seed present)
network_enabled() {
    [ -f "$INTENT_PATH" ] || return 1
    # Has seed and network not explicitly false
    grep -q '"seedPath"[[:space:]]*:[[:space:]]*"[^"]\+"' "$INTENT_PATH" || return 1
    grep -q '"network"[[:space:]]*:[[:space:]]*false' "$INTENT_PATH" && return 1
    return 0
}

if network_enabled; then
    guest_id=$(grep -o '"guestId"[[:space:]]*:[[:space:]]*"[^"]*"' "$INTENT_PATH" | cut -d'"' -f4)
    # Sanitize guest_id for path (default/rocky -> default-rocky)
    safe_id=$(echo "$guest_id" | tr '/' '-')
    lease_dir="$RUN_DIR/$safe_id"
    mkdir -p "$lease_dir"

    # Derive subnet from br0 (e.g. 10.244.1.1/24 -> 10.244.1.0/24)
    br_addr=$(ip -4 addr show "$BRIDGE" 2>/dev/null | grep 'inet ' | awk '{print $2}' | head -1)
    [ -z "$br_addr" ] && { echo "No IP on $BRIDGE"; exit 1; }
    base=$(echo "$br_addr" | cut -d'/' -f1 | sed 's/\.[0-9]*$/.0/')
    range_start=$(echo "$base" | sed 's/\.0$/.10/')
    range_end=$(echo "$base" | sed 's/\.0$/.20/')
    range="$range_start,$range_end"

    dnsmasq --no-daemon --bind-interfaces --interface="$BRIDGE" \
        --dhcp-range="$range" \
        --dhcp-leasefile="$lease_dir/dnsmasq.leases" \
        --dhcp-authoritative &
    sleep 1
fi

exec /usr/local/bin/swiftletd "$@"
