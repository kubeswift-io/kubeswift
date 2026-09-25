#!/usr/bin/env bash
# KubeSwift smoke test — multi-scenario VM boot validation.
# Requires: kubectl, KubeSwift cluster with CRDs and controllers deployed.
#
# Runs in $NAMESPACE (default: default); the samples' `namespace: default` is
# dropped when they are created. Objects the run creates are labelled
# kubeswift.io/smoke-test=<namespace>, and cleanup (--cleanup-only, or the end
# of a run without --no-cleanup) deletes only those. An object that already
# exists, such as a shared SwiftImage or the cluster's `default`
# SwiftGuestClass, is used as it is: not updated and not deleted.
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

# Label on every object this script creates; its value is the namespace of the
# run, so the cluster-scoped objects of runs in different namespaces are told
# apart.
OWNER_LABEL="kubeswift.io/smoke-test"
OWNED="${OWNER_LABEL}=${NAMESPACE}"

# --- Cleanup function ---
# Deletes the objects that carry this run's label, and nothing else: an object
# the run found already there and reused (a lab's shared SwiftImage, the
# cluster's `default` SwiftGuestClass) is left alone. Safe to call multiple
# times, and from a later invocation (--cleanup-only), since the label is on
# the objects themselves.

cleanup_all() {
  echo ""
  echo "=== Cleaning up smoke-test resources (label ${OWNED}) ==="

  # 0. The gpu-alloc mock SwiftGPUNode before its guest, so that its GPU is
  #    never free for another guest to take. It is named after a real Node,
  #    which has no GPU.
  echo "  Deleting mock SwiftGPUNode..."
  kubectl delete swiftgpunode -l "$OWNED" \
    --ignore-not-found --timeout=30s 2>/dev/null || true

  # 1. Guests (they own launcher pods). Their seed Secret and runtime-intent
  #    ConfigMap are owned by the guest and go with it.
  echo "  Deleting SwiftGuests..."
  kubectl delete swiftguest -l "$OWNED" \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=60s 2>/dev/null || true

  # 2. Images (each has a backing PVC)
  echo "  Deleting SwiftImages..."
  kubectl delete swiftimage -l "$OWNED" \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=60s 2>/dev/null || true

  # 3. Kernels
  echo "  Deleting SwiftKernels..."
  kubectl delete swiftkernel -l "$OWNED" \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=30s 2>/dev/null || true

  # 4. GPU profiles
  echo "  Deleting SwiftGPUProfiles..."
  kubectl delete swiftgpuprofile -l "$OWNED" \
    -n "$NAMESPACE" --ignore-not-found --timeout=30s 2>/dev/null || true

  # 5. Seed profiles
  echo "  Deleting SwiftSeedProfiles..."
  kubectl delete swiftseedprofile -l "$OWNED" \
    -n "$NAMESPACE" --ignore-not-found --wait --timeout=30s 2>/dev/null || true

  # 6. Guest class (cluster-scoped)
  echo "  Deleting SwiftGuestClass..."
  kubectl delete swiftguestclass -l "$OWNED" \
    --ignore-not-found --wait --timeout=30s 2>/dev/null || true

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
  # As of 2026-04-29 the controller auto-creates the per-namespace
  # `swiftletd-reporter` RoleBinding on first SwiftGuest reconcile in
  # a namespace; no manual apply is needed. Apply the cluster-scoped
  # ClusterRole as a no-op (idempotent) so older clusters that don't
  # have the ClusterRole yet still get it.
  kubectl apply -k "$RBAC_DIR" >/dev/null 2>&1 || true
}

# sample FILE: print a sample manifest without its `namespace: default`, so it
# is created in $NAMESPACE (kubectl refuses -n with a different namespace in
# the object).
sample() {
  sed -E '/^  namespace: default$/d' "$SAMPLES_DIR/$1"
}

# create_owned: create the objects of the manifest on stdin in $NAMESPACE,
# labelled ${OWNED}. An object that already exists is used as it is: it is not
# updated, and cleanup, which deletes only labelled objects, leaves it. Any
# other error fails.
create_owned() {
  local manifest out line obj rc=0 failed=false
  manifest=$(kubectl label --local -f - "$OWNED" -o json) || return 1
  out=$(printf '%s\n' "$manifest" | kubectl create -n "$NAMESPACE" -f - 2>&1) || rc=$?
  while IFS= read -r line; do
    case "$line" in
      "" | *" created") ;;
      *"(AlreadyExists)"*)
        obj="${line##*: }"
        echo "  Using the existing ${obj% already exists} as it is"
        ;;
      *) echo "  $line" >&2; failed=true ;;
    esac
  done <<<"$out"
  if [[ "$failed" == "true" || ( "$rc" -ne 0 && "$out" != *"(AlreadyExists)"* ) ]]; then
    return 1
  fi
}

