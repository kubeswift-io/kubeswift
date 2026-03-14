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

MIN_KERNEL="4.11"
RECOMMENDED_KERNEL="5.15"

usage() {
  cat <<EOF
Usage: $SCRIPT_NAME [--help]

KubeSwift worker-node readiness preflight for Linux hosts.
Primary target: Ubuntu x86_64.

This script performs read-only checks for:
- architecture
- Linux kernel version
- CPU virtualization support
- KVM device and kernel modules
- cgroup v2
- swap status
- required networking modules/sysctls
- container runtime presence
- basic host suitability for KubeSwift worker-node use

Exit codes:
  0  Ready (no hard failures; warnings may still be present)
  1  Not ready (one or more hard failures)
  2  Script misuse or unsupported invocation/environment

Examples:
  ./$SCRIPT_NAME
  curl -fsSL https://raw.githubusercontent.com/projectbeskar/kubeswift/main/scripts/$SCRIPT_NAME -o $SCRIPT_NAME
  chmod +x $SCRIPT_NAME
  ./$SCRIPT_NAME

Notes:
- This script does not modify the host.
- This script checks worker-node prerequisites only.
- Warnings indicate caveats or recommended improvements, but they do not change the exit code.
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
  echo "[WARN] $msg"
  WARN_COUNT=$((WARN_COUNT + 1))
  WARN_ITEMS+=("$msg")
}

