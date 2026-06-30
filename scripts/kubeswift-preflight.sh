#!/usr/bin/env bash
#
# KubeSwift worker-node readiness preflight script
#
# Read-only host preflight for prospective KubeSwift worker nodes.
# Primary target: Ubuntu x86_64
#
# This script checks worker-node prerequisites only. It does NOT:
# - install packages
# - modify the host
# - join the node to a cluster
# - validate developer tooling like kubectl, Go, or Rust
#
# Exit codes:
#   0 - Ready (no hard failures; warnings may still be present)
#   1 - Not ready (one or more hard failures)
#   2 - Script misuse or unsupported invocation/environment
#
# Usage:
#   ./kubeswift-worker-preflight.sh
#   ./kubeswift-worker-preflight.sh --help
#
set -u -o pipefail

SCRIPT_NAME="$(basename "$0")"
PASS_COUNT=0
WARN_COUNT=0
FAIL_COUNT=0

PASS_ITEMS=()
WARN_ITEMS=()
FAIL_ITEMS=()

WARN_FIXES=()
FAIL_FIXES=()

MIN_KERNEL="4.11"
RECOMMENDED_KERNEL="5.15"

CPU_VENDOR_MODULE=""
OS_ID=""
OS_PRETTY_NAME=""
OS_VERSION_ID=""

usage() {
  cat <<EOF
Usage: $SCRIPT_NAME [--help]

KubeSwift worker-node readiness preflight for Linux hosts.
Primary target: Ubuntu x86_64.

This script performs read-only checks for:
- operating system and distribution
- architecture
- Linux kernel version
- CPU virtualization support
- KVM device and kernel modules
- cgroup v2
- swap status
- required networking modules/sysctls
- container runtime presence
- recommended capacity checks
- (optional) IOMMU and VFIO for GPU/SR-IOV passthrough
- (optional) SR-IOV VF availability
- (optional) hugepage allocation
- (optional) Multus CNI presence
- (optional) IOMMU and VFIO for GPU/SR-IOV passthrough
- (optional) SR-IOV VF availability
- (optional) hugepage allocation
- (optional) Multus CNI presence

Exit codes:
  0  Ready (no hard failures; warnings may still be present)
  1  Not ready (one or more hard failures)
  2  Script misuse or unsupported invocation/environment

Examples:
  ./$SCRIPT_NAME
  curl -fsSL https://raw.githubusercontent.com/kubeswift-io/kubeswift/main/scripts/$SCRIPT_NAME -o $SCRIPT_NAME
  chmod +x $SCRIPT_NAME
  ./$SCRIPT_NAME

Notes:
- This script does not modify the host.
- This script checks worker-node prerequisites only.
- Warnings indicate caveats or recommended improvements, but they do not change the exit code.
- For each WARN or FAIL, the script prints suggested permanent remediation commands where practical.
EOF
}

emit_pass() {
  local msg="$1"
  echo "[PASS] $msg"
  PASS_COUNT=$((PASS_COUNT + 1))
  PASS_ITEMS+=("$msg")
}

emit_warn() {
  local msg="$1"
  local remedy="${2:-}"
  echo "[WARN] $msg"
  WARN_COUNT=$((WARN_COUNT + 1))
  WARN_ITEMS+=("$msg")
  if [ -n "$remedy" ]; then
    WARN_FIXES+=("$msg|||$remedy")
  fi
}

emit_fail() {
  local msg="$1"
  local remedy="${2:-}"
  echo "[FAIL] $msg"
  FAIL_COUNT=$((FAIL_COUNT + 1))
  FAIL_ITEMS+=("$msg")
  if [ -n "$remedy" ]; then
    FAIL_FIXES+=("$msg|||$remedy")
  fi
}

command_exists() {
  command -v "$1" >/dev/null 2>&1
}

