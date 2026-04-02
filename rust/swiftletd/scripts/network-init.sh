#!/bin/sh
# Network init: create internal bridge br0 (do NOT add eth0), create tap0 for the VM.
# eth0 is the pod's uplink and must keep its IP for swiftletd to reach the Kubernetes API.
# br0 gets its own subnet; VM traffic is NATted out via eth0.
set -e

BRIDGE="${BRIDGE_NAME:-br0}"
TAP="${TAP_NAME:-tap0}"

# VM subnet (internal only; must not conflict with pod network)
BRIDGE_IP="${BRIDGE_IP:-10.244.125.1/24}"

# Create bridge (internal only — do NOT add eth0)
ip link add "$BRIDGE" type bridge stp_state 0
ip link set "$BRIDGE" up

# Give the bridge an IP — dnsmasq will bind here, and this is the VM's gateway
ip addr add "$BRIDGE_IP" dev "$BRIDGE"

# Create tap interface for the VM's virtio-net
ip tuntap add dev "$TAP" mode tap
ip link set "$TAP" up
ip link set "$TAP" master "$BRIDGE"

# Enable IP forwarding
echo 1 > /proc/sys/net/ipv4/ip_forward

# Masquerade VM traffic out through the pod's real interface
iptables -t nat -A POSTROUTING -s 10.244.125.0/24 ! -d 10.244.125.0/24 -j MASQUERADE

echo "Network init done: $BRIDGE (internal) with $TAP; $PRIMARY unchanged"