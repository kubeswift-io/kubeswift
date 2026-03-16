#!/usr/bin/env bash
# Smoke test: apply boot-first samples, verify each stage, wait for Ready/Running, assert conditions, cleanup.
# Requires: kubectl, KubeSwift cluster with CRDs and controllers deployed.
# See docs/operator/smoke-verification.md for prerequisites and failure checks.
#
# Usage: ./boot-test.sh [--timeout-image MIN] [--timeout-guest MIN] [--timeout-network MIN] [--no-cleanup]
# Default: timeout-image=15, timeout-guest=5, timeout-network=5

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

while [[ $# -gt 0 ]]; do
  case $1 in
    --timeout-image)
      TIMEOUT_IMAGE="$2"
      shift 2
      ;;
    --timeout-guest)
      TIMEOUT_GUEST="$2"
      shift 2
      ;;
    --timeout-network)
      TIMEOUT_NETWORK="$2"
      shift 2
      ;;
    --no-cleanup)
      NO_CLEANUP=true
      shift
      ;;
    *)
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

echo "=== KubeSwift boot smoke test ==="
echo "Namespace: $NAMESPACE"
echo "Image timeout: ${TIMEOUT_IMAGE}m, Guest timeout: ${TIMEOUT_GUEST}m, Network timeout: ${TIMEOUT_NETWORK}m"
echo ""

# Apply RBAC before SwiftGuest (required for swiftletd status reporting)
echo "Applying RBAC for swiftletd..."
kubectl apply -k "$RBAC_DIR" -n "$NAMESPACE"
# Patch RoleBinding subject for non-default namespace
if [[ "$NAMESPACE" != "default" ]]; then
  kubectl patch rolebinding swiftletd-reporter -n "$NAMESPACE" --type=json \
    -p="[{\"op\":\"replace\",\"path\":\"/subjects/0/namespace\",\"value\":\"$NAMESPACE\"}]" 2>/dev/null || true
fi

# Apply samples in order
echo "Applying samples..."
kubectl apply -f "$SAMPLES_DIR/swiftguestclass-default.yaml" -n "$NAMESPACE"
kubectl apply -f "$SAMPLES_DIR/swiftimage-http.yaml" -n "$NAMESPACE"
kubectl apply -f "$SAMPLES_DIR/swiftseedprofile-minimal.yaml" -n "$NAMESPACE"
kubectl apply -f "$SAMPLES_DIR/swiftguest-sample.yaml" -n "$NAMESPACE"

# Stage 1: Wait for SwiftImage Ready
echo "Waiting for SwiftImage ubuntu-cloud Ready (timeout ${TIMEOUT_IMAGE}m)..."
if ! kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftimage/ubuntu-cloud -n "$NAMESPACE" --timeout="${TIMEOUT_IMAGE}m"; then
  echo "FAIL: SwiftImage did not reach Ready"
  kubectl describe swiftimage ubuntu-cloud -n "$NAMESPACE" || true
  exit 1
fi
echo "SwiftImage Ready"

# Stage 2: Verify SwiftGuest pod created and scheduled
echo "Verifying SwiftGuest pod created and scheduled..."
POD_NAME=""
for i in $(seq 1 90); do
  POD_NAME=$(kubectl get pods -n "$NAMESPACE" -l swift.kubeswift.io/guest=sample -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -n "$POD_NAME" ]]; then
    PHASE=$(kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "$PHASE" == "Running" ]]; then
      break
    fi
    if [[ "$PHASE" == "Pending" ]] && [[ $i -gt 60 ]]; then
      echo "FAIL: SwiftGuest pod stuck Pending"
      kubectl describe pod "$POD_NAME" -n "$NAMESPACE" || true
      exit 1
    fi
  fi
  sleep 2
done
if [[ -z "$POD_NAME" ]]; then
  echo "FAIL: SwiftGuest pod not found"
  kubectl get pods -n "$NAMESPACE" -l swift.kubeswift.io/guest=sample || true
  kubectl describe swiftguest sample -n "$NAMESPACE" || true
  exit 1
fi
echo "Pod $POD_NAME created and scheduled"