# Returns 0 if version $1 >= version $2, using major.minor comparison.
version_ge() {
  local a="${1%%-*}"
  local b="${2%%-*}"
  local a_major a_minor b_major b_minor

  a_major="$(printf '%s' "$a" | cut -d. -f1)"
  a_minor="$(printf '%s' "$a" | cut -d. -f2)"
  b_major="$(printf '%s' "$b" | cut -d. -f1)"
  b_minor="$(printf '%s' "$b" | cut -d. -f2)"

  a_minor="${a_minor:-0}"
  b_minor="${b_minor:-0}"

  [[ "$a_major" =~ ^[0-9]+$ ]] || return 1
  [[ "$a_minor" =~ ^[0-9]+$ ]] || a_minor=0
  [[ "$b_major" =~ ^[0-9]+$ ]] || return 1
  [[ "$b_minor" =~ ^[0-9]+$ ]] || b_minor=0

  if [ "$a_major" -gt "$b_major" ]; then
    return 0
  fi
  if [ "$a_major" -lt "$b_major" ]; then
    return 1
  fi
  [ "$a_minor" -ge "$b_minor" ]
}

kernel_major_minor() {
  uname -r | cut -d. -f1,2
}

detect_os_release() {
  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-}"
    OS_PRETTY_NAME="${PRETTY_NAME:-${ID:-unknown}}"
    OS_VERSION_ID="${VERSION_ID:-unknown}"
  fi
}

detect_cpu_vendor_module() {
  if grep -qE '(^|[[:space:]])vmx([[:space:]]|$)' /proc/cpuinfo 2>/dev/null; then
    CPU_VENDOR_MODULE="kvm_intel"
  elif grep -qE '(^|[[:space:]])svm([[:space:]]|$)' /proc/cpuinfo 2>/dev/null; then
    CPU_VENDOR_MODULE="kvm_amd"
  else
    CPU_VENDOR_MODULE="kvm_intel"
  fi
}

fix_upgrade_kernel() {
  cat <<'EOF'
# Ubuntu: install the generic kernel meta-package and reboot.
sudo apt update
sudo apt install -y linux-generic
sudo reboot
EOF
}

fix_enable_virtualization_bios() {
  cat <<'EOF'
# No safe Linux command can enable CPU virtualization if firmware has it disabled.
# You must enable Intel VT-x or AMD-V / SVM in BIOS or UEFI, then reboot.
# After reboot, re-run the preflight script.
EOF
}

fix_load_kvm_modules() {
  local vendor_module="$1"
  cat <<EOF
# Load KVM modules now.
sudo modprobe kvm
sudo modprobe ${vendor_module}

# Make them load permanently on boot.
cat <<'EOM' | sudo tee /etc/modules-load.d/kubeswift-kvm.conf >/dev/null
kvm
${vendor_module}
EOM

# Re-run the preflight script after reboot if /dev/kvm is still missing.
EOF
}

fix_dev_kvm_permissions() {
  cat <<'EOF'
# Ensure the current user can access /dev/kvm persistently.
sudo usermod -aG kvm "$USER"

# Then fully log out and log back in, or reboot.
# Verify again with:
#   ls -l /dev/kvm
#   id | grep kvm
EOF
}

fix_enable_cgroup_v2() {
  cat <<'EOF'
# Ubuntu: enable unified cgroup v2 and reboot.
sudo cp /etc/default/grub /etc/default/grub.bak

if grep -q '^GRUB_CMDLINE_LINUX=' /etc/default/grub; then
  sudo sed -i 's/^GRUB_CMDLINE_LINUX="/GRUB_CMDLINE_LINUX="systemd.unified_cgroup_hierarchy=1 /' /etc/default/grub
else
  echo 'GRUB_CMDLINE_LINUX="systemd.unified_cgroup_hierarchy=1"' | sudo tee -a /etc/default/grub >/dev/null
fi

sudo update-grub
sudo reboot
EOF
}

fix_disable_swap() {
  cat <<'EOF'
# Disable swap now.
sudo swapoff -a

# Disable swap permanently in /etc/fstab.
sudo cp /etc/fstab /etc/fstab.bak
sudo sed -ri '/\sswap\s/s/^#?/#/' /etc/fstab

# Confirm:
#   swapon --show
EOF
}

