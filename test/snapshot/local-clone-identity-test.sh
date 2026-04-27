#!/usr/bin/env bash
# KubeSwift Tier B clone-identity-regeneration e2e test.
#
# Purpose: prove that two clones from a single memory snapshot end up
# with distinct identities (machine-id, SSH host keys, hostname, MAC)
# despite resuming from the same captured RAM. This is the contract
# the SwiftRestore stager + in-guest cloud-init bootcmd implement.
#
# The seed profile (config/samples/seed-profiles/clone-identity-regen.yaml)
# is applied to the source VM. When the controller patches the snapshot's
# config.json with kubeswift.clone=true and per-clone MAC rewrites, the
# stager init container materializes the patched copy in a pod-local
# emptyDir; the in-guest bootcmd regenerates machine-id, SSH host keys,
# and hostname on first wake.
#
# Requires:
#   - kubectl configured against a KubeSwift cluster.
#   - Source VM image: ubuntu-noble (created on first run if missing).

set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
NO_CLEANUP=false
SAMPLES_DIR="$(cd "$(dirname "$0")/../../config/samples/local-snapshots" && pwd)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-cleanup) NO_CLEANUP=true; shift ;;
    --namespace)  NAMESPACE="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

echo "=== KubeSwift Tier B clone-identity e2e ==="
echo "Namespace: $NAMESPACE"
echo ""

