#!/bin/sh
# Launcher entrypoint: when network enabled, start dnsmasq then exec swiftletd.
# Otherwise exec swiftletd directly.
set -e

INTENT_PATH="${KUBESWIFT_INTENT_PATH:-/var/lib/kubeswift/intent/runtime-intent.json}"
RUN_DIR="${KUBESWIFT_RUN_DIR:-/var/lib/kubeswift/run}"

# Set cgroup v2 memory.high to ~85% of memory.max so the kernel proactively
# reclaims reclaimable page cache (chiefly the guest root-disk read cache)
# BEFORE the container hits the hard memory.max wall. cgroup v2 does no
# proactive reclaim by default (only the hard limit), so a memory-snapshot
# capture's page-cache write burst can OOMKill the launcher during Capturing
# (cluster-diagnosed). memory.high gives the kernel a throttle-and-reclaim band
# below the wall; combined with direct=on disk I/O (which stops the cache
# building in the first place) it keeps the snapshot write within budget.
#
# Best-effort and MUST NEVER fail the launcher (set -e is on): no-op on
# cgroup v1, when memory.max is unlimited, or when the cgroup files are not
# writable (delegation varies by runtime/host -- the spike confirmed they are
# writable on this cluster, but stay defensive).
set_memory_high() {
    cg=/sys/fs/cgroup
    # cgroup v2 unified hierarchy only (memory.high is v2-only).
    [ -f "$cg/cgroup.controllers" ] || return 0
    [ -w "$cg/memory.high" ] || return 0
    max=$(cat "$cg/memory.max" 2>/dev/null) || return 0
    # "max" == unlimited -> nothing to throttle against.
    [ "$max" = "max" ] && return 0
    # Guard: only proceed if memory.max is a plain integer (bytes).
    case "$max" in '' | *[!0-9]*) return 0 ;; esac
    high=$((max * 85 / 100))
    [ "$high" -gt 0 ] || return 0
    if echo "$high" > "$cg/memory.high" 2>/dev/null; then
        echo "launcher: set memory.high=$high (memory.max=$max) for proactive cache reclaim"
    fi
    return 0
}
set_memory_high || true

network_enabled() {
    [ -f "$INTENT_PATH" ] || return 1
    grep -q '"network"[[:space:]]*:[[:space:]]*true' "$INTENT_PATH" || return 1
    return 0
}

# Determine the primary bridge name.
# Multi-NIC mode: read from nics array. Legacy mode: default br0.
get_primary_bridge() {
    if grep -q '"nics"' "$INTENT_PATH" 2>/dev/null; then
        # Extract bridge name from the primary NIC entry
        python3 -c "
import json
with open('$INTENT_PATH') as f:
    intent = json.load(f)
nics = intent.get('nics', [])
for nic in nics:
    if nic.get('primary', False):
        print(nic['bridge'])
        break
else:
    print('br0')
" 2>/dev/null || echo "br0"
    else
        echo "${BRIDGE_NAME:-br0}"
    fi
}

if network_enabled; then
    BRIDGE=$(get_primary_bridge)

    guest_id=$(grep -o '"guestId"[[:space:]]*:[[:space:]]*"[^"]*"' "$INTENT_PATH" | cut -d'"' -f4)
    # Sanitize guest_id for path (default/rocky -> default-rocky)
    safe_id=$(echo "$guest_id" | tr '/' '-')
    lease_dir="$RUN_DIR/$safe_id"
    mkdir -p "$lease_dir"

    # Derive subnet from bridge (e.g. 10.244.125.1/24 -> 10.244.125.0/24)
    br_addr=$(ip -4 addr show "$BRIDGE" 2>/dev/null | grep 'inet ' | awk '{print $2}' | head -1)
    [ -z "$br_addr" ] && { echo "No IP on $BRIDGE"; exit 1; }
    base=$(echo "$br_addr" | cut -d'/' -f1 | sed 's/\.[0-9]*$/.0/')
    gateway=$(echo "$base" | sed 's/\.0$/.1/')
    range_start=$(echo "$base" | sed 's/\.0$/.10/')
    range_end=$(echo "$base" | sed 's/\.0$/.20/')
    range="$range_start,$range_end"

    # DNS: use cluster DNS from resolv.conf, fallback to 10.96.0.10
    dns=$(grep '^nameserver ' /etc/resolv.conf 2>/dev/null | head -1 | awk '{print $2}')
    [ -z "$dns" ] && dns="10.96.0.10"

    dnsmasq --no-daemon --bind-interfaces --interface="$BRIDGE" \
        --dhcp-range="$range" \
        --dhcp-option=option:router,"$gateway" \
        --dhcp-option=option:dns-server,"$dns" \
        --dhcp-leasefile="$lease_dir/dnsmasq.leases" \
        --dhcp-authoritative &
    sleep 1
fi

exec /usr/local/bin/swiftletd "$@"
