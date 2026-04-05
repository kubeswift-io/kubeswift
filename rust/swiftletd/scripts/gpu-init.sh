#!/bin/bash
set -euo pipefail

# gpu-init.sh — runs as an init container before network-init and swiftletd.
# Binds GPU PCI devices to the vfio-pci driver so the VM can access them via
# VFIO passthrough. Also binds all IOMMU group peer devices (e.g., NVIDIA HD
# Audio controllers on consumer GPUs) since VFIO requires all devices in an
# IOMMU group to be isolated. Optionally activates a Fabric Manager partition
# for shared NVSwitch mode (Tier 2 HGX workloads).
#
# Required environment variables:
#   GPU_PCI_ADDRESSES  — comma-separated BDF addresses (e.g. "0000:17:00.0,0000:3d:00.0")
#   GPU_PARTITION_ID   — Fabric Manager partition ID to activate, or -1 to skip
#
# The full host sysfs is mounted at /host/sys (not /sys) to avoid being
# shadowed by the container runtime's read-only sysfs mount. Device symlinks
# under /sys/bus/pci/devices/ resolve to /sys/devices/ so the full tree is needed.
SYSFS_PCI="${SYSFS_PCI_PATH:-/host/sys/bus/pci}"

# Validate PCI BDF address format (DDDD:BB:DD.F) to prevent path traversal
# or injection via crafted addresses in the GPU_PCI_ADDRESSES env var.
validate_bdf() {
  local addr="$1"
  if ! echo "$addr" | grep -qE '^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$'; then
    echo "ERROR: invalid PCI address format: '$addr'"
    exit 1
  fi
}

# Bind a single PCI device to vfio-pci.
bind_to_vfio() {
  local addr="$1"
  local label="${2:-device}"

  validate_bdf "$addr"

  # Check if already bound to vfio-pci.
  local current_driver
  current_driver=$(basename "$(readlink ${SYSFS_PCI}/devices/"${addr}"/driver 2>/dev/null)" 2>/dev/null || echo "none")
  if [ "$current_driver" = "vfio-pci" ]; then
    echo "${addr} (${label}): already bound to vfio-pci"
    return 0
  fi

  echo "Binding ${addr} (${label}) to vfio-pci"

  # Unbind from the current driver (ignore errors — device may already be unbound).
  if [ -e "${SYSFS_PCI}/devices/${addr}/driver/unbind" ]; then
    echo "${addr}" > ${SYSFS_PCI}/devices/"${addr}"/driver/unbind 2>/dev/null || true
  fi

  # Override to vfio-pci and probe.
  echo vfio-pci > ${SYSFS_PCI}/devices/"${addr}"/driver_override
  echo "${addr}" > ${SYSFS_PCI}/drivers_probe

  # Clear the override so it does not persist across future driver probes.
  echo > ${SYSFS_PCI}/devices/"${addr}"/driver_override

  # Verify the binding succeeded.
  local driver
  driver=$(basename "$(readlink ${SYSFS_PCI}/devices/"${addr}"/driver 2>/dev/null)" 2>/dev/null || echo "none")
  if [ "$driver" != "vfio-pci" ]; then
    echo "ERROR: ${addr} bound to '${driver}', expected 'vfio-pci'"
    exit 1
  fi
  echo "${addr} (${label}): bound to vfio-pci"
}

# Bind all non-bridge devices in the same IOMMU group as the given GPU.
# VFIO requires all devices in an IOMMU group to be either:
#   - bound to vfio-pci, or
#   - not bound to any driver (unbound)
#   - PCI/PCIe bridges (class 0x0604xx) are handled by VFIO internally
#
# Consumer NVIDIA GPUs (GTX, RTX) typically share their IOMMU group with
# a companion HD Audio controller. HGX GPUs may share with NVSwitch or
# other devices. This function handles all cases.
bind_iommu_group_peers() {
  local gpu_addr="$1"

  # Find the IOMMU group for this device.
  local iommu_group_path
  iommu_group_path=$(readlink -f ${SYSFS_PCI}/devices/"${gpu_addr}"/iommu_group 2>/dev/null || true)
  if [ -z "$iommu_group_path" ] || [ ! -d "$iommu_group_path/devices" ]; then
    echo "WARNING: could not determine IOMMU group for ${gpu_addr}"
    return 0
  fi

  local group_id
  group_id=$(basename "$iommu_group_path")
  echo "IOMMU group ${group_id}: scanning peer devices for ${gpu_addr}"

  for dev_link in "$iommu_group_path"/devices/*; do
    local peer_addr
    peer_addr=$(basename "$dev_link")

    # Skip the GPU itself — we handle it in the main loop.
    if [ "$peer_addr" = "$gpu_addr" ]; then
      continue
    fi

    validate_bdf "$peer_addr"

    # Read the PCI class to determine device type.
    local pci_class
    pci_class=$(cat ${SYSFS_PCI}/devices/"${peer_addr}"/class 2>/dev/null || echo "0x000000")

    # Skip PCI/PCIe bridges (class 0x0604xx). VFIO handles bridges internally
    # via the pci-stub or vfio-pci bridge support. Binding them to vfio-pci
    # would break the PCI hierarchy.
    case "$pci_class" in
      0x0604*)
        echo "${peer_addr}: PCIe bridge (class ${pci_class}) — skipping (VFIO handles internally)"
        continue
        ;;
    esac

    # Bind the peer device to vfio-pci.
    local peer_label="IOMMU group ${group_id} peer"
    bind_to_vfio "$peer_addr" "$peer_label"
  done
}

# Track which IOMMU groups we've already processed to avoid duplicate work
# when multiple GPUs share the same group.
PROCESSED_GROUPS=""

IFS=',' read -ra ADDRS <<< "${GPU_PCI_ADDRESSES}"

for addr in "${ADDRS[@]}"; do
  addr="${addr// /}"  # trim whitespace
  if [ -z "$addr" ]; then
    continue
  fi

  # Bind IOMMU group peers first — they must be isolated before VFIO will
  # grant access to the GPU.
  local_group=$(basename "$(readlink -f ${SYSFS_PCI}/devices/"${addr}"/iommu_group 2>/dev/null)" 2>/dev/null || echo "")
  if [ -n "$local_group" ]; then
    case " $PROCESSED_GROUPS " in
      *" $local_group "*)
        echo "IOMMU group ${local_group}: already processed"
        ;;
      *)
        bind_iommu_group_peers "$addr"
        PROCESSED_GROUPS="$PROCESSED_GROUPS $local_group"
        ;;
    esac
  fi

  # Now bind the GPU itself.
  bind_to_vfio "$addr" "GPU"
done

# Activate Fabric Manager partition for shared NVSwitch mode (Tier 2).
GPU_PARTITION_ID="${GPU_PARTITION_ID:--1}"
if [ "${GPU_PARTITION_ID}" -ge 0 ]; then
  echo "Activating Fabric Manager partition ${GPU_PARTITION_ID}"
  fmpm -a "${GPU_PARTITION_ID}"
  echo "Partition ${GPU_PARTITION_ID} activated"
fi
