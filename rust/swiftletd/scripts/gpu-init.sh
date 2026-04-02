#!/bin/bash
set -euo pipefail

# gpu-init.sh — runs as an init container before network-init and swiftletd.
# Binds GPU PCI devices to the vfio-pci driver so the VM can access them via
# VFIO passthrough. Optionally activates a Fabric Manager partition for shared
# NVSwitch mode (Tier 2 HGX workloads).
#
# Required environment variables:
#   GPU_PCI_ADDRESSES  — comma-separated BDF addresses (e.g. "0000:17:00.0,0000:3d:00.0")
#   GPU_PARTITION_ID   — Fabric Manager partition ID to activate, or -1 to skip

IFS=',' read -ra ADDRS <<< "${GPU_PCI_ADDRESSES}"

for addr in "${ADDRS[@]}"; do
  addr="${addr// /}"  # trim whitespace
  if [ -z "$addr" ]; then
    continue
  fi

  echo "Binding ${addr} to vfio-pci"

  # Unbind from the current driver (ignore errors — device may already be unbound).
  if [ -e "/sys/bus/pci/devices/${addr}/driver/unbind" ]; then
    echo "${addr}" > /sys/bus/pci/devices/"${addr}"/driver/unbind 2>/dev/null || true
  fi

  # Override to vfio-pci and probe.
  echo vfio-pci > /sys/bus/pci/devices/"${addr}"/driver_override
  echo "${addr}" > /sys/bus/pci/drivers_probe

  # Clear the override so it does not persist across future driver probes.
  echo > /sys/bus/pci/devices/"${addr}"/driver_override

  # Verify the binding succeeded.
  driver=$(basename "$(readlink /sys/bus/pci/devices/"${addr}"/driver 2>/dev/null)" 2>/dev/null || echo "none")
  if [ "$driver" != "vfio-pci" ]; then
    echo "ERROR: ${addr} bound to '${driver}', expected 'vfio-pci'"
    exit 1
  fi
  echo "${addr} bound to vfio-pci successfully"
done

# Activate Fabric Manager partition for shared NVSwitch mode (Tier 2).
GPU_PARTITION_ID="${GPU_PARTITION_ID:--1}"
if [ "${GPU_PARTITION_ID}" -ge 0 ]; then
  echo "Activating Fabric Manager partition ${GPU_PARTITION_ID}"
  fmpm -a "${GPU_PARTITION_ID}"
  echo "Partition ${GPU_PARTITION_ID} activated"
fi
