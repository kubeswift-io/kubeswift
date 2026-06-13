#!/bin/sh
# Launcher entrypoint: when network enabled, start dnsmasq then exec swiftletd.
# Otherwise exec swiftletd directly.
set -e

INTENT_PATH="${KUBESWIFT_INTENT_PATH:-/var/lib/kubeswift/intent/runtime-intent.json}"
RUN_DIR="${KUBESWIFT_RUN_DIR:-/var/lib/kubeswift/run}"

# NOTE: an earlier attempt set cgroup-v2 memory.high here to dodge the
# memory-snapshot launcher OOM. It was REMOVED -- cluster validation showed it
# could not reclaim the unreclaimable guest-RAM footprint (memfd + CoW anon),
# only throttled CH (70k+ breaches, cgroup stalls) and still OOM'd. The real
# fix is `--memory ...,shared=on` (swift-ch-client config.rs), which halves the
# footprint by mapping the guest-RAM memfd MAP_SHARED instead of MAP_PRIVATE.
# Do NOT reintroduce a memory.high self-write here.

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

    # Primary-on-NAD (multi-node L2, EXPERIMENTAL): network-init persisted the
    # NAD-assigned primary IP. Hand exactly that IP to the guest (matched by
    # MAC) via a fixed dnsmasq lease, so the guest's primary IP is the NAD's
    # portable IP. lease.rs then discovers it from the same lease file (no code
    # change). DATAPATH UNVALIDATED -- see docs/networking/multi-node-l2.md.
    nad_env="$lease_dir/primary-nad.env"
    if [ -f "$nad_env" ]; then
        # shellcheck disable=SC1090
        . "$nad_env"   # NAD_IP, NAD_PREFIX, NAD_GW, NAD_MAC, NAD_BRIDGE
        dns=$(grep '^nameserver ' /etc/resolv.conf 2>/dev/null | head -1 | awk '{print $2}')
        [ -z "$dns" ] && dns="10.96.0.10"
        echo "Primary-on-NAD dnsmasq: handing $NAD_IP to $NAD_MAC on $NAD_BRIDGE (gw $NAD_GW)"
        dnsmasq --no-daemon --bind-interfaces --interface="$NAD_BRIDGE" \
            --dhcp-range="$NAD_IP,$NAD_IP" \
            ${NAD_MAC:+--dhcp-host="$NAD_MAC,$NAD_IP"} \
            ${NAD_GW:+--dhcp-option=option:router,"$NAD_GW"} \
            --dhcp-option=option:dns-server,"$dns" \
            --dhcp-leasefile="$lease_dir/dnsmasq.leases" \
            --dhcp-authoritative &
        sleep 1
        exec /usr/local/bin/swiftletd "$@"
    fi

    # Derive subnet from bridge (e.g. 10.244.125.1/24 -> 10.244.125.0/24)
    br_addr=$(ip -4 addr show "$BRIDGE" 2>/dev/null | grep 'inet ' | awk '{print $2}' | head -1)
    [ -z "$br_addr" ] && { echo "No IP on $BRIDGE"; exit 1; }
    base=$(echo "$br_addr" | cut -d'/' -f1 | sed 's/\.[0-9]*$/.0/')
    gateway=$(echo "$base" | sed 's/\.0$/.1/')
    range_start=$(echo "$base" | sed 's/\.0$/.10/')
    range_end=$(echo "$base" | sed 's/\.0$/.20/')
    range="$range_start,$range_end"

    # Service exposure: network-init pinned the primary VM IP and installed the
    # in-pod DNAT against it. Narrow the DHCP range to that single address so the
    # VM deterministically gets the DNAT target IP (no silent failure).
    # See docs/design/service-exposure.md.
    if [ -f "$lease_dir/expose.env" ]; then
        # shellcheck disable=SC1090
        . "$lease_dir/expose.env"   # EXPOSE_VM_IP
        [ -n "$EXPOSE_VM_IP" ] && range="$EXPOSE_VM_IP,$EXPOSE_VM_IP"
    fi

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
