#!/bin/bash
set -euo pipefail

# gpu-init.sh -- runs as an init container before network-init and swiftletd.
# Binds GPU PCI devices to the vfio-pci driver so the VM can access them via
# VFIO passthrough. Also binds all IOMMU group peer devices (e.g., NVIDIA HD
# Audio controllers on consumer GPUs) since VFIO requires all devices in an
# IOMMU group to be isolated. Optionally activates a Fabric Manager partition
# for shared NVSwitch mode (Tier 2 HGX workloads).
#
# Required environment variables:
#   GPU_PCI_ADDRESSES  -- comma-separated BDF addresses (e.g. "0000:17:00.0,0000:3d:00.0")
#   GPU_PARTITION_ID   -- Fabric Manager partition ID to activate, or -1 to skip
#
# The full host sysfs is mounted at /host/sys (not /sys) to avoid being
# shadowed by the container runtime's read-only sysfs mount. Device symlinks
# under /sys/bus/pci/devices/ resolve to /sys/devices/ so the full tree is needed.
#
# IMPORTANT: readlink -f cannot be used on paths under $HOST_SYS because symlink
# targets are relative to the real /sys, not our $HOST_SYS mount. We use readlink
# (without -f) and resolve paths manually.
HOST_SYS="${HOST_SYS_PATH:-/host/sys}"
SYSFS_PCI="${HOST_SYS}/bus/pci"

echo "gpu-init: HOST_SYS=$HOST_SYS SYSFS_PCI=$SYSFS_PCI"
echo "gpu-init: GPU_PCI_ADDRESSES=${GPU_PCI_ADDRESSES:-}"

# Validate PCI BDF address format (DDDD:BB:DD.F) to prevent path traversal
# or injection via crafted addresses in the GPU_PCI_ADDRESSES env var.
validate_bdf() {
  local addr="$1"
  if ! echo "$addr" | grep -qE '^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$'; then
    echo "ERROR: invalid PCI address format: '$addr'"
    exit 1
  fi
}

# Resolve IOMMU group ID for a PCI device by reading the iommu_group symlink.
# Returns the group ID (e.g., "1") or empty string if not found.
get_iommu_group_id() {
  local addr="$1"
  local link_target
  link_target=$(readlink "${SYSFS_PCI}/devices/${addr}/iommu_group" 2>/dev/null || true)
  if [ -z "$link_target" ]; then
    echo ""
    return
  fi
  # link_target is relative like "../../../../kernel/iommu_groups/1"
  basename "$link_target"
}

# Bind a single PCI device to vfio-pci.
bind_to_vfio() {
  local addr="$1"
  local label="${2:-device}"

  validate_bdf "$addr"

  # Check if already bound to vfio-pci.
  local current_driver
  current_driver=$(basename "$(readlink ${SYSFS_PCI}/devices/${addr}/driver 2>/dev/null)" 2>/dev/null || echo "none")
  if [ "$current_driver" = "vfio-pci" ]; then
    echo "${addr} (${label}): already bound to vfio-pci"
    return 0
  fi

  echo "Binding ${addr} (${label}) to vfio-pci (current driver: ${current_driver})"

  # Unbind from the current driver (ignore errors -- device may already be unbound).
  if [ -e "${SYSFS_PCI}/devices/${addr}/driver/unbind" ]; then
    echo "${addr}" > "${SYSFS_PCI}/devices/${addr}/driver/unbind" 2>/dev/null || true
  fi

  # Override to vfio-pci and probe.
  echo vfio-pci > "${SYSFS_PCI}/devices/${addr}/driver_override"
  echo "${addr}" > "${SYSFS_PCI}/drivers_probe"

  # Clear the override so it does not persist across future driver probes.
  echo > "${SYSFS_PCI}/devices/${addr}/driver_override"

  # Verify the binding succeeded.
  local driver
  driver=$(basename "$(readlink ${SYSFS_PCI}/devices/${addr}/driver 2>/dev/null)" 2>/dev/null || echo "none")
  if [ "$driver" != "vfio-pci" ]; then
    echo "ERROR: ${addr} bound to '${driver}', expected 'vfio-pci'"
    exit 1
  fi
  echo "${addr} (${label}): bound to vfio-pci"
}

