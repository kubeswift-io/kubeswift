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
RUN_DIR="${KUBESWIFT_RUN_DIR:-/var/lib/kubeswift/run}"

# --- Helper functions ---

# guest_run_dir prints the per-guest run directory (shared with the launcher
# via the "run" emptyDir, mounted in this init container). Used to hand the
# launcher entrypoint the NAD-assigned primary IP (primary-on-NAD path).
guest_run_dir() {
    gid=$(grep -o '"guestId"[[:space:]]*:[[:space:]]*"[^"]*"' "$INTENT_PATH" 2>/dev/null | cut -d'"' -f4)
    echo "$RUN_DIR/$(echo "$gid" | tr '/' '-')"
}

setup_primary_nic() {
    local bridge="$1" tap="$2"

    # VM-internal subnet (per launcher-pod network namespace; must NOT
    # collide with the cluster pod-CIDR pool or any per-node Calico
    # allocation, otherwise SYN-ACK replies to cross-node traffic
    # routed via this br0 (linkdown) instead of eth0 -- see
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

# setup_exposed_ports installs the service-exposure DNAT for the primary
# (nat-bound) NIC. For each intent.ports[] entry it adds
#   PREROUTING -p <proto> --dport <port> -j DNAT --to <vmIP>:<targetPort>
# where <vmIP> is the pinned primary VM IP (.10 of the bridge subnet, matching
# the launcher entrypoint's dnsmasq range_start). It hands that pinned IP to the
# entrypoint via expose.env so dnsmasq serves a single-address lease (the IP is
# then deterministic, so the DNAT target is correct -- no silent failure).
# See docs/design/service-exposure.md. No-op when intent has no ports.
setup_exposed_ports() {
    bridge="$1"
    command -v python3 >/dev/null 2>&1 || return 0
    ports=$(python3 -c "
import json
try:
    with open('$INTENT_PATH') as f: intent = json.load(f)
except Exception:
    raise SystemExit
for p in intent.get('ports', []):
    proto = (p.get('protocol') or 'tcp').lower()
    print(f\"{proto} {p['port']} {p.get('targetPort') or p['port']}\")
" 2>/dev/null)
    [ -z "$ports" ] && return 0

    br_addr=$(ip -4 addr show "$bridge" 2>/dev/null | awk '/inet /{print $2; exit}')
    if [ -z "$br_addr" ]; then
        echo "WARN: no IPv4 on $bridge; skipping service-port DNAT" >&2
        return 0
    fi
    base=$(echo "$br_addr" | cut -d/ -f1 | sed 's/\.[0-9]*$/.0/')
    vm_ip=$(echo "$base" | sed 's/\.0$/.10/')

    rd=$(guest_run_dir); mkdir -p "$rd"
    echo "EXPOSE_VM_IP=$vm_ip" > "$rd/expose.env"

    echo "$ports" | while read -r proto port tport; do
        [ -z "$port" ] && continue
        iptables -t nat -A PREROUTING -p "$proto" --dport "$port" \
            -j DNAT --to-destination "$vm_ip:$tport"
        echo "Exposed port DNAT: $proto/$port -> $vm_ip:$tport"
    done
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

# setup_primary_nad_nic -- multi-node L2, primary-on-NAD.
#
# Datapath is cluster-validated (kube-ovn primary NAD: zero-touch cross-node
# reachability + IP-preserving live migration; see docs/networking/multi-node-l2.md
# and docs/networking/ovn-l2-install.md). It implements the KubeVirt-style "bridge
# binding": the NAD's CNI assigns the pod's Multus interface an IP; we hand that
# exact IP to the GUEST so the guest's primary IP is the NAD's portable IP
# (survives a move between nodes).
#
#   1. wait for the Multus interface, read its CNI-assigned IP + gateway
#   2. persist them (+ the guest MAC) to the per-guest run dir, for the
#      launcher entrypoint to drive a fixed-lease dnsmasq
#   3. flush the IP off the Multus interface (the GUEST will own it; avoids a
#      duplicate-address ARP conflict on the L2)
#   4. bridge the Multus interface to the guest's tap (br has NO node-local IP,
#      no NAT -- this is a flat L2 bridge, not the node-local masqueraded one)
#   5. give the bridge a best-effort helper IP in the NAD subnet so the
#      entrypoint's dnsmasq can bind and serve the fixed lease
#
# The structure (read->flush->bridge->serve->lease.rs discovers) is the contract.
# Residual caveat: the helper IP is a .254 heuristic (step 5 / below) and the
# dnsmasq is redundant when the NAD's own segment already serves DHCP.
setup_primary_nad_nic() {
    bridge="$1"; tap="$2"; multus_iface="$3"; guest_mac="$4"

    attempts=0
    while [ $attempts -lt 30 ]; do
        ip link show "$multus_iface" >/dev/null 2>&1 && break
        sleep 1; attempts=$((attempts + 1))
    done
    if ! ip link show "$multus_iface" >/dev/null 2>&1; then
        echo "ERROR: primary NAD interface $multus_iface not found after 30s" >&2
        exit 1
    fi

    nad_cidr=$(ip -4 addr show "$multus_iface" 2>/dev/null | awk '/inet /{print $2; exit}')
    if [ -z "$nad_cidr" ]; then
        echo "ERROR: primary NAD interface $multus_iface has no IPv4 address (NAD IPAM did not assign one)" >&2
        exit 1
    fi
    nad_ip="${nad_cidr%/*}"
    nad_prefix="${nad_cidr#*/}"
    nad_gw=$(ip route show default 2>/dev/null | awk -v d="$multus_iface" '$0 ~ ("dev "d){print $3; exit}')
    [ -z "$nad_gw" ] && nad_gw=$(ip route show dev "$multus_iface" 2>/dev/null | awk '/default/{print $3; exit}')

    # The NAD interface's MTU (set by the CNI -- e.g. 1450 on a VXLAN overlay).
    # Handed to the guest via DHCP so an overlay's encapsulation overhead does not
    # silently drop the guest's full-size frames.
    nad_mtu=$(ip -o link show "$multus_iface" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="mtu"){print $(i+1); exit}}')

    # Persist for the launcher entrypoint (shared run dir).
    rd=$(guest_run_dir); mkdir -p "$rd"
    {
        echo "NAD_IP=$nad_ip"
        echo "NAD_PREFIX=$nad_prefix"
        echo "NAD_GW=$nad_gw"
        echo "NAD_MAC=$guest_mac"
        echo "NAD_BRIDGE=$bridge"
        echo "NAD_MTU=$nad_mtu"
    } > "$rd/primary-nad.env"

    # The guest claims nad_ip; the host must not also hold it on the L2.
    ip addr flush dev "$multus_iface" 2>/dev/null || true

    ip link add "$bridge" type bridge stp_state 0 2>/dev/null || true
    ip link set "$bridge" up
    ip tuntap add dev "$tap" mode tap 2>/dev/null || true
    ip link set "$tap" up
    ip link set "$tap" master "$bridge"

    # kube-ovn (OVN-class) primary NAD: the controller stamps the guest's MAC onto
    # the pod NIC (ovn.kubernetes.io/mac_address) so the OVN logical-switch-port
    # identity IS the guest -- otherwise OVN's per-port ARP responder answers with
    # the pod NIC's own MAC and the bridged guest is unreachable on the segment.
    # But if the NIC then carries the SAME MAC as the guest, enslaving it to the
    # bridge makes the kernel add a permanent fdb entry <guest-mac> -> NIC, which
    # SHADOWS the guest's tap: the bridge delivers the guest's return traffic (and
    # unicast DHCP) to the NIC instead of the tap, so the guest is unreachable.
    # Re-MAC the NIC to a dummy BEFORE enslaving (the KubeVirt bridge-binding
    # pattern). The NIC's kernel MAC is not load-bearing once it is a bridge port;
    # the OVN LSP keeps the guest MAC, so OVN still delivers the guest's frames to
    # the NIC -> bridge -> tap. No-op for every NAD whose IPAM gives the NIC its own
    # distinct MAC (the plain-bridge / VXLAN-mesh path).
    nic_mac=$(cat "/sys/class/net/$multus_iface/address" 2>/dev/null)
    if [ -n "$guest_mac" ] && [ "$nic_mac" = "$guest_mac" ]; then
        ip link set "$multus_iface" down 2>/dev/null || true
        ip link set "$multus_iface" address "0a:${guest_mac#*:}" 2>/dev/null || true
        ip link set "$multus_iface" up 2>/dev/null || true
        echo "Primary NAD: re-MAC'd $multus_iface off the guest MAC $guest_mac (it would shadow the tap on $bridge)"
    fi

    ip link set "$multus_iface" master "$bridge"
    ip link set "$multus_iface" up

    # Best-effort helper IP in the NAD subnet for dnsmasq to bind. Heuristic
    # (.254 host) with a standing collision risk if the subnet already uses it;
    # documented in docs/networking/multi-node-l2.md.
    net_base=$(echo "$nad_ip" | sed 's/\.[0-9]*$//')
    helper="${net_base}.254"
    [ "$helper" = "$nad_ip" ] && helper="${net_base}.253"
    ip addr add "$helper/$nad_prefix" dev "$bridge" 2>/dev/null || true

    echo "Primary NIC on NAD: $bridge bridges $multus_iface to $tap; guest IP=$nad_ip/$nad_prefix gw=$nad_gw (helper $helper)"
}

# --- Main ---

# Check if intent has "nics" array (multi-NIC mode)
has_nics() {
    [ -f "$INTENT_PATH" ] || return 1
    grep -q '"nics"' "$INTENT_PATH" || return 1
    return 0
}

if has_nics; then
    # Multi-NIC mode: parse NIC list from intent JSON with python3.
    # FAIL LOUD if python3 is missing -- otherwise the python3 pipe below
    # produces no output, the per-NIC loop runs zero times, and the script
    # would falsely report success while configuring NO interfaces (the guest
    # then fails later at "No IP on br0"). python3 ships in the launcher image.
    if ! command -v python3 >/dev/null 2>&1; then
        echo "ERROR: python3 not found but multi-NIC intent present; cannot configure interfaces" >&2
        exit 1
    fi
    # Extract the nics array content
    NIC_COUNT=$(grep -o '"tapDevice"' "$INTENT_PATH" | wc -l)

    if [ "$NIC_COUNT" -eq 0 ]; then
        echo "nics array is empty, falling back to legacy mode"
        setup_primary_nic "br0" "tap0"
        setup_exposed_ports "br0"
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
    mac = nic.get('mac', '')
    print(f\"{nic_type}|{tap}|{bridge}|{primary}|{multus}|{mac}|{nic['name']}\")
" | while IFS='|' read -r nic_type tap bridge primary multus mac name; do
        if [ "$nic_type" = "sriov" ]; then
            echo "Skipping SR-IOV interface $name -- VFIO passthrough handled by swiftletd"
            continue
        fi
        if [ "$nic_type" = "vhost-user" ]; then
            # vhost-user-net: the datapath is an operator backend socket; there
            # is no in-pod tap/bridge to set up (and no multusInterface). Skip,
            # like SR-IOV -- swiftletd hands CH --net vhost_user=on,socket=.
            echo "Skipping vhost-user interface $name -- backend datapath handled by swiftletd"
            continue
        fi
        if [ "$primary" = "1" ] && [ -n "$multus" ]; then
            # Primary-on-NAD (multi-node L2): the primary rides a Multus NAD
            # instead of the node-local bridge.
            setup_primary_nad_nic "$bridge" "$tap" "$multus" "$mac"
        elif [ "$primary" = "1" ]; then
            setup_primary_nic "$bridge" "$tap"
            setup_exposed_ports "$bridge"
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
    setup_exposed_ports "$BRIDGE"
    echo "Network init done: $BRIDGE (internal) with $TAP"
fi