apply_shared() {
  sample shared/swiftguestclass-default.yaml | create_owned
  sample shared/swiftseedprofile-minimal.yaml | create_owned
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

# guest_field NAME JSONPATH [WANT]: print a SwiftGuest status field once it
# equals WANT (or, without WANT, once it is set), polling for up to 60 s.
# The GuestRunning condition and status.runtime are written moments after
# phase=Running, so a single read at that instant found them empty. Prints
# the last value read when the time runs out.
guest_field() {
  local name="$1" path="$2" want="${3:-}" got=""
  for _ in $(seq 1 30); do
    got=$(kubectl get "swiftguest/$name" -n "$NAMESPACE" -o jsonpath="$path" 2>/dev/null || true)
    if [[ -n "$want" && "$got" == "$want" ]] || [[ -z "$want" && -n "$got" ]]; then
      break
    fi
    sleep 2
  done
  echo "$got"
}

check_hypervisor() {
  local name="$1" expected="$2"
  local actual
  actual=$(guest_field "$name" '{.status.runtime.hypervisor}')
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
  sample disk-boot/swiftimage-ubuntu-noble.yaml | create_owned
  sample disk-boot/swiftguest-sample.yaml | create_owned

  wait_image_ready "ubuntu-noble" || { RESULTS[disk-boot]="FAIL"; return; }
  wait_guest_running "sample" || { RESULTS[disk-boot]="FAIL"; return; }

  # Verify GuestRunning condition
  local gr
  gr=$(guest_field sample '{.status.conditions[?(@.type=="GuestRunning")].status}' True)
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
  sample kernel-boot/swiftkernel-faas.yaml | create_owned

  echo "  Waiting for SwiftKernel faas-minimal Ready..."
  if ! kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftkernel/faas-minimal -n "$NAMESPACE" --timeout="5m" 2>/dev/null; then
    echo "  FAIL: SwiftKernel did not reach Ready"
    RESULTS[kernel-boot]="FAIL"
    return
  fi

  sample kernel-boot/swiftguest-faas.yaml | create_owned
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
  sample qemu-boot/swiftguest-qemu.yaml | create_owned

  wait_image_ready "ubuntu-noble-qemu" || { RESULTS[qemu-boot]="FAIL"; return; }
  wait_guest_running "qemu-test" || { RESULTS[qemu-boot]="FAIL"; return; }
  check_hypervisor "qemu-test" "qemu"
  wait_guest_ip "qemu-test" || { RESULTS[qemu-boot]="FAIL"; return; }

  RESULTS[qemu-boot]="PASS"
  echo "  qemu-boot: PASS"
}

# --- Scenario 4: GPU Allocation (control plane only, no hardware) ---
#
# Allocation takes a SwiftGPUNode only when it is VFIO-ready and a Node of the
# same name exists and is not cordoned, so a mock called mock-gpu-node was
# always refused (NoCapacity). The mock is named after a real Node instead:
# one that is not cordoned, has no SwiftGPUNode, and is not labelled
# kubeswift.io/gpu-node=true (gpu-discovery runs there and owns that Node's
# SwiftGPUNode). That Node has no GPU, so nothing may start there with the
# mock's device:
#   - the test guest is created Stopped. Allocation does not depend on
#     runPolicy, and a Stopped guest never gets a launcher, whose gpu-init
#     would bind the device's PCI address to vfio-pci;
#   - spec.nodeName limits its allocation to the mock, so no real GPU is
#     reserved;
#   - the mock is given its GPU only once the test guest waits for it, and not
#     at all while another SwiftGuest in the cluster waits for a GPU, which
#     could take it instead. Its PCI address is one no host has;
#   - cleanup deletes the mock before the guest, so its GPU is never free.

# mock_gpu_node: print the Node to name the mock SwiftGPUNode after: the mock
# an earlier run in this namespace left, else the first qualifying Node by
# name. Prints nothing when there is none.
mock_gpu_node() {
  local existing taken node unschedulable gpu
  existing=$(kubectl get swiftgpunode -l "$OWNED" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -n "$existing" ]]; then
    echo "$existing"
    return
  fi
  taken=" $(kubectl get swiftgpunode -o jsonpath='{.items[*].metadata.name}') " || return 0
  while read -r node unschedulable gpu; do
    [[ "$unschedulable" == "true" || "$gpu" == "true" || "$taken" == *" $node "* ]] && continue
    echo "$node"
    return
  done < <(kubectl get nodes --no-headers \
    -o custom-columns='NAME:.metadata.name,UNSCHEDULABLE:.spec.unschedulable,GPU:.metadata.labels.kubeswift\.io/gpu-node' |
    sort)
}

