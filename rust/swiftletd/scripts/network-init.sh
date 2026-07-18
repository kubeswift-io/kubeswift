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

# apply_restricted_egress hardens a sandbox guest's outbound traffic (untrusted
# code). Gated on KUBESWIFT_SANDBOX_EGRESS=restricted (set only by the SwiftSandbox
# pod builder -- SwiftGuests never set it, so their egress is untouched).
#
# The guest's traffic is FORWARDed (VM -> br0 -> eth0, MASQUERADEd), so a FORWARD
# filter is the precise, CNI-independent hard control (a NetworkPolicy is only
# enforced if the CNI supports egress policy -- silent no-op otherwise). Rules live
# in a dedicated chain jumped to from the TOP of FORWARD (before any CNI ACCEPT) for
# VM-sourced traffic only:
#   - ESTABLISHED/RELATED return traffic and DNS to the cluster resolver(s) RETURN
#     (allowed) -- the guest queries kube-dns directly (dnsmasq hands it as the
#     resolver), so :53 must be punched through the cluster block or DNS breaks;
#   - 169.254.0.0/16 (cloud metadata -> node creds) and RFC1918 (pod + service
#     CIDRs -> lateral movement) DROP;
#   - everything else (public internet) falls through -> ACCEPT -> MASQUERADE.
# Node public IPs are NOT blocked (same exposure as any pod); documented.
apply_restricted_egress() {
    src_net="$1"
    [ "${KUBESWIFT_SANDBOX_EGRESS:-}" = "restricted" ] || return 0
    chain="KUBESWIFT_SBX_EGRESS"
    iptables -N "$chain" 2>/dev/null || iptables -F "$chain"
    iptables -A "$chain" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
    for d in $(grep '^nameserver ' /etc/resolv.conf 2>/dev/null | awk '{print $2}'); do
        iptables -A "$chain" -d "$d" -p udp --dport 53 -j RETURN
        iptables -A "$chain" -d "$d" -p tcp --dport 53 -j RETURN
    done
    iptables -A "$chain" -d 169.254.0.0/16 -j DROP
    iptables -A "$chain" -d 10.0.0.0/8     -j DROP
    iptables -A "$chain" -d 172.16.0.0/12  -j DROP
    iptables -A "$chain" -d 192.168.0.0/16 -j DROP
    # Idempotent jump at the top of FORWARD for the VM subnet.
    iptables -C FORWARD -s "$src_net" -j "$chain" 2>/dev/null \
        || iptables -I FORWARD 1 -s "$src_net" -j "$chain"
    echo "Restricted egress: $src_net -> DNS + internet; DROP 169.254/16 + RFC1918 (chain $chain)"
}

setup_primary_nic() {
    local bridge="$1" tap="$2"

    # VM-internal subnet (per launcher-pod network namespace; must NOT
    # collide with the cluster pod-CIDR pool or any per-node Calico
    # allocation, otherwise SYN-ACK replies to cross-node traffic
    # routed via this br0 (linkdown) instead of eth0.
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

    # Sandbox restricted-egress hardening (no-op unless KUBESWIFT_SANDBOX_EGRESS=restricted).
    apply_restricted_egress "$bridge_net"

    echo "Primary NIC: $bridge ($bridge_ip, net $bridge_net) with $tap"
}

# setup_exposed_ports installs the service-exposure DNAT for the primary
# (nat-bound) NIC. For each intent.ports[] entry it adds
#   PREROUTING -p <proto> --dport <port> -j DNAT --to <vmIP>:<targetPort>
# where <vmIP> is the pinned primary VM IP (.10 of the bridge subnet, matching
# the launcher entrypoint's dnsmasq range_start). It hands that pinned IP to the
# entrypoint via expose.env so dnsmasq serves a single-address lease (the IP is
# then deterministic, so the DNAT target is correct -- no silent failure).
# No-op when intent has no ports.
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