fix_load_network_modules() {
  cat <<'EOF'
# Load modules now.
sudo modprobe overlay
sudo modprobe br_netfilter

# Make them load permanently on boot.
cat <<'EOM' | sudo tee /etc/modules-load.d/kubeswift-k8s.conf >/dev/null
overlay
br_netfilter
EOM
EOF
}

fix_network_sysctls() {
  cat <<'EOF'
# Persist required Kubernetes/KubeSwift networking sysctls.
cat <<'EOM' | sudo tee /etc/sysctl.d/99-kubeswift-k8s.conf >/dev/null
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOM

sudo sysctl --system
EOF
}

fix_install_containerd() {
  cat <<'EOF'
# Install and enable containerd on Ubuntu.
sudo apt update
sudo apt install -y containerd

sudo mkdir -p /etc/containerd
sudo containerd config default | sudo tee /etc/containerd/config.toml >/dev/null
sudo sed -ri 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml

sudo systemctl enable --now containerd
sudo systemctl restart containerd
EOF
}

fix_containerd_systemdcgroup() {
  cat <<'EOF'
# Ensure containerd uses systemd cgroups.
sudo mkdir -p /etc/containerd

# Create a default config if one does not exist.
if [ ! -f /etc/containerd/config.toml ]; then
  sudo containerd config default | sudo tee /etc/containerd/config.toml >/dev/null
fi

sudo sed -ri 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
sudo systemctl restart containerd
EOF
}

fix_use_ubuntu() {
  cat <<'EOF'
# Recommended: use Ubuntu 22.04 LTS or 24.04 LTS for KubeSwift worker nodes.
# There is no universal in-place command that safely converts an arbitrary Linux
# distribution to Ubuntu. Provision a supported Ubuntu host and re-run the script.
EOF
}

fix_low_memory() {
  cat <<'EOF'
# Recommended: increase host memory to at least 16 GiB for smoke testing.
# There is no universal safe command to add RAM permanently from inside the OS.
# If this is a VM, increase assigned memory in the hypervisor settings and reboot.
EOF
}

fix_low_disk() {
  cat <<'EOF'
# Recommended: provide at least 100 GiB free disk for smoke testing.
# Permanent fixes depend on your storage layout:
# - add/expand the backing disk in your hypervisor or cloud
# - extend the partition/LVM/filesystem
# - or free space under /var or /
#
# Quick inspection:
df -h / /var
EOF
}

check_architecture() {
  local arch
  arch="$(uname -m 2>/dev/null || true)"

  if [ -z "$arch" ]; then
    emit_fail "Architecture: could not determine via uname -m" \
      "uname -m"
    return
  fi

  if [ "$arch" = "x86_64" ]; then
    emit_pass "Architecture: $arch"
  else
    emit_fail "Architecture: $arch (KubeSwift worker nodes currently require x86_64)" \
      "Provision or reinstall on an x86_64 host."
  fi
}

check_os() {
  local os
  os="$(uname -s 2>/dev/null || true)"

  if [ "$os" != "Linux" ]; then
    emit_fail "Operating system: ${os:-unknown} (Linux required)" \
      "Provision a Linux host. KubeSwift worker nodes are currently Linux-only."
    return
  fi

  emit_pass "Operating system: Linux"

  if [ -n "$OS_ID" ]; then
    if [ "$OS_ID" = "ubuntu" ]; then
      emit_pass "Distribution: Ubuntu ${OS_VERSION_ID:-unknown}"
    else
      emit_warn "Distribution: ${OS_PRETTY_NAME:-${OS_ID:-unknown}} (Ubuntu x86_64 is the primary tested target)" \
        "$(fix_use_ubuntu)"
    fi
  else
    emit_warn "Distribution: could not determine from /etc/os-release" \
      "Inspect /etc/os-release manually and prefer Ubuntu 22.04 or 24.04 LTS."
  fi
}