scenario_gpu_alloc() {
  echo ""
  echo "--- Scenario: gpu-alloc (control plane allocation, no GPU hardware) ---"

  local guests waiting mock created=false
  if ! guests=$(kubectl get swiftguest -A -o jsonpath='{range .items[?(@.spec.gpuProfileRef)]}{.metadata.namespace}/{.metadata.name} {.status.conditions[?(@.type=="GPUAllocated")].status}{"\n"}{end}'); then
    echo "  SKIP: cannot list SwiftGuests in all namespaces to check that none waits for a GPU"
    RESULTS[gpu-alloc]="SKIP"
    return
  fi
  waiting=$(awk -v self="${NAMESPACE}/gpu-test" '$1 != self && $2 != "True" { print $1 }' <<<"$guests")
  if [[ -n "$waiting" ]]; then
    echo "  SKIP: SwiftGuests wait for a GPU and could be given the mock one: ${waiting//$'\n'/ }"
    RESULTS[gpu-alloc]="SKIP"
    return
  fi
  mock=$(mock_gpu_node)
  if [[ -z "$mock" ]]; then
    echo "  SKIP: no Node to name the mock SwiftGPUNode after (every Node is cordoned, labelled kubeswift.io/gpu-node=true, or has a SwiftGPUNode)"
    RESULTS[gpu-alloc]="SKIP"
    return
  fi

  apply_shared
  sample gpu-pcie/swiftgpuprofile-a100-pcie.yaml | create_owned || { RESULTS[gpu-alloc]="FAIL"; return; }

  echo "  Mock SwiftGPUNode $mock (named after an existing Node, which has no GPU)"
  if ! kubectl get swiftgpunode "$mock" >/dev/null 2>&1; then
    create_owned <<MOCK_EOF || { RESULTS[gpu-alloc]="FAIL"; return; }
apiVersion: gpu.kubeswift.io/v1alpha1
kind: SwiftGPUNode
metadata:
  name: ${mock}
  labels:
    kubeswift.io/gpu-node: "true"
MOCK_EOF
    created=true
  fi
  # Never write to a SwiftGPUNode this test did not create.
  if [[ "$(kubectl get swiftgpunode "$mock" -o jsonpath='{.metadata.labels.kubeswift\.io/smoke-test}' 2>/dev/null || true)" != "$NAMESPACE" ]]; then
    echo "  FAIL: SwiftGPUNode $mock was not created by this test; left untouched"
    RESULTS[gpu-alloc]="FAIL"
    return
  fi

  # Stopped, and pinned to the mock: see the note above this scenario.
  cat <<GPU_EOF | create_owned || { RESULTS[gpu-alloc]="FAIL"; return; }
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: gpu-test
spec:
  imageRef:
    name: ubuntu-noble-qemu
  gpuProfileRef:
    name: a100-pcie-single
  guestClassRef:
    name: default
  seedProfileRef:
    name: minimal
  nodeName: ${mock}
  runPolicy: Stopped
GPU_EOF

  # Give the mock its GPU, unless it was reused with one: that GPU may already
  # be allocated to gpu-test.
  if [[ "$created" == "false" && -n "$(kubectl get swiftgpunode "$mock" -o jsonpath='{.status.gpus}' 2>/dev/null || true)" ]]; then
    echo "  Reusing the mock's GPU from an earlier run"
  elif ! kubectl patch swiftgpunode "$mock" --type=merge --subresource=status -p '{
    "status": {
      "phase": "Ready",
      "vfioReady": true,
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
        "pciAddress": "ffff:ff:1f.7",
        "vendor": "NVIDIA",
        "model": "NVIDIA A100-PCIe",
        "deviceId": "10de:20b0",
        "numaNode": 0,
        "iommuGroup": 15,
        "driver": "vfio-pci",
        "allocated": false,
        "allocatedTo": ""
      }]
    }
  }' >/dev/null; then
    echo "  FAIL: could not set the mock SwiftGPUNode status"
    RESULTS[gpu-alloc]="FAIL"
    return
  fi

  # Wait for GPUAllocated condition. A guest refused before the mock had its
  # GPU is retried when the SwiftGPUNode changes, and after 30 s.
  echo "  Waiting for GPUAllocated=True (60s)..."
  local allocated=""
  for _ in $(seq 1 30); do
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

    # Verify GPU status fields populated, from the mock
    local devices hypervisor gpunode
    devices=$(kubectl get swiftguest/gpu-test -n "$NAMESPACE" -o jsonpath='{.status.gpu.devices}' 2>/dev/null || echo "")
    hypervisor=$(kubectl get swiftguest/gpu-test -n "$NAMESPACE" -o jsonpath='{.status.gpu.hypervisor}' 2>/dev/null || echo "")
    gpunode=$(kubectl get swiftguest/gpu-test -n "$NAMESPACE" -o jsonpath='{.status.gpu.nodeName}' 2>/dev/null || echo "")
    echo "  gpu.devices=$devices"
    echo "  gpu.hypervisor=$hypervisor"
    echo "  gpu.nodeName=$gpunode"

    if [[ -n "$devices" ]] && [[ -n "$hypervisor" ]] && [[ "$gpunode" == "$mock" ]]; then
      RESULTS[gpu-alloc]="PASS"
      echo "  gpu-alloc: PASS"
    else
      echo "  FAIL: GPU status fields not populated, or not from the mock"
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
  cat <<'IMG_EOF' | create_owned
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
  cat <<'MULTINIC_EOF' | create_owned
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
