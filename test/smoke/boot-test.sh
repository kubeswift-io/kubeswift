#!/usr/bin/env bash
# KubeSwift smoke test — multi-scenario VM boot validation.
# Requires: kubectl, KubeSwift cluster with CRDs and controllers deployed.
#
# Usage: ./boot-test.sh [OPTIONS]
#   --timeout-image MIN   Image import timeout (default: 15)
#   --timeout-guest MIN   Guest running timeout (default: 5)
#   --timeout-network MIN Network IP timeout (default: 5)
#   --no-cleanup          Skip resource cleanup after tests
#   --cleanup-only        Run only the cleanup function and exit
#   --scenario NAME       Run a single scenario (disk-boot, kernel-boot, qemu-boot, gpu-alloc, multi-nic)
#   --skip-qemu           Skip QEMU boot scenario
#   --skip-kernel         Skip kernel boot scenario

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SAMPLES_DIR="${REPO_ROOT}/config/samples"
RBAC_DIR="${REPO_ROOT}/config/rbac"
NAMESPACE="${NAMESPACE:-default}"
TIMEOUT_IMAGE=15
TIMEOUT_GUEST=5
TIMEOUT_NETWORK=5
NO_CLEANUP=false
CLEANUP_ONLY=false
SCENARIO=""
SKIP_QEMU=false
SKIP_KERNEL=false

# Results tracking
declare -A RESULTS

while [[ $# -gt 0 ]]; do
  case $1 in
    --timeout-image)  TIMEOUT_IMAGE="$2"; shift 2 ;;
    --timeout-guest)  TIMEOUT_GUEST="$2"; shift 2 ;;
    --timeout-network) TIMEOUT_NETWORK="$2"; shift 2 ;;
    --no-cleanup)     NO_CLEANUP=true; shift ;;
    --cleanup-only)   CLEANUP_ONLY=true; shift ;;
    --scenario)       SCENARIO="$2"; shift 2 ;;
    --skip-qemu)      SKIP_QEMU=true; shift ;;
    --skip-kernel)    SKIP_KERNEL=true; shift ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# --- Cleanup function ---
# Deletes all resources created by any scenario. Safe to call multiple times.
# All deletes use --ignore-not-found so missing resources are not errors.

cleanup_all() {
  echo ""
  echo "=== Cleaning up smoke-test resources ==="

  # 1. Guests first (they own launcher pods)
  echo "  Deleting SwiftGuests..."
  kubectl delete swiftguest sample multi-nic-test faas-test qemu-test gpu-test \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=60s 2>/dev/null || true

  # 2. Images (each has a backing PVC)
  echo "  Deleting SwiftImages..."
  kubectl delete swiftimage ubuntu-noble ubuntu-noble-qemu ubuntu-noble-multinic \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=60s 2>/dev/null || true

  # 3. Kernels
  echo "  Deleting SwiftKernels..."
  kubectl delete swiftkernel faas-minimal \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=30s 2>/dev/null || true

  # 4. GPU resources (cluster-scoped SwiftGPUNode, namespaced SwiftGPUProfile)
  echo "  Deleting GPU resources..."
  kubectl delete swiftgpunode mock-gpu-node \
    --ignore-not-found --timeout=30s 2>/dev/null || true
  kubectl delete swiftgpuprofile a100-pcie-single \
    -n "$NAMESPACE" --ignore-not-found --timeout=30s 2>/dev/null || true

  # 5. Seed profiles
  echo "  Deleting SwiftSeedProfiles..."
  kubectl delete swiftseedprofile minimal qemu-test-seed \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=30s 2>/dev/null || true

  # 6. Shared resources (guest class)
  echo "  Deleting SwiftGuestClass..."
  kubectl delete swiftguestclass default \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=30s 2>/dev/null || true

  # 7. ConfigMaps created by the controller for seed/intent
  echo "  Deleting ConfigMaps..."
  kubectl delete configmap \
    sample-seed sample-runtime-intent \
    multi-nic-test-seed multi-nic-test-runtime-intent \
    faas-test-runtime-intent \
    qemu-test-seed qemu-test-runtime-intent \
    gpu-test-seed gpu-test-runtime-intent \
    -n "$NAMESPACE" --ignore-not-found --timeout=10s 2>/dev/null || true

  echo "  Cleanup done."
}

# If --cleanup-only, run cleanup and exit immediately
if [[ "$CLEANUP_ONLY" == "true" ]]; then
  cleanup_all
  exit 0
fi

echo "=== KubeSwift smoke test ==="
echo "Namespace: $NAMESPACE"
echo "Image timeout: ${TIMEOUT_IMAGE}m, Guest timeout: ${TIMEOUT_GUEST}m, Network timeout: ${TIMEOUT_NETWORK}m"
echo ""