check_kernel() {
  local full short
  full="$(uname -r 2>/dev/null || true)"
  short="$(kernel_major_minor 2>/dev/null || true)"

  if [ -z "$full" ] || [ -z "$short" ]; then
    emit_fail "Kernel: could not determine version" \
      "uname -r"
    return
  fi

  if version_ge "$short" "$RECOMMENDED_KERNEL"; then
    emit_pass "Kernel: $full (meets recommended baseline >= $RECOMMENDED_KERNEL)"
  elif version_ge "$short" "$MIN_KERNEL"; then
    emit_warn "Kernel: $full (supported minimum met, but >= $RECOMMENDED_KERNEL is recommended)" \
      "$(fix_upgrade_kernel)"
  else
    emit_fail "Kernel: $full (below minimum required baseline $MIN_KERNEL)" \
      "$(fix_upgrade_kernel)"
  fi
}

check_virtualization() {
  local flags_count
  flags_count="$(grep -Eoc '(^|[[:space:]])(vmx|svm)([[:space:]]|$)' /proc/cpuinfo 2>/dev/null || true)"
  flags_count="${flags_count:-0}"

  if [ "$flags_count" -gt 0 ]; then
    emit_pass "CPU virtualization: vmx/svm detected"
  else
    emit_fail "CPU virtualization: vmx/svm not detected in /proc/cpuinfo (enable VT-x/AMD-V in firmware)" \
      "$(fix_enable_virtualization_bios)"
  fi
}

check_kvm_modules() {
  local have_kvm=0
  local have_vendor=0

  if [ -r /proc/modules ]; then
    grep -qE '^kvm[[:space:]]' /proc/modules 2>/dev/null && have_kvm=1
    grep -qE "^${CPU_VENDOR_MODULE}[[:space:]]" /proc/modules 2>/dev/null && have_vendor=1
  fi

  if [ "$have_kvm" -eq 1 ] && [ "$have_vendor" -eq 1 ]; then
    emit_pass "KVM modules: loaded (kvm and vendor module present)"
  elif [ "$have_kvm" -eq 1 ]; then
    emit_fail "KVM modules: kvm loaded but ${CPU_VENDOR_MODULE} missing" \
      "$(fix_load_kvm_modules "$CPU_VENDOR_MODULE")"
  else
    emit_fail "KVM modules: not loaded" \
      "$(fix_load_kvm_modules "$CPU_VENDOR_MODULE")"
  fi
}

check_dev_kvm() {
  if [ ! -e /dev/kvm ]; then
    emit_fail "/dev/kvm: missing" \
      "$(fix_load_kvm_modules "$CPU_VENDOR_MODULE")"
    return
  fi

  local mode owner group
  mode="$(stat -c '%a' /dev/kvm 2>/dev/null || echo '?')"
  owner="$(stat -c '%U' /dev/kvm 2>/dev/null || echo '?')"
  group="$(stat -c '%G' /dev/kvm 2>/dev/null || echo '?')"

  if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
    emit_fail "/dev/kvm: present but not readable/writable by current user (owner=$owner group=$group mode=$mode)" \
      "$(fix_dev_kvm_permissions)"
    return
  fi

  emit_pass "/dev/kvm: present and accessible (owner=$owner group=$group mode=$mode)"
}

check_cgroup_v2() {
  if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    emit_pass "cgroup: v2 unified hierarchy detected"
  else
    emit_fail "cgroup: v2 unified hierarchy not detected" \
      "$(fix_enable_cgroup_v2)"
  fi
}

check_swap() {
  local swap_lines
  swap_lines="$(awk 'NR>1 {count++} END {print count+0}' /proc/swaps 2>/dev/null || echo 0)"

  if [ "$swap_lines" -eq 0 ]; then
    emit_pass "Swap: disabled"
  else
    emit_fail "Swap: enabled" \
      "$(fix_disable_swap)"
  fi
}

check_module_loaded() {
  local module="$1"
  local label="$2"

  if [ -r /proc/modules ] && grep -qE "^${module}[[:space:]]" /proc/modules 2>/dev/null; then
    emit_pass "Kernel module: $label loaded"
  else
    emit_fail "Kernel module: $label not loaded" \
      "$(fix_load_network_modules)"
  fi
}

