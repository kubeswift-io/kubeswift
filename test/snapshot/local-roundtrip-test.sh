#!/usr/bin/env bash
# KubeSwift Tier B local-backend round-trip e2e test.
#
# Purpose: prove that a memory snapshot actually captures + restores
# in-memory state. The test plants a sentinel file on the source VM's
# tmpfs (RAM-backed, NOT persisted to disk), takes a memory snapshot,
# kills the launcher pod, restores in-place, and verifies the sentinel
# survived.
#
# This is the canonical "memory snapshot works" test: a tmpfs file is
# in RAM, so if it survives a snapshot/kill/restore cycle, the memory
# state really did round-trip through the snapshot directory.
#
# Requires:
#   - kubectl configured against a KubeSwift cluster.
#   - The cluster's CRDs and controllers up-to-date with this branch.
#   - A node labeled for kubeswift workloads (any node will do — local
#     snapshots don't need GPU labels).
#
# Usage:
#   ./local-roundtrip-test.sh [--no-cleanup] [--namespace <ns>]

set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
NO_CLEANUP=false
SAMPLES_DIR="$(cd "$(dirname "$0")/../../config/samples/local-snapshots" && pwd)"
IMAGE_DIR="$(cd "$(dirname "$0")/../../config/samples/images" 2>/dev/null && pwd || true)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-cleanup) NO_CLEANUP=true; shift ;;
    --namespace)  NAMESPACE="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

echo "=== KubeSwift Tier B round-trip e2e ==="
echo "Namespace:    $NAMESPACE"
echo "Samples:      $SAMPLES_DIR"
echo ""