# setup_secondary_nad_nic -- a SECONDARY interface that rides a Multus NAD (e.g. an
# OVN-Kubernetes secondary UDN) and must carry a routable IP INTO the guest. The
# mirror of setup_primary_nad_nic for a non-primary NIC: without it a secondary NAD
# interface is only bridged (setup_secondary_nic), so the guest never gets the NAD's
# IP and the NIC is unusable (e.g. as a routable node-datapath interface).
#
# Same read->flush->re-MAC->bridge->serve contract as the primary path, with two
# differences: the IP is handed via a SECOND dnsmasq (secondary-nad.env, driven by
# the launcher entrypoint) so the primary NIC's lease is untouched; and NO default
# route is handed (the primary interface keeps the default route -- the guest gets
# only the on-link route to the NAD subnet).
setup_secondary_nad_nic() {
    bridge="$1"; tap="$2"; multus_iface="$3"; guest_mac="$4"

    attempts=0
    while [ $attempts -lt 30 ]; do
        ip link show "$multus_iface" >/dev/null 2>&1 && break
        sleep 1; attempts=$((attempts + 1))
    done
    if ! ip link show "$multus_iface" >/dev/null 2>&1; then
        echo "ERROR: secondary NAD interface $multus_iface not found after 30s" >&2
        exit 1
    fi

    nad_cidr=$(ip -4 addr show "$multus_iface" 2>/dev/null | awk '/inet /{print $2; exit}')
    if [ -z "$nad_cidr" ]; then
        echo "ERROR: secondary NAD interface $multus_iface has no IPv4 address (NAD IPAM did not assign one)" >&2
        exit 1
    fi
    nad_ip="${nad_cidr%/*}"
    nad_prefix="${nad_cidr#*/}"
    nad_mtu=$(ip -o link show "$multus_iface" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="mtu"){print $(i+1); exit}}')

    rd=$(guest_run_dir); mkdir -p "$rd"
    {
        echo "SEC_IP=$nad_ip"
        echo "SEC_PREFIX=$nad_prefix"
        echo "SEC_MAC=$guest_mac"
        echo "SEC_BRIDGE=$bridge"
        echo "SEC_MTU=$nad_mtu"
    } > "$rd/secondary-nad.env"

    # The guest claims nad_ip; the host must not also hold it on the L2.
    ip addr flush dev "$multus_iface" 2>/dev/null || true

    ip link add "$bridge" type bridge stp_state 0 2>/dev/null || true
    ip link set "$bridge" up
    ip tuntap add dev "$tap" mode tap 2>/dev/null || true
    ip link set "$tap" up
    ip link set "$tap" master "$bridge"

    # Re-MAC the NIC off the guest MAC before enslaving (same reason as the primary
    # path: when the OVN LSP is stamped with the guest MAC, an enslaved NIC carrying
    # that same MAC adds a permanent fdb entry that shadows the guest's tap). No-op
    # when the NIC's IPAM gave it a distinct MAC (plain-bridge / VXLAN-mesh path).
    nic_mac=$(cat "/sys/class/net/$multus_iface/address" 2>/dev/null)
    if [ -n "$guest_mac" ] && [ "$nic_mac" = "$guest_mac" ]; then
        ip link set "$multus_iface" down 2>/dev/null || true
        ip link set "$multus_iface" address "0a:${guest_mac#*:}" 2>/dev/null || true
        ip link set "$multus_iface" up 2>/dev/null || true
        echo "Secondary NAD: re-MAC'd $multus_iface off the guest MAC $guest_mac"
    fi

    ip link set "$multus_iface" master "$bridge"
    ip link set "$multus_iface" up

    # Helper IP for the secondary dnsmasq to bind (same .254 heuristic as primary).
    net_base=$(echo "$nad_ip" | sed 's/\.[0-9]*$//')
    helper="${net_base}.254"
    [ "$helper" = "$nad_ip" ] && helper="${net_base}.253"
    ip addr add "$helper/$nad_prefix" dev "$bridge" 2>/dev/null || true

    echo "Secondary NIC on NAD: $bridge bridges $multus_iface to $tap; guest IP=$nad_ip/$nad_prefix (helper $helper, no default route)"
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