sysctl_value() {
  local key="$1"
  if command_exists sysctl; then
    sysctl -n "$key" 2>/dev/null || true
  fi
}

check_sysctl_equals() {
  local key="$1"
  local expected="$2"
  local description="$3"
  local val
  val="$(sysctl_value "$key")"

  if [ -z "$val" ]; then
    emit_fail "Sysctl: $key could not be read ($description)" \
      "$(fix_network_sysctls)"
  elif [ "$val" = "$expected" ]; then
    emit_pass "Sysctl: $key=$expected ($description)"
  else
    emit_fail "Sysctl: $key=$val (expected $expected; $description)" \
      "$(fix_network_sysctls)"
  fi
}

check_networking_prereqs() {
  check_module_loaded "overlay" "overlay"
  check_module_loaded "br_netfilter" "br_netfilter"

  check_sysctl_equals "net.ipv4.ip_forward" "1" "IPv4 forwarding"
  check_sysctl_equals "net.bridge.bridge-nf-call-iptables" "1" "bridge traffic to iptables"
  check_sysctl_equals "net.bridge.bridge-nf-call-ip6tables" "1" "bridge traffic to ip6tables"
}

check_container_runtime() {
  local found=0
  local runtime=""

  if command_exists systemctl; then
    if systemctl is-active --quiet containerd 2>/dev/null; then
      found=1
      runtime="containerd"
    elif systemctl is-active --quiet crio 2>/dev/null; then
      found=1
      runtime="crio"
    fi
  fi

  if [ "$found" -eq 0 ]; then
    if [ -S /run/containerd/containerd.sock ]; then
      found=1
      runtime="containerd"
    elif [ -S /var/run/crio/crio.sock ]; then
      found=1
      runtime="crio"
    fi
  fi

  if [ "$found" -eq 1 ]; then
    emit_pass "Container runtime: $runtime detected"
  else
    emit_fail "Container runtime: no supported runtime detected (containerd or CRI-O required)" \
      "$(fix_install_containerd)"
  fi
}

check_containerd_config_warning() {
  if ! command_exists containerd; then
    return
  fi

  if [ -r /etc/containerd/config.toml ]; then
    if grep -Eq 'SystemdCgroup[[:space:]]*=[[:space:]]*true' /etc/containerd/config.toml 2>/dev/null; then
      emit_pass "containerd config: SystemdCgroup=true detected"
    else
      emit_warn "containerd config: could not confirm SystemdCgroup=true" \
        "$(fix_containerd_systemdcgroup)"
    fi
  else
    emit_warn "containerd config: /etc/containerd/config.toml not readable; systemd cgroup setting not verified" \
      "$(fix_containerd_systemdcgroup)"
  fi
}

check_memory_warning() {
  local mem_kb mem_gb
  mem_kb="$(awk '/MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  mem_kb="${mem_kb:-0}"

  if [ "$mem_kb" -le 0 ]; then
    emit_warn "Memory: could not determine total RAM" \
      "grep MemTotal /proc/meminfo"
    return
  fi

  mem_gb=$((mem_kb / 1024 / 1024))

  if [ "$mem_gb" -ge 16 ]; then
    emit_pass "Memory: ${mem_gb} GiB detected"
  else
    emit_warn "Memory: ${mem_gb} GiB detected (16+ GiB recommended for smoke testing)" \
      "$(fix_low_memory)"
  fi
}

check_disk_warning() {
  local avail_kb avail_gb
  avail_kb="$(df -Pk /var 2>/dev/null | awk 'NR==2 {print $4}' || true)"

  if [ -z "$avail_kb" ]; then
    avail_kb="$(df -Pk / 2>/dev/null | awk 'NR==2 {print $4}' || true)"
  fi

  if [ -z "$avail_kb" ]; then
    emit_warn "Disk: could not determine available disk space" \
      "df -h / /var"
    return
  fi

  avail_gb=$((avail_kb / 1024 / 1024))

  if [ "$avail_gb" -ge 100 ]; then
    emit_pass "Disk: ${avail_gb} GiB available"
  else
    emit_warn "Disk: ${avail_gb} GiB available (100+ GiB recommended for smoke testing)" \
      "$(fix_low_disk)"
  fi
}