cleanup() {
  if [[ "$NO_CLEANUP" == "true" ]]; then
    echo "(skipping cleanup; --no-cleanup)"
    return
  fi
  echo ""
  echo "--- Cleanup ---"
  kubectl delete -n "$NAMESPACE" --ignore-not-found \
    swiftrestore/snapshot-local-inplace \
    swiftsnapshot/snapshot-local-mem \
    swiftguest/snapshot-local-source \
    swiftguestclass/snapshot-local-class \
    swiftseedprofile/clone-identity-regen >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 1. Apply the seed profile + source SwiftGuest manifests.
echo "--- Step 1: Apply source manifests ---"
kubectl apply -n "$NAMESPACE" -f "$(cd "$(dirname "$0")/../../config/samples/seed-profiles" && pwd)/clone-identity-regen.yaml" >/dev/null
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftguest-source.yaml" >/dev/null

# Ensure Ubuntu Noble image is present (most clusters running these
# tests already have it from the snapshot-test.sh prior runs).
if ! kubectl get swiftimage ubuntu-noble -n "$NAMESPACE" >/dev/null 2>&1; then
  echo "  Importing Ubuntu Noble (first run on this cluster)..."
  kubectl apply -n "$NAMESPACE" -f - >/dev/null <<'EOF'
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: ubuntu-noble
spec:
  format: qcow2
  rootDisk:
    size: "10Gi"
  source:
    http:
      url: https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img
EOF
  kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftimage/ubuntu-noble -n "$NAMESPACE" --timeout=15m
fi

echo "  Waiting for source SwiftGuest Running (5m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Running \
  swiftguest/snapshot-local-source -n "$NAMESPACE" --timeout=5m

# Wait for the guest to actually be reachable via swiftctl ssh. The
# Running phase asserts CH is bound; the SSH usability comes a few
# seconds later when sshd reports.
echo "  Waiting up to 2m for SSH reachability via swiftctl..."
SSH_READY=false
for _ in $(seq 1 24); do
  if swiftctl ssh -n "$NAMESPACE" snapshot-local-source -- true >/dev/null 2>&1; then
    SSH_READY=true
    break
  fi
  sleep 5
done
if [[ "$SSH_READY" != "true" ]]; then
  echo "  FAIL: SSH never became reachable on snapshot-local-source"
  exit 1
fi

# 2. Plant the tmpfs sentinel — RAM-only, NOT persisted to disk.
echo ""
echo "--- Step 2: Plant tmpfs sentinel inside the guest (in-memory only) ---"
SENTINEL_VALUE="kubeswift-roundtrip-$(date +%s)-$RANDOM"
swiftctl ssh -n "$NAMESPACE" snapshot-local-source -- sudo bash -c "
  mkdir -p /run/kubeswift-mem
  echo '$SENTINEL_VALUE' > /run/kubeswift-mem/sentinel
  # Verify it actually landed on tmpfs (not a disk).
  fs_type=\$(stat -fc %T /run/kubeswift-mem)
  if [[ \"\$fs_type\" != \"tmpfs\" ]]; then
    echo \"sentinel dir not tmpfs: fs_type=\$fs_type\"
    exit 1
  fi
"
echo "  Planted: SENTINEL_VALUE=$SENTINEL_VALUE"

# 3. Snapshot with includeMemory: true.
echo ""
echo "--- Step 3: Take Tier B memory snapshot ---"
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftsnapshot-memory.yaml" >/dev/null
echo "  Waiting for SwiftSnapshot Ready (5m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  swiftsnapshot/snapshot-local-mem -n "$NAMESPACE" --timeout=5m

NODE_NAME=$(kubectl get swiftsnapshot snapshot-local-mem -n "$NAMESPACE" -o jsonpath='{.status.nodeName}')
PAUSE_MS=$(kubectl get swiftsnapshot snapshot-local-mem -n "$NAMESPACE" -o jsonpath='{.status.observedPauseWindowMs}')
echo "  Captured on node $NODE_NAME (pause window ${PAUSE_MS}ms)"
if [[ -z "$NODE_NAME" ]]; then
  echo "  FAIL: snapshot status.nodeName is empty"
  exit 1
fi

# 4. Kill the source launcher pod. The SwiftGuest controller would
#    normally just recreate the pod from disk (fresh boot, no memory).
#    We immediately follow with a SwiftRestore to bring it back from
#    the snapshot.
echo ""
echo "--- Step 4: Kill source launcher pod ---"
kubectl delete pod snapshot-local-source -n "$NAMESPACE" --grace-period=0 --force --ignore-not-found >/dev/null
sleep 5

# 5. In-place restore.
echo ""
echo "--- Step 5: In-place restore (fast path: no init container, no patcher) ---"
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftrestore-inplace.yaml" >/dev/null
echo "  Waiting for SwiftRestore Ready (5m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  swiftrestore/snapshot-local-inplace -n "$NAMESPACE" --timeout=5m

# Verify the launcher pod is in restore-receive mode (label set by
# BuildRestorePod) and that there's NO snapshot-stager init container
# (in-place fast path).
ROLE_LABEL=$(kubectl get pod snapshot-local-source -n "$NAMESPACE" -o jsonpath='{.metadata.labels.swift\.kubeswift\.io/role}' 2>/dev/null || echo "")
if [[ "$ROLE_LABEL" != "restore-receive" ]]; then
  echo "  FAIL: pod missing restore-receive role label (got $ROLE_LABEL)"
  exit 1
fi
HAS_STAGER=$(kubectl get pod snapshot-local-source -n "$NAMESPACE" -o jsonpath='{.spec.initContainers[?(@.name=="snapshot-stager")].name}' 2>/dev/null || echo "")
if [[ -n "$HAS_STAGER" ]]; then
  echo "  FAIL: in-place restore pod must NOT have snapshot-stager init container; found: $HAS_STAGER"
  exit 1
fi
echo "  OK: launcher pod is in restore-receive mode, no stager (in-place fast path)"

# 6. Verify the tmpfs sentinel survived.
echo ""
echo "--- Step 6: Verify tmpfs sentinel survived (the headline Phase 2 promise) ---"
echo "  Waiting up to 2m for SSH reachability via swiftctl post-restore..."
for _ in $(seq 1 24); do
  if swiftctl ssh -n "$NAMESPACE" snapshot-local-source -- true >/dev/null 2>&1; then
    break
  fi
  sleep 5
done
GOT_VALUE=$(swiftctl ssh -n "$NAMESPACE" snapshot-local-source -- cat /run/kubeswift-mem/sentinel 2>/dev/null || echo "<missing>")
GOT_VALUE=$(echo "$GOT_VALUE" | tr -d '\r\n')
if [[ "$GOT_VALUE" != "$SENTINEL_VALUE" ]]; then
  echo "  FAIL: sentinel mismatch"
  echo "    expected: $SENTINEL_VALUE"
  echo "    got:      $GOT_VALUE"
  echo "  This means the memory snapshot did NOT round-trip — the VM"
  echo "  came up clean rather than restoring from the captured RAM."
  exit 1
fi
echo "  OK: sentinel survived: $GOT_VALUE"

echo ""
echo "=== Tier B round-trip e2e PASS ==="
echo "Memory state was captured, the VM was killed, restored in-place,"
echo "and the tmpfs sentinel matches — proving CH --restore actually"
echo "loaded the captured RAM image rather than booting fresh."