# Unbind a device from its current host driver so vfio-pci can claim it -- and
# so its IOMMU-group peers become bindable. Idempotent: no-op if the device is
# already unbound or already on vfio-pci.
unbind_from_host() {
  local addr="$1"
  if [ ! -e "${SYSFS_PCI}/devices/${addr}/driver" ]; then
    return 0
  fi
  local cur
  cur=$(basename "$(readlink ${SYSFS_PCI}/devices/${addr}/driver 2>/dev/null)" 2>/dev/null || echo "none")
  if [ "$cur" = "vfio-pci" ]; then
    return 0
  fi
  echo "Unbinding ${addr} from ${cur}"
  echo "${addr}" > "${SYSFS_PCI}/devices/${addr}/driver/unbind" 2>/dev/null || true
}

# Bind the GPU and every non-bridge device in its IOMMU group to vfio-pci.
#
# Uses the correct two-pass order: UNBIND all group devices from their host
# drivers FIRST, then bind them all to vfio-pci. vfio-pci's viability check
# refuses to bind any device while ANOTHER device in the same IOMMU group is
# still on a non-vfio host driver -- so the old one-pass order (bind peers,
# then the GPU) failed on hosts where the GPU is bound to nvidia/nouveau: the
# peer could not bind because the GPU was not yet isolated.
process_iommu_group() {
  local gpu_addr="$1"

  local group_id
  group_id=$(get_iommu_group_id "$gpu_addr")

  # Collect the non-bridge devices in the group (the GPU plus its peers).
  # Bridges (class 0x0604xx) are skipped -- VFIO handles them internally and
  # binding them would break the PCI hierarchy. Fall back to the GPU alone
  # when the group cannot be enumerated.
  local devices=""
  local group_devices="${HOST_SYS}/kernel/iommu_groups/${group_id}/devices"
  if [ -n "$group_id" ] && [ -d "$group_devices" ]; then
    echo "IOMMU group ${group_id}: scanning devices for ${gpu_addr}"
    local dev_link
    for dev_link in "$group_devices"/*; do
      local peer_addr
      peer_addr=$(basename "$dev_link")
      validate_bdf "$peer_addr"
      local pci_class
      pci_class=$(cat "${SYSFS_PCI}/devices/${peer_addr}/class" 2>/dev/null || echo "0x000000")
      case "$pci_class" in
        0x0604*)
          echo "${peer_addr}: PCIe bridge (class ${pci_class}) -- skipping (VFIO handles internally)"
          continue
          ;;
      esac
      devices="${devices} ${peer_addr}"
    done
  else
    echo "WARNING: IOMMU group for ${gpu_addr} not enumerable; binding the GPU alone"
    devices=" ${gpu_addr}"
  fi

  # Pass 1: unbind every group device from its host driver so the whole group
  # is isolatable.
  local d
  for d in $devices; do
    unbind_from_host "$d"
  done

  # Pass 2: bind every group device to vfio-pci. With the group now isolated,
  # vfio-pci's viability check passes regardless of order.
  for d in $devices; do
    if [ "$d" = "$gpu_addr" ]; then
      bind_to_vfio "$d" "GPU"
    else
      bind_to_vfio "$d" "IOMMU group ${group_id} peer"
    fi
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
  validate_bdf "$addr"

  # Dedup by IOMMU group: process_iommu_group binds the GPU AND all its group
  # peers together, so a second requested GPU sharing the same group is already
  # handled. (For multiple GPUs in one group, the first one binds them all.)
  local_group=$(get_iommu_group_id "$addr")
  if [ -n "$local_group" ]; then
    case " $PROCESSED_GROUPS " in
      *" $local_group "*)
        echo "IOMMU group ${local_group}: already processed"
        continue
        ;;
    esac
    PROCESSED_GROUPS="$PROCESSED_GROUPS $local_group"
  fi

  process_iommu_group "$addr"
done

# Activate Fabric Manager partition for shared NVSwitch mode (Tier 2).
GPU_PARTITION_ID="${GPU_PARTITION_ID:--1}"
if [ "${GPU_PARTITION_ID}" -ge 0 ]; then
  echo "Activating Fabric Manager partition ${GPU_PARTITION_ID}"
  fmpm -a "${GPU_PARTITION_ID}"
  echo "Partition ${GPU_PARTITION_ID} activated"
fi

echo "gpu-init: complete"