# ─── Optional capability checks (WARN-level, not FAIL) ────────────────────

check_iommu() {
  local cmdline
  cmdline="$(cat /proc/cmdline 2>/dev/null || true)"

  local iommu_param=0
  if echo "$cmdline" | grep -qE '(intel_iommu=on|amd_iommu=on)'; then
    iommu_param=1
  fi

  local iommu_active=0
  if dmesg 2>/dev/null | grep -qiE '(DMAR:.*IOMMU enabled|AMD-Vi.*loaded)'; then
    iommu_active=1
  fi

  if [ "$iommu_active" -eq 1 ]; then
    emit_pass "IOMMU: active (GPU/SR-IOV passthrough capable)"
  elif [ "$iommu_param" -eq 1 ]; then
    emit_warn "IOMMU: kernel parameter set but not confirmed active in dmesg (reboot may be needed)" \
      "Reboot and verify with: dmesg | grep -e DMAR -e IOMMU"
  else
    emit_warn "IOMMU: not enabled (required for GPU passthrough and SR-IOV; add intel_iommu=on or amd_iommu=on to kernel cmdline)" \
      "$(cat <<'EOF'
# Enable IOMMU (Intel example):
sudo sed -i 's/GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"/GRUB_CMDLINE_LINUX_DEFAULT="\1 intel_iommu=on iommu=pt"/' /etc/default/grub
sudo update-grub
sudo reboot
EOF
)"
  fi
}

check_vfio_modules() {
  local have_vfio=0 have_vfio_pci=0 have_iommu_type1=0

  if [ -r /proc/modules ]; then
    grep -qE '^vfio[[:space:]]' /proc/modules 2>/dev/null && have_vfio=1
    grep -qE '^vfio_pci[[:space:]]' /proc/modules 2>/dev/null && have_vfio_pci=1
    grep -qE '^vfio_iommu_type1[[:space:]]' /proc/modules 2>/dev/null && have_iommu_type1=1
  fi

  if [ "$have_vfio" -eq 1 ] && [ "$have_vfio_pci" -eq 1 ] && [ "$have_iommu_type1" -eq 1 ]; then
    emit_pass "VFIO modules: vfio, vfio_pci, vfio_iommu_type1 loaded"
  else
    local missing=""
    [ "$have_vfio" -eq 0 ] && missing="vfio "
    [ "$have_vfio_pci" -eq 0 ] && missing="${missing}vfio_pci "
    [ "$have_iommu_type1" -eq 0 ] && missing="${missing}vfio_iommu_type1 "
    emit_warn "VFIO modules: missing ${missing}(required for GPU/SR-IOV passthrough)" \
      "$(cat <<'EOF'
# Load VFIO modules:
sudo modprobe vfio vfio_iommu_type1 vfio_pci

# Make permanent:
cat <<'EOM' | sudo tee /etc/modules-load.d/kubeswift-vfio.conf
vfio
vfio_iommu_type1
vfio_pci
EOM
EOF
)"
  fi
}

check_sriov_vfs() {
  local vf_count=0
  vf_count="$(lspci 2>/dev/null | grep -ci 'Virtual Function' || true)"

  if [ "$vf_count" -gt 0 ]; then
    emit_pass "SR-IOV VFs: $vf_count VF(s) detected"
  else
    emit_warn "SR-IOV VFs: none detected (required for SR-IOV NIC passthrough)" \
      "$(cat <<'EOF'
# Create VFs on an SR-IOV NIC (example):
echo 8 | sudo tee /sys/class/net/<pf>/device/sriov_numvfs

# Verify:
lspci | grep "Virtual Function"

# See docs/networking/sriov.md for the full setup guide.
EOF
)"
  fi
}