# setup_primary_udn_nic -- multi-node L2, guest on the namespace PRIMARY OVN-K UDN
# (Model A). OVN-Kubernetes attaches the pod
# to its namespace primary UserDefinedNetwork as `ovn-udn1` (e.g. 10.50.0.10/16)
# automatically, driven by the namespace label; eth0 stays on the cluster default
# (role infrastructure-locked) for the swiftletd->apiserver control path + egress.
#
# Same KubeVirt bridge-binding as setup_primary_nad_nic, with ONE Model-A specific:
# the OVN logical-switch-port PINS MAC+IP in port_security, and that MAC is IP-DERIVED
# and immutable (0a:58:<ip-octets-hex>). The guest CANNOT present its own 52:54:.. MAC
# -- OVN drops it. So the guest ADOPTS OVN's MAC: capture it here, hand it to swiftletd
# (UDN_MAC -> KUBESWIFT_PRIMARY_UDN_MAC via the entrypoint) for CH `--net mac=`, and the
# launcher's fixed-lease dnsmasq hands the captured IP to that MAC. (Controller-side
# MAC stamping -- the kube-ovn primary-NAD trick -- is impossible: a primary UDN has no
# Multus selection element to carry a requested MAC.)
#
#   1. wait for ovn-udn1; read its OVN-assigned MAC + IP + gw + MTU
#   2. persist them (primary-udn.env) for the launcher entrypoint (dnsmasq fixed lease
#      + the guest-MAC override)
#   3. flush the IP off ovn-udn1 (the GUEST owns it; avoids a duplicate-address ARP
#      conflict on the UDN)
#   4. re-MAC ovn-udn1 to a DISTINCT dummy BEFORE enslaving -- the guest uses the SAME
#      0a:58:.. MAC, which would otherwise add a permanent fdb entry shadowing the tap.
#      The 0a:fe:<rest> dummy differs from the 0a:58:<rest> guest MAC (the #240
#      0a:<rest> derivation is a no-op for a 0a:.. MAC).
#   5. bridge ovn-udn1 to the guest's tap; give br a helper IP for dnsmasq to bind
setup_primary_udn_nic() {
    bridge="$1"; tap="$2"; udn_iface="$3"

    attempts=0
    while [ $attempts -lt 30 ]; do
        ip link show "$udn_iface" >/dev/null 2>&1 && break
        sleep 1; attempts=$((attempts + 1))
    done
    if ! ip link show "$udn_iface" >/dev/null 2>&1; then
        echo "ERROR: primary UDN interface $udn_iface not found after 30s (is the namespace primary-UDN label set?)" >&2
        exit 1
    fi

    udn_cidr=$(ip -4 addr show "$udn_iface" 2>/dev/null | awk '/inet /{print $2; exit}')
    if [ -z "$udn_cidr" ]; then
        echo "ERROR: primary UDN interface $udn_iface has no IPv4 address (OVN-K IPAM did not assign one)" >&2
        exit 1
    fi
    udn_ip="${udn_cidr%/*}"
    udn_prefix="${udn_cidr#*/}"
    udn_mac=$(cat "/sys/class/net/$udn_iface/address" 2>/dev/null)
    if [ -z "$udn_mac" ]; then
        echo "ERROR: primary UDN interface $udn_iface has no MAC" >&2
        exit 1
    fi
    udn_gw=$(ip route show default 2>/dev/null | awk -v d="$udn_iface" '$0 ~ ("dev "d){print $3; exit}')
    [ -z "$udn_gw" ] && udn_gw=$(ip route show dev "$udn_iface" 2>/dev/null | awk '/default/{print $3; exit}')
    udn_mtu=$(ip -o link show "$udn_iface" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="mtu"){print $(i+1); exit}}')

    # Persist for the launcher entrypoint (shared run dir). The guest must adopt
    # UDN_MAC (OVN port_security), so the entrypoint exports it as
    # KUBESWIFT_PRIMARY_UDN_MAC for swiftletd's CH --net mac=.
    rd=$(guest_run_dir); mkdir -p "$rd"
    {
        echo "UDN_IP=$udn_ip"
        echo "UDN_PREFIX=$udn_prefix"
        echo "UDN_GW=$udn_gw"
        echo "UDN_MAC=$udn_mac"
        echo "UDN_BRIDGE=$bridge"
        echo "UDN_MTU=$udn_mtu"
    } > "$rd/primary-udn.env"

    # The guest claims udn_ip; the host must not also hold it on the UDN.
    ip addr flush dev "$udn_iface" 2>/dev/null || true

    ip link add "$bridge" type bridge stp_state 0 2>/dev/null || true
    ip link set "$bridge" up
    ip tuntap add dev "$tap" mode tap 2>/dev/null || true
    ip link set "$tap" up
    ip link set "$tap" master "$bridge"

    # The guest adopts OVN's MAC (udn_mac), so the pod NIC's kernel MAC (== udn_mac)
    # would add a permanent fdb entry <udn_mac> -> ovn-udn1 that SHADOWS the guest's
    # tap (return traffic + unicast DHCP go to the NIC, not the guest). Re-MAC the NIC
    # to a DISTINCT dummy before enslaving (KubeVirt bridge-binding). The OVN LSP keeps
    # udn_mac, so OVN still delivers the guest's frames NIC -> bridge -> tap. Per-pod
    # netns, so the dummy need only differ from udn_mac within this pod.
    dummy="0a:fe:${udn_mac#*:*:}"
    ip link set "$udn_iface" down 2>/dev/null || true
    ip link set "$udn_iface" address "$dummy" 2>/dev/null || true
    ip link set "$udn_iface" up 2>/dev/null || true
    echo "Primary UDN: re-MAC'd $udn_iface $udn_mac -> $dummy (it would shadow the tap on $bridge); guest adopts $udn_mac"

    ip link set "$udn_iface" master "$bridge"
    ip link set "$udn_iface" up

    # Best-effort helper IP in the UDN subnet for dnsmasq to bind (same .254 heuristic
    # as the NAD path; the guest talks via its UDN IP + gateway, never the helper).
    net_base=$(echo "$udn_ip" | sed 's/\.[0-9]*$//')
    helper="${net_base}.254"
    [ "$helper" = "$udn_ip" ] && helper="${net_base}.253"
    ip addr add "$helper/$udn_prefix" dev "$bridge" 2>/dev/null || true

    echo "Primary NIC on UDN: $bridge bridges $udn_iface to $tap; guest IP=$udn_ip/$udn_prefix mac=$udn_mac gw=$udn_gw (helper $helper)"
}