# --- Helpers ---

apply_rbac() {
  kubectl apply -k "$RBAC_DIR" -n "$NAMESPACE" >/dev/null 2>&1
  if [[ "$NAMESPACE" != "default" ]]; then
    kubectl patch rolebinding swiftletd-reporter -n "$NAMESPACE" --type=json \
      -p="[{\"op\":\"replace\",\"path\":\"/subjects/0/namespace\",\"value\":\"$NAMESPACE\"}]" 2>/dev/null || true
  fi
}

apply_shared() {
  kubectl apply -f "$SAMPLES_DIR/shared/swiftguestclass-default.yaml" -n "$NAMESPACE" >/dev/null
  kubectl apply -f "$SAMPLES_DIR/shared/swiftseedprofile-minimal.yaml" -n "$NAMESPACE" >/dev/null
}

wait_image_ready() {
  local name="$1"
  echo "  Waiting for SwiftImage $name Ready (timeout ${TIMEOUT_IMAGE}m)..."
  if ! kubectl wait --for=jsonpath='{.status.phase}'=Ready "swiftimage/$name" -n "$NAMESPACE" --timeout="${TIMEOUT_IMAGE}m" 2>/dev/null; then
    echo "  FAIL: SwiftImage $name did not reach Ready"
    kubectl describe "swiftimage/$name" -n "$NAMESPACE" 2>/dev/null || true
    return 1
  fi
}

wait_guest_running() {
  local name="$1"
  echo "  Waiting for SwiftGuest $name Running (timeout ${TIMEOUT_GUEST}m)..."
  if ! kubectl wait --for=jsonpath='{.status.phase}'=Running "swiftguest/$name" -n "$NAMESPACE" --timeout="${TIMEOUT_GUEST}m" 2>/dev/null; then
    local phase
    phase=$(kubectl get "swiftguest/$name" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")
    echo "  FAIL: SwiftGuest $name did not reach Running (phase=$phase)"
    kubectl describe "swiftguest/$name" -n "$NAMESPACE" 2>/dev/null || true
    return 1
  fi
}

wait_guest_ip() {
  local name="$1"
  echo "  Waiting for primaryIP (timeout ${TIMEOUT_NETWORK}m)..."
  local ip=""
  for _ in $(seq 1 $((TIMEOUT_NETWORK * 12))); do
    ip=$(kubectl get "swiftguest/$name" -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null || true)
    if [[ -n "$ip" ]]; then
      echo "  primaryIP=$ip"
      return 0
    fi
    sleep 5
  done
  echo "  FAIL: primaryIP not populated"
  return 1
}

check_hypervisor() {
  local name="$1" expected="$2"
  local actual
  actual=$(kubectl get "swiftguest/$name" -n "$NAMESPACE" -o jsonpath='{.status.runtime.hypervisor}' 2>/dev/null || echo "")
  if [[ "$actual" == "$expected" ]]; then
    echo "  hypervisor=$actual (expected)"
  else
    echo "  WARN: hypervisor=$actual, expected $expected"
  fi
}

# --- Scenario 1: Disk Boot (Cloud Hypervisor) ---

scenario_disk_boot() {
  echo ""
  echo "--- Scenario: disk-boot (Cloud Hypervisor + Ubuntu Noble) ---"

  apply_rbac
  apply_shared
  kubectl apply -f "$SAMPLES_DIR/disk-boot/swiftimage-ubuntu-noble.yaml" -n "$NAMESPACE" >/dev/null
  kubectl apply -f "$SAMPLES_DIR/disk-boot/swiftguest-sample.yaml" -n "$NAMESPACE" >/dev/null

  wait_image_ready "ubuntu-noble" || { RESULTS[disk-boot]="FAIL"; return; }
  wait_guest_running "sample" || { RESULTS[disk-boot]="FAIL"; return; }

  # Verify GuestRunning condition
  local gr
  gr=$(kubectl get swiftguest/sample -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="GuestRunning")].status}' 2>/dev/null || echo "")
  if [[ "$gr" != "True" ]]; then
    echo "  WARN: GuestRunning=$gr"
  fi

  check_hypervisor "sample" "cloud-hypervisor"
  wait_guest_ip "sample" || { RESULTS[disk-boot]="FAIL"; return; }

  RESULTS[disk-boot]="PASS"
  echo "  disk-boot: PASS"
}

# --- Scenario 2: Kernel Boot (Cloud Hypervisor) ---