emit_fail() {
  local msg="$1"
  echo "[FAIL] $msg"
  FAIL_COUNT=$((FAIL_COUNT + 1))
  FAIL_ITEMS+=("$msg")
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

check_architecture() {
  local arch
  arch="$(uname -m 2>/dev/null || true)"

  if [ -z "$arch" ]; then
    emit_fail "Architecture: could not determine via uname -m"
    return
  fi

  if [ "$arch" = "x86_64" ]; then
    emit_pass "Architecture: $arch"
  else
    emit_fail "Architecture: $arch (KubeSwift worker nodes currently require x86_64)"
  fi
}

check_os() {
  local os
  os="$(uname -s 2>/dev/null || true)"

  if [ "$os" != "Linux" ]; then
    emit_fail "Operating system: ${os:-unknown} (Linux required)"
    return
  fi

  emit_pass "Operating system: Linux"

  if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    if [ "${ID:-}" = "ubuntu" ]; then
      emit_pass "Distribution: Ubuntu ${VERSION_ID:-unknown}"
    else
      emit_warn "Distribution: ${PRETTY_NAME:-${ID:-unknown}} (Ubuntu x86_64 is the primary tested target)"
    fi
  else
    emit_warn "Distribution: could not determine from /etc/os-release"
  fi
}

check_kernel() {
  local full short
  full="$(uname -r 2>/dev/null || true)"
  short="$(kernel_major_minor 2>/dev/null || true)"

  if [ -z "$full" ] || [ -z "$short" ]; then
    emit_fail "Kernel: could not determine version"
    return
  fi

  if version_ge "$short" "$RECOMMENDED_KERNEL"; then
    emit_pass "Kernel: $full (meets recommended baseline >= $RECOMMENDED_KERNEL)"
  elif version_ge "$short" "$MIN_KERNEL"; then
    emit_warn "Kernel: $full (supported minimum met, but >= $RECOMMENDED_KERNEL is recommended)"
  else
    emit_fail "Kernel: $full (below minimum required baseline $MIN_KERNEL)"
  fi
}

check_virtualization() {
  local flags_count
  flags_count="$(grep -Eoc '(^|[[:space:]])(vmx|svm)([[:space:]]|$)' /proc/cpuinfo 2>/dev/null || true)"
  flags_count="${flags_count:-0}"

  if [ "$flags_count" -gt 0 ]; then
    emit_pass "CPU virtualization: vmx/svm detected"
  else
    emit_fail "CPU virtualization: vmx/svm not detected in /proc/cpuinfo (enable VT-x/AMD-V in firmware)"
  fi
}

check_kvm_modules() {
  local have_kvm=0
  local have_vendor=0

  if [ -r /proc/modules ]; then
    grep -qE '^kvm[[:space:]]' /proc/modules 2>/dev/null && have_kvm=1
    grep -qE '^kvm_(intel|amd)[[:space:]]' /proc/modules 2>/dev/null && have_vendor=1
  fi

  if [ "$have_kvm" -eq 1 ] && [ "$have_vendor" -eq 1 ]; then
    emit_pass "KVM modules: loaded (kvm and vendor module present)"
  elif [ "$have_kvm" -eq 1 ]; then
    emit_fail "KVM modules: kvm loaded but kvm_intel/kvm_amd missing"
  else
    emit_fail "KVM modules: not loaded"
  fi
}

check_dev_kvm() {
  if [ ! -e /dev/kvm ]; then
    emit_fail "/dev/kvm: missing"
    return
  fi

  local mode owner group
  mode="$(stat -c '%a' /dev/kvm 2>/dev/null || echo '?')"
  owner="$(stat -c '%U' /dev/kvm 2>/dev/null || echo '?')"
  group="$(stat -c '%G' /dev/kvm 2>/dev/null || echo '?')"

  if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
    emit_fail "/dev/kvm: present but not readable/writable by current user (owner=$owner group=$group mode=$mode)"
    return
  fi

  emit_pass "/dev/kvm: present and accessible (owner=$owner group=$group mode=$mode)"
}

check_cgroup_v2() {
  if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    emit_pass "cgroup: v2 unified hierarchy detected"
  else
    emit_fail "cgroup: v2 unified hierarchy not detected"
  fi
}

check_swap() {
  local swap_lines
  swap_lines="$(awk 'NR>1 {count++} END {print count+0}' /proc/swaps 2>/dev/null || echo 0)"

  if [ "$swap_lines" -eq 0 ]; then
    emit_pass "Swap: disabled"
  else
    emit_fail "Swap: enabled"
  fi
}

check_module_loaded() {
  local module="$1"
  local label="$2"

  if [ -r /proc/modules ] && grep -qE "^${module}[[:space:]]" /proc/modules 2>/dev/null; then
    emit_pass "Kernel module: $label loaded"
  else
    emit_fail "Kernel module: $label not loaded"
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
    emit_fail "Sysctl: $key could not be read ($description)"
  elif [ "$val" = "$expected" ]; then
    emit_pass "Sysctl: $key=$expected ($description)"
  else
    emit_fail "Sysctl: $key=$val (expected $expected; $description)"
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
    emit_fail "Container runtime: no supported runtime detected (containerd or CRI-O required)"
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
      emit_warn "containerd config: could not confirm SystemdCgroup=true"
    fi
  else
    emit_warn "containerd config: /etc/containerd/config.toml not readable; systemd cgroup setting not verified"
  fi
}

check_memory_warning() {
  local mem_kb mem_gb
  mem_kb="$(awk '/MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  mem_kb="${mem_kb:-0}"

  if [ "$mem_kb" -le 0 ]; then
    emit_warn "Memory: could not determine total RAM"
    return
  fi

  mem_gb=$((mem_kb / 1024 / 1024))

  if [ "$mem_gb" -ge 16 ]; then
    emit_pass "Memory: ${mem_gb} GiB detected"
  else
    emit_warn "Memory: ${mem_gb} GiB detected (16+ GiB recommended for smoke testing)"
  fi
}

check_disk_warning() {
  local avail_kb avail_gb
  avail_kb="$(df -Pk /var 2>/dev/null | awk 'NR==2 {print $4}' || true)"

  if [ -z "$avail_kb" ]; then
    avail_kb="$(df -Pk / 2>/dev/null | awk 'NR==2 {print $4}' || true)"
  fi

  if [ -z "$avail_kb" ]; then
    emit_warn "Disk: could not determine available disk space"
    return
  fi

  avail_gb=$((avail_kb / 1024 / 1024))

  if [ "$avail_gb" -ge 100 ]; then
    emit_pass "Disk: ${avail_gb} GiB available"
  else
    emit_warn "Disk: ${avail_gb} GiB available (100+ GiB recommended for smoke testing)"
  fi
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

  print_summary

  if [ "$FAIL_COUNT" -gt 0 ]; then
    exit 1
  fi

  exit 0
}

main "$@"