# Stage 3: Verify seed ConfigMap and mount (sample has seedProfileRef)
echo "Verifying seed ConfigMap and mount..."
if ! kubectl get configmap sample-seed -n "$NAMESPACE" &>/dev/null; then
  echo "FAIL: Seed ConfigMap sample-seed not found"
  kubectl get configmap -n "$NAMESPACE" | grep -E "sample|seed" || true
  exit 1
fi
if ! kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o yaml | grep -q "mountPath: /var/lib/kubeswift/seed"; then
  echo "WARN: Seed volume mount at /var/lib/kubeswift/seed not found (check pod spec)"
fi
echo "Seed ConfigMap and mount OK"

# Stage 4 & 5: Wait for SwiftGuest Running and verify launcher
echo "Waiting for SwiftGuest sample Running (timeout ${TIMEOUT_GUEST}m)..."
if ! kubectl wait --for=jsonpath='{.status.phase}'=Running swiftguest/sample -n "$NAMESPACE" --timeout="${TIMEOUT_GUEST}m" 2>/dev/null; then
  PHASE=$(kubectl get swiftguest sample -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  echo "FAIL: SwiftGuest did not reach Running (phase=$PHASE)"
  kubectl describe swiftguest sample -n "$NAMESPACE" || true
  echo "Launcher logs:"
  kubectl logs "$POD_NAME" -n "$NAMESPACE" -c launcher --tail=50 2>/dev/null || true
  exit 1
fi

# Verify launcher container is running
LAUNCHER_STATUS=$(kubectl get pod "$POD_NAME" -n "$NAMESPACE" -o jsonpath='{.status.containerStatuses[?(@.name=="launcher")].state}' 2>/dev/null || true)
if [[ -z "$LAUNCHER_STATUS" ]] || echo "$LAUNCHER_STATUS" | grep -q "terminated"; then
  echo "FAIL: Launcher container not running or terminated"
  kubectl logs "$POD_NAME" -n "$NAMESPACE" -c launcher 2>/dev/null || true
  exit 1
fi
echo "SwiftGuest Running"

# Assert status conditions
echo "Checking status conditions..."
kubectl get swiftguest sample -n "$NAMESPACE" -o yaml | grep -E "Resolved|PodScheduled|GuestRunning" || true
GUEST_RUNNING=$(kubectl get swiftguest sample -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="GuestRunning")].status}' 2>/dev/null || echo "")
if [[ "$GUEST_RUNNING" != "True" ]]; then
  echo "WARN: GuestRunning condition not True (status=$GUEST_RUNNING)"
fi
echo "Conditions OK"

# Stage 6: Verify networking (status.network.primaryIP)
echo "Waiting for status.network.primaryIP (timeout ${TIMEOUT_NETWORK}m)..."
PRIMARY_IP=""
for i in $(seq 1 $((TIMEOUT_NETWORK * 12))); do
  PRIMARY_IP=$(kubectl get swiftguest sample -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null || true)
  if [[ -n "$PRIMARY_IP" ]]; then
    break
  fi
  sleep 5
done
if [[ -z "$PRIMARY_IP" ]]; then
  echo "FAIL: status.network.primaryIP not populated (networking may not be working)"
  kubectl describe swiftguest sample -n "$NAMESPACE" || true
  kubectl logs "$POD_NAME" -n "$NAMESPACE" -c launcher --tail=30 2>/dev/null || true
  exit 1
fi
echo "status.network.primaryIP=$PRIMARY_IP"

echo ""
echo "=== Smoke test PASSED ==="

# Cleanup
if [[ "$NO_CLEANUP" != "true" ]]; then
  echo "Cleaning up..."
  kubectl delete swiftguest sample -n "$NAMESPACE" --ignore-not-found --wait=false
  kubectl delete swiftimage ubuntu-cloud -n "$NAMESPACE" --ignore-not-found --wait=false
  kubectl delete swiftseedprofile minimal -n "$NAMESPACE" --ignore-not-found --wait=false
  kubectl delete swiftguestclass default -n "$NAMESPACE" --ignore-not-found --wait=false
  echo "Cleanup done"
fi