scenario_kernel_boot() {
  echo ""
  echo "--- Scenario: kernel-boot (Cloud Hypervisor + faas-minimal) ---"

  # Check if any kernel-node labeled nodes exist
  local kernel_nodes
  kernel_nodes=$(kubectl get nodes -l kubeswift.io/kernel-node=true -o name 2>/dev/null | wc -l)
  if [[ "$kernel_nodes" -eq 0 ]]; then
    echo "  SKIP: No nodes labeled kubeswift.io/kernel-node=true"
    RESULTS[kernel-boot]="SKIP"
    return
  fi

  apply_rbac
  apply_shared
  kubectl apply -f "$SAMPLES_DIR/kernel-boot/swiftkernel-faas.yaml" -n "$NAMESPACE" >/dev/null

  echo "  Waiting for SwiftKernel faas-minimal Ready..."
  if ! kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/faas-minimal -n "$NAMESPACE" --timeout="5m" 2>/dev/null; then
    echo "  FAIL: SwiftKernel did not reach Ready"
    RESULTS[kernel-boot]="FAIL"
    return
  fi

  kubectl apply -f "$SAMPLES_DIR/kernel-boot/swiftguest-faas.yaml" -n "$NAMESPACE" >/dev/null
  wait_guest_running "faas-test" || { RESULTS[kernel-boot]="FAIL"; return; }
  check_hypervisor "faas-test" "cloud-hypervisor"
  wait_guest_ip "faas-test" || { RESULTS[kernel-boot]="FAIL"; return; }

  RESULTS[kernel-boot]="PASS"
  echo "  kernel-boot: PASS"
}

# --- Scenario 3: QEMU Boot ---

scenario_qemu_boot() {
  echo ""
  echo "--- Scenario: qemu-boot (QEMU + Ubuntu Noble + OVMF) ---"

  apply_rbac
  apply_shared
  kubectl apply -f "$SAMPLES_DIR/qemu-boot/swiftguest-qemu.yaml" -n "$NAMESPACE" >/dev/null

  wait_image_ready "ubuntu-noble-qemu" || { RESULTS[qemu-boot]="FAIL"; return; }
  wait_guest_running "qemu-test" || { RESULTS[qemu-boot]="FAIL"; return; }
  check_hypervisor "qemu-test" "qemu"
  wait_guest_ip "qemu-test" || { RESULTS[qemu-boot]="FAIL"; return; }

  RESULTS[qemu-boot]="PASS"
  echo "  qemu-boot: PASS"
}

# --- Scenario 4: GPU Allocation (control plane only, no hardware) ---

scenario_gpu_alloc() {
  echo ""
  echo "--- Scenario: gpu-alloc (control plane allocation, no GPU hardware) ---"

  apply_shared

  # Create mock SwiftGPUNode with a fake GPU
  echo "  Creating mock SwiftGPUNode..."
  cat <<'MOCK_EOF' | kubectl apply -f - >/dev/null
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUNode
metadata:
  name: mock-gpu-node
  labels:
    kubeswift.io/gpu-node: "true"
MOCK_EOF

  # Patch status subresource with mock GPU data
  kubectl patch swiftgpunode mock-gpu-node --type=merge --subresource=status -p '{
    "status": {
      "phase": "Ready",
      "gpuCount": 1,
      "freeGPUs": 1,
      "gpuModel": "NVIDIA A100-PCIe",
      "host": {
        "cpuTopology": {"sockets": 1, "coresPerSocket": 8, "threadsPerCore": 2, "totalCPUs": 16},
        "numaNodes": [{"id": 0, "cpus": "0-15", "memoryMi": 65536}],
        "iommuEnabled": true
      },
      "gpus": [{
        "index": 0,
        "pciAddress": "0000:41:00.0",
        "model": "NVIDIA A100-PCIe",
        "deviceId": "10de:20b0",
        "numaNode": 0,
        "iommuGroup": 15,
        "driver": "vfio-pci",
        "allocated": false,
        "allocatedTo": ""
      }]
    }
  }' >/dev/null 2>&1

  # Create GPU profile
  kubectl apply -f "$SAMPLES_DIR/gpu-pcie/swiftgpuprofile-a100-pcie.yaml" -n "$NAMESPACE" >/dev/null

  # Create GPU guest — it will get GPUAllocated but won't actually run (no real node)
  kubectl apply -f "$SAMPLES_DIR/gpu-pcie/swiftguest-gpu.yaml" -n "$NAMESPACE" >/dev/null

  # Wait for GPUAllocated condition
  echo "  Waiting for GPUAllocated=True (30s)..."
  local allocated=""
  for _ in $(seq 1 15); do
    allocated=$(kubectl get swiftguest/gpu-test -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="GPUAllocated")].status}' 2>/dev/null || echo "")
    if [[ "$allocated" == "True" ]]; then
      break
    fi
    sleep 2
  done

  if [[ "$allocated" != "True" ]]; then
    echo "  FAIL: GPUAllocated condition not True (status=$allocated)"
    kubectl describe swiftguest gpu-test -n "$NAMESPACE" 2>/dev/null || true
    RESULTS[gpu-alloc]="FAIL"
  else
    echo "  GPUAllocated=True"

    # Verify GPU status fields populated
    local devices hypervisor
    devices=$(kubectl get swiftguest/gpu-test -n "$NAMESPACE" -o jsonpath='{.status.gpu.devices}' 2>/dev/null || echo "")
    hypervisor=$(kubectl get swiftguest/gpu-test -n "$NAMESPACE" -o jsonpath='{.status.gpu.hypervisor}' 2>/dev/null || echo "")
    echo "  gpu.devices=$devices"
    echo "  gpu.hypervisor=$hypervisor"

    if [[ -n "$devices" ]] && [[ -n "$hypervisor" ]]; then
      RESULTS[gpu-alloc]="PASS"
      echo "  gpu-alloc: PASS"
    else
      echo "  FAIL: GPU status fields not populated"
      RESULTS[gpu-alloc]="FAIL"
    fi
  fi
}

