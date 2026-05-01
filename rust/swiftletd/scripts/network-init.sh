#!/bin/sh
# Network init: create bridges and tap devices for VM networking.
#
# Two modes:
#   1. Legacy (no "nics" in intent): create br0 + tap0 + NAT (backward compat)
#   2. Multi-NIC ("nics" array in intent): create bridge+tap per NIC,
#      bridging Multus interfaces for secondary NICs
#
# eth0 is the pod's uplink and must keep its IP for swiftletd to reach the Kubernetes API.
# br0 gets its own subnet; VM traffic is NATted out via eth0.
set -e

INTENT_PATH="${KUBESWIFT_INTENT_PATH:-/var/lib/kubeswift/intent/runtime-intent.json}"

# --- Helper functions ---

setup_primary_nic() {
    local bridge="$1" tap="$2"

    # VM-internal subnet (per launcher-pod network namespace; must NOT
    # collide with the cluster pod-CIDR pool or any per-node Calico
    # allocation, otherwise SYN-ACK replies to cross-node traffic
    # routed via this br0 (linkdown) instead of eth0 — see
    # docs/design/live-migration-phase-3a-spike.md B0 finding.
    local bridge_ip="${BRIDGE_IP:-192.168.99.1/24}"

    # Derive the network address from bridge_ip for iptables MASQUERADE.
    # bridge_ip is host/prefix (e.g. 192.168.99.1/24). We need the
    # network/prefix form (e.g. 192.168.99.0/24) for the source-match
    # and exclude-match rules.
    local bridge_addr="${bridge_ip%/*}"
    local bridge_prefix="${bridge_ip#*/}"
    # Compute network address by zeroing the last octet of bridge_addr
    # for /24 subnets. For now we only support /24; if bridge_prefix is
    # different, fail loudly so an operator override doesn't silently
    # mis-program iptables.
    if [ "$bridge_prefix" != "24" ]; then
        echo "ERROR: BRIDGE_IP prefix must be /24 (got $bridge_prefix)"
        exit 1
    fi
    local bridge_net="${bridge_addr%.*}.0/24"

    # Create bridge (internal only -- do NOT add eth0)
    ip link add "$bridge" type bridge stp_state 0
    ip link set "$bridge" up

    # Give the bridge an IP -- dnsmasq will bind here, and this is the VM's gateway
    ip addr add "$bridge_ip" dev "$bridge"

    # Create tap interface for the VM's virtio-net
    ip tuntap add dev "$tap" mode tap
    ip link set "$tap" up
    ip link set "$tap" master "$bridge"

    # Enable IP forwarding
    echo 1 > /proc/sys/net/ipv4/ip_forward

    # Masquerade VM traffic out through the pod's real interface.
    # Source/exclude derive from bridge_ip so any operator override
    # via BRIDGE_IP env stays internally consistent.
    iptables -t nat -A POSTROUTING -s "$bridge_net" ! -d "$bridge_net" -j MASQUERADE

    echo "Primary NIC: $bridge ($bridge_ip, net $bridge_net) with $tap"
}

setup_secondary_nic() {
    local bridge="$1" tap="$2" multus_iface="$3"

    # Wait for Multus to create the interface (up to 30s)
    local attempts=0
    while [ $attempts -lt 30 ]; do
        if ip link show "$multus_iface" >/dev/null 2>&1; then
            break
        fi
        sleep 1
        attempts=$((attempts + 1))
    done

    if ! ip link show "$multus_iface" >/dev/null 2>&1; then
        echo "ERROR: Multus interface $multus_iface not found after 30s"
        exit 1
    fi

    # Create bridge for this secondary NIC
    ip link add "$bridge" type bridge stp_state 0
    ip link set "$bridge" up

    # Create tap interface
    ip tuntap add dev "$tap" mode tap
    ip link set "$tap" up
    ip link set "$tap" master "$bridge"

    # Attach the Multus interface to the bridge
    ip link set "$multus_iface" master "$bridge"
    ip link set "$multus_iface" up

    echo "Secondary NIC: $bridge with $tap, bridged to $multus_iface"
}

# --- Main ---

# Check if intent has "nics" array (multi-NIC mode)
has_nics() {
    [ -f "$INTENT_PATH" ] || return 1
    grep -q '"nics"' "$INTENT_PATH" || return 1
    return 0
}

if has_nics; then
    # Multi-NIC mode: parse NIC list from intent JSON
    # Uses lightweight JSON parsing with grep/sed -- no jq dependency
    # Extract the nics array content
    NIC_COUNT=$(grep -o '"tapDevice"' "$INTENT_PATH" | wc -l)

    if [ "$NIC_COUNT" -eq 0 ]; then
        echo "nics array is empty, falling back to legacy mode"
        setup_primary_nic "br0" "tap0"
        echo "Network init done: br0 (internal) with tap0"
        exit 0
    fi

    # Parse each NIC from the intent JSON using python3 (available in base images)
    # This is more reliable than shell-based JSON parsing for arrays
    python3 -c "
import json, sys
with open('$INTENT_PATH') as f:
    intent = json.load(f)
nics = intent.get('nics', [])
for nic in nics:
    nic_type = nic.get('type', 'bridge')
    primary = '1' if nic.get('primary', False) else '0'
    multus = nic.get('multusInterface', '')
    tap = nic.get('tapDevice', '')
    bridge = nic.get('bridge', '')
    print(f\"{nic_type}|{tap}|{bridge}|{primary}|{multus}|{nic['name']}\")
" | while IFS='|' read -r nic_type tap bridge primary multus name; do
        if [ "$nic_type" = "sriov" ]; then
            echo "Skipping SR-IOV interface $name -- VFIO passthrough handled by swiftletd"
            continue
        fi
        if [ "$primary" = "1" ]; then
            setup_primary_nic "$bridge" "$tap"
        else
            setup_secondary_nic "$bridge" "$tap" "$multus"
        fi
    done

    echo "Network init done: $NIC_COUNT NIC(s) configured"
else
    # Legacy mode: single NIC (backward compatible)
    BRIDGE="${BRIDGE_NAME:-br0}"
    TAP="${TAP_NAME:-tap0}"
    setup_primary_nic "$BRIDGE" "$TAP"
    echo "Network init done: $BRIDGE (internal) with $TAP"
fi