cleanup() {
  if [[ "$NO_CLEANUP" == "true" ]]; then
    echo "(skipping cleanup; --no-cleanup)"
    return
  fi
  echo ""
  echo "--- Cleanup ---"
  kubectl delete -n "$NAMESPACE" --ignore-not-found \
    swiftrestore/snapshot-local-clone-a \
    swiftrestore/snapshot-local-clone-b \
    swiftguest/snapshot-local-clone-a \
    swiftguest/snapshot-local-clone-b \
    swiftsnapshot/snapshot-local-mem \
    swiftguest/snapshot-local-source \
    swiftguestclass/snapshot-local-class \
    swiftseedprofile/snapshot-local-test-seed >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Helper: SSH into a guest and capture identity attributes.
# Prints: machine-id sshfp hostname mac (space-separated).
identity_of() {
  local guest="$1"
  swiftctl ssh -n "$NAMESPACE" "$guest" -- bash -c '
    set -e
    mid=$(cat /etc/machine-id 2>/dev/null || echo missing)
    sshfp=$(ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub 2>/dev/null | awk "{print \$2}" || echo missing)
    if [ "$sshfp" = "missing" ]; then
      sshfp=$(ssh-keygen -lf /etc/ssh/ssh_host_rsa_key.pub 2>/dev/null | awk "{print \$2}" || echo missing)
    fi
    hn=$(hostname 2>/dev/null || echo missing)
    mac=$(cat /sys/class/net/eth0/address 2>/dev/null || echo missing)
    echo "$mid $sshfp $hn $mac"
  ' 2>/dev/null | tail -1
}

wait_for_ssh() {
  local guest="$1"
  local timeout="${2:-180}"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    if swiftctl ssh -n "$NAMESPACE" "$guest" -- true >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done
  return 1
}

# 1. Source VM with the combined seed profile (kubeswift user for
#    swiftctl ssh + clone-identity-regen bootcmd).
echo "--- Step 1: Apply source manifests + seed profile ---"
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftseedprofile-test.yaml" >/dev/null
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftguest-source.yaml" >/dev/null

if ! kubectl get swiftimage ubuntu-noble -n "$NAMESPACE" >/dev/null 2>&1; then
  echo "  Importing Ubuntu Noble..."
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

if ! wait_for_ssh snapshot-local-source 180; then
  echo "  FAIL: SSH never reachable on source"
  exit 1
fi

# Capture source identity for later comparison.
SOURCE_IDENTITY=$(identity_of snapshot-local-source)
echo "  Source identity: $SOURCE_IDENTITY"

# 2. Take memory snapshot.
echo ""
echo "--- Step 2: Take Tier B memory snapshot ---"
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftsnapshot-memory.yaml" >/dev/null
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  swiftsnapshot/snapshot-local-mem -n "$NAMESPACE" --timeout=5m
echo "  Snapshot Ready"

# 3. Restore two clones with full identity regeneration.
echo ""
echo "--- Step 3: Create two clones (A and B) with identity.regenerate ---"
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftrestore-clone-a.yaml" >/dev/null
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftrestore-clone-b.yaml" >/dev/null

echo "  Waiting for clone A Ready (5m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  swiftrestore/snapshot-local-clone-a -n "$NAMESPACE" --timeout=5m
echo "  Waiting for clone B Ready (5m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  swiftrestore/snapshot-local-clone-b -n "$NAMESPACE" --timeout=5m

# Verify clone pods have the snapshot-stager init container.
for clone in snapshot-local-clone-a snapshot-local-clone-b; do
  HAS_STAGER=$(kubectl get pod "$clone" -n "$NAMESPACE" -o jsonpath='{.spec.initContainers[?(@.name=="snapshot-stager")].name}' 2>/dev/null || echo "")
  if [[ -z "$HAS_STAGER" ]]; then
    echo "  FAIL: clone $clone missing snapshot-stager init container"
    exit 1
  fi
done
echo "  Both clones have snapshot-stager init container (clone path engaged)"

# 4. Wait for first-boot identity regeneration in each clone.
echo ""
echo "--- Step 4: Wait for in-guest identity regeneration sentinel ---"
for clone in snapshot-local-clone-a snapshot-local-clone-b; do
  if ! wait_for_ssh "$clone" 180; then
    echo "  FAIL: clone $clone SSH not reachable"
    exit 1
  fi
  # Wait up to 90s for /var/lib/kubeswift/.clone-regenerated to appear.
  for _ in $(seq 1 18); do
    if swiftctl ssh -n "$NAMESPACE" "$clone" -- test -f /var/lib/kubeswift/.clone-regenerated 2>/dev/null; then
      echo "  $clone: clone-regenerated sentinel present"
      break
    fi
    sleep 5
  done
done

# 5. Compare identities — A vs B vs source must all differ.
echo ""
echo "--- Step 5: Compare identities ---"
A_IDENTITY=$(identity_of snapshot-local-clone-a)
B_IDENTITY=$(identity_of snapshot-local-clone-b)
echo "  Source: $SOURCE_IDENTITY"
echo "  A:      $A_IDENTITY"
echo "  B:      $B_IDENTITY"

# Each entry is space-separated: machine-id sshfp hostname mac.
# Pull the four fields and compare.
read -r SRC_MID SRC_SSHFP SRC_HOST SRC_MAC <<< "$SOURCE_IDENTITY"
read -r A_MID   A_SSHFP   A_HOST   A_MAC   <<< "$A_IDENTITY"
read -r B_MID   B_SSHFP   B_HOST   B_MAC   <<< "$B_IDENTITY"

fail=0
check_diff() {
  local name="$1" v1="$2" v2="$3" v3="$4"
  if [[ "$v1" == "$v2" || "$v1" == "$v3" || "$v2" == "$v3" ]]; then
    echo "  FAIL: $name not unique across source/A/B (source=$v1 A=$v2 B=$v3)"
    fail=1
  else
    echo "  OK: $name unique across all three"
  fi
}
check_diff "machine-id"  "$SRC_MID"   "$A_MID"   "$B_MID"
check_diff "ssh-fp"      "$SRC_SSHFP" "$A_SSHFP" "$B_SSHFP"
check_diff "hostname"    "$SRC_HOST"  "$A_HOST"  "$B_HOST"
check_diff "mac (eth0)"  "$SRC_MAC"   "$A_MAC"   "$B_MAC"

if [[ $fail -ne 0 ]]; then
  exit 1
fi

echo ""
echo "=== Tier B clone-identity e2e PASS ==="
echo "Two clones from one memory snapshot have distinct machine-id,"
echo "SSH host fingerprint, hostname, and MAC — confirming the"
echo "stager + cloud-init bootcmd regeneration end-to-end."