# --- Main ---

# Check if intent has "nics" array (multi-NIC mode)
has_nics() {
    [ -f "$INTENT_PATH" ] || return 1
    grep -q '"nics"' "$INTENT_PATH" || return 1
    return 0
}

# Model A: the top-level primaryUDNInterface
# signal (ovn-udn1) applies to the PRIMARY NIC -- whether the guest has explicit
# interfaces or the default single NIC -- so it is read once here, not per-NIC. Empty
# for every other networking mode.
PRIMARY_UDN_IFACE=$(sed -n 's/.*"primaryUDNInterface"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$INTENT_PATH" 2>/dev/null | head -1)

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
        elif [ "$primary" = "1" ] && [ -n "$PRIMARY_UDN_IFACE" ]; then
            # Model A: the primary rides the namespace primary OVN-K UDN (ovn-udn1).
            # The resolver only sets the signal for a node-local primary, so a
            # primary-on-NAD guest takes the branch above, never this one.
            setup_primary_udn_nic "$bridge" "$tap" "$PRIMARY_UDN_IFACE"
        elif [ "$primary" = "1" ]; then
            setup_primary_nic "$bridge" "$tap"
            setup_exposed_ports "$bridge"
        elif [ -n "$multus" ]; then
            # Secondary-on-NAD: the NIC rides a Multus NAD and must carry the NAD's
            # IP into the guest (e.g. a routable node-datapath interface). Hand it
            # over the same way the primary-on-NAD path does, minus the default route.
            setup_secondary_nad_nic "$bridge" "$tap" "$multus" "$mac"
        else
            setup_secondary_nic "$bridge" "$tap" "$multus"
        fi
    done

    echo "Network init done: $NIC_COUNT NIC(s) configured"
else
    # Legacy mode: single NIC (backward compatible)
    BRIDGE="${BRIDGE_NAME:-br0}"
    TAP="${TAP_NAME:-tap0}"
    if [ -n "$PRIMARY_UDN_IFACE" ]; then
        # Model A: the default single NIC rides the namespace primary OVN-K UDN
        # (ovn-udn1). This is the common Model A case -- a plain SwiftGuest with no
        # spec.interfaces in a primary-UDN namespace.
        setup_primary_udn_nic "$BRIDGE" "$TAP" "$PRIMARY_UDN_IFACE"
        echo "Network init done: $BRIDGE bridges $PRIMARY_UDN_IFACE (primary UDN) to $TAP"
    else
        setup_primary_nic "$BRIDGE" "$TAP"
        setup_exposed_ports "$BRIDGE"
        echo "Network init done: $BRIDGE (internal) with $TAP"
    fi
fi