# --- Scenario 5: Multi-NIC (backward compatibility — no Multus required) ---

scenario_multi_nic() {
  echo ""
  echo "--- Scenario: multi-nic (explicit interfaces field, single primary NIC) ---"

  apply_rbac
  apply_shared

  # Create a dedicated SwiftImage for this scenario to avoid PVC lock races
  # with other scenarios (e.g., disk-boot) that use a different SwiftImage.
  cat <<'IMG_EOF' | kubectl apply -n "$NAMESPACE" -f - >/dev/null
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble-multinic
spec:
  format: qcow2
  rootDisk:
    size: "40Gi"
  source:
    http:
      url: https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
IMG_EOF

  wait_image_ready "ubuntu-noble-multinic" || { RESULTS[multi-nic]="FAIL"; return; }

  # Apply a SwiftGuest with explicit interfaces field (single primary, no Multus needed)
  cat <<'MULTINIC_EOF' | kubectl apply -n "$NAMESPACE" -f - >/dev/null
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: multi-nic-test
spec:
  imageRef:
    name: ubuntu-noble-multinic
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  interfaces:
  - name: mgmt
  runPolicy: Running
MULTINIC_EOF

  wait_guest_running "multi-nic-test" || { RESULTS[multi-nic]="FAIL"; return; }
  check_hypervisor "multi-nic-test" "cloud-hypervisor"
  wait_guest_ip "multi-nic-test" || { RESULTS[multi-nic]="FAIL"; return; }

  RESULTS[multi-nic]="PASS"
  echo "  multi-nic: PASS"
}

# --- Run scenarios ---

run_scenario() {
  local name="$1"
  case "$name" in
    disk-boot)    scenario_disk_boot ;;
    kernel-boot)  scenario_kernel_boot ;;
    qemu-boot)    scenario_qemu_boot ;;
    gpu-alloc)    scenario_gpu_alloc ;;
    multi-nic)    scenario_multi_nic ;;
    *) echo "Unknown scenario: $name"; exit 1 ;;
  esac
}

if [[ -n "$SCENARIO" ]]; then
  run_scenario "$SCENARIO"
else
  scenario_disk_boot

  if [[ "$SKIP_KERNEL" == "true" ]]; then
    echo ""
    echo "--- Scenario: kernel-boot — SKIPPED (--skip-kernel) ---"
    RESULTS[kernel-boot]="SKIP"
  else
    scenario_kernel_boot
  fi

  if [[ "$SKIP_QEMU" == "true" ]]; then
    echo ""
    echo "--- Scenario: qemu-boot — SKIPPED (--skip-qemu) ---"
    RESULTS[qemu-boot]="SKIP"
  else
    scenario_qemu_boot
  fi

  scenario_gpu_alloc

  scenario_multi_nic
fi

# --- Cleanup ---

if [[ "$NO_CLEANUP" != "true" ]]; then
  cleanup_all
fi

# --- Summary ---

echo ""
echo "=== Smoke Test Summary ==="
printf "%-15s %s\n" "Scenario" "Result"
printf "%-15s %s\n" "--------" "------"
EXIT_CODE=0
for scenario in disk-boot kernel-boot qemu-boot gpu-alloc multi-nic; do
  result="${RESULTS[$scenario]:-N/A}"
  printf "%-15s %s\n" "$scenario" "$result"
  if [[ "$result" == "FAIL" ]]; then
    EXIT_CODE=1
  fi
done
echo ""

if [[ $EXIT_CODE -eq 0 ]]; then
  echo "=== All scenarios PASSED ==="
else
  echo "=== Some scenarios FAILED ==="
fi

exit $EXIT_CODE