check_hugepages() {
  local total free
  total="$(awk '/HugePages_Total:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  free="$(awk '/HugePages_Free:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"

  if [ "$total" -gt 0 ]; then
    emit_pass "Hugepages: ${total} total, ${free} free (1GiB pages)"
  else
    emit_warn "Hugepages: none allocated (required for GPU Tier 2/3 workloads)" \
      "$(cat <<'EOF'
# Allocate 1GiB hugepages (example: 400 pages = 400 GiB):
echo 400 | sudo tee /proc/sys/vm/nr_hugepages

# Make permanent:
echo "vm.nr_hugepages = 400" | sudo tee /etc/sysctl.d/99-kubeswift-hugepages.conf
sudo sysctl --system
EOF
)"
  fi
}

check_multus() {
  if ! command_exists kubectl; then
    emit_warn "Multus: cannot check (kubectl not found)"
    return
  fi

  local multus_pods
  multus_pods="$(kubectl get pods -A 2>/dev/null | grep -c multus || true)"

  if [ "$multus_pods" -gt 0 ]; then
    emit_pass "Multus CNI: $multus_pods pod(s) detected in cluster"
  else
    emit_warn "Multus CNI: not detected in cluster (required for multi-NIC and SR-IOV guests)" \
      "$(cat <<'EOF'
# Install Multus CNI:
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset-thick.yml

# See docs/multi-nic.md for multi-NIC setup.
EOF
)"
  fi
}

print_actions() {
  local title="$1"
  shift
  local -a items=("$@")
  local item msg remedy i

  if [ "${#items[@]}" -eq 0 ]; then
    return
  fi

  echo
  echo "$title"
  echo "$(printf '%*s' "${#title}" '' | tr ' ' '-')"

  i=1
  for item in "${items[@]}"; do
    msg="${item%%|||*}"
    remedy="${item#*|||}"
    echo
    echo "$i. $msg"
    echo "$remedy"
    i=$((i + 1))
  done
}

print_summary() {
  echo
  echo "--- Summary ---"
  echo "PASS: $PASS_COUNT"
  echo "WARN: $WARN_COUNT"
  echo "FAIL: $FAIL_COUNT"

  if [ "$WARN_COUNT" -gt 0 ]; then
    echo
    echo "Warnings:"
    local item
    for item in "${WARN_ITEMS[@]}"; do
      echo "  - $item"
    done
  fi

  if [ "$FAIL_COUNT" -gt 0 ]; then
    echo
    echo "Failures:"
    local item
    for item in "${FAIL_ITEMS[@]}"; do
      echo "  - $item"
    done
  fi

  print_actions "Recommended actions for FAIL items" "${FAIL_FIXES[@]}"
  print_actions "Recommended actions for WARN items" "${WARN_FIXES[@]}"

  echo
  if [ "$FAIL_COUNT" -gt 0 ]; then
    echo "Overall: NOT READY"
    echo "Exit code: 1"
  else
    echo "Overall: READY"
    if [ "$WARN_COUNT" -gt 0 ]; then
      echo "Warnings were found; review them before joining the node."
    fi
    echo "Exit code: 0"
  fi
}

main() {
  case "${1:-}" in
    "")
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unsupported argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac

  if ! command_exists uname; then
    echo "[FAIL] Cannot run: uname not found" >&2
    exit 2
  fi

  if [ "$(uname -s 2>/dev/null || true)" != "Linux" ]; then
    echo "[FAIL] Unsupported environment: Linux required" >&2
    exit 2
  fi

  detect_os_release
  detect_cpu_vendor_module

  echo "KubeSwift worker-node readiness preflight"
  echo "========================================"

  check_os
  check_architecture
  check_kernel
  check_virtualization
  check_kvm_modules
  check_dev_kvm
  check_cgroup_v2
  check_swap
  check_networking_prereqs
  check_container_runtime
  check_containerd_config_warning
  check_memory_warning
  check_disk_warning

  echo
  echo "--- Optional capabilities (GPU, SR-IOV, Multi-NIC) ---"
  check_iommu
  check_vfio_modules
  check_sriov_vfs
  check_hugepages
  check_multus

  print_summary

  if [ "$FAIL_COUNT" -gt 0 ]; then
    exit 1
  fi

  exit 0
}

main "$@"