#!/bin/sh
# Network init: create bridge br0, add eth0 to br0, create tap0, add tap0 to br0.
# Run as init container with NET_ADMIN. Pod keeps connectivity via br0.
set -e

PRIMARY="${PRIMARY_IF:-eth0}"
BRIDGE="${BRIDGE_NAME:-br0}"
TAP="${TAP_NAME:-tap0}"

# Save eth0 address before moving to bridge
ADDR=$(ip -4 addr show "$PRIMARY" 2>/dev/null | grep 'inet ' | awk '{print $2}' | head -1)
[ -z "$ADDR" ] && { echo "No IPv4 on $PRIMARY"; exit 1; }

# Create bridge and tap
ip link add name "$BRIDGE" type bridge
ip link set "$BRIDGE" up
ip tuntap add dev "$TAP" mode tap
ip link set "$TAP" master "$BRIDGE"
ip link set "$TAP" up

# Move primary interface to bridge
ip addr flush dev "$PRIMARY"
ip link set "$PRIMARY" master "$BRIDGE"
ip addr add "$ADDR" dev "$BRIDGE"
ip link set "$PRIMARY" up

echo "Network init done: $BRIDGE with $PRIMARY and $TAP"
