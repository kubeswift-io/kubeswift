#!/usr/bin/env bash
# KubeSwift snapshot/restore e2e test (csi-volume-snapshot backend).
#
# Verifies the full SwiftSnapshot + SwiftRestore lifecycle end-to-end
# against a real cluster.
#
# Requires:
#   - kubectl, KubeSwift cluster with CRDs and controllers deployed.
#   - A snapshot-capable CSI driver and a default VolumeSnapshotClass.
#     (The script auto-detects the default class; pass --vsclass to override.)
#
# Usage:
#   ./snapshot-test.sh [--vsclass <name>] [--no-cleanup]

set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
VSCLASS=""
NO_CLEANUP=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --vsclass)    VSCLASS="$2"; shift 2 ;;
    --no-cleanup) NO_CLEANUP=true; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

# Auto-detect the default VolumeSnapshotClass if --vsclass not provided.
if [[ -z "$VSCLASS" ]]; then
  VSCLASS=$(kubectl get volumesnapshotclass -o jsonpath='{range .items[?(@.metadata.annotations.snapshot\.storage\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1)
fi
if [[ -z "$VSCLASS" ]]; then
  echo "ERROR: no default VolumeSnapshotClass found and --vsclass not provided" >&2
  exit 2
fi

echo "=== KubeSwift snapshot e2e test ==="
echo "Namespace: $NAMESPACE"
echo "VolumeSnapshotClass: $VSCLASS"
echo ""

cleanup() {
  if [[ "$NO_CLEANUP" == "true" ]]; then
    echo "(skipping cleanup; --no-cleanup)"
    return
  fi
  echo ""
  echo "--- Cleanup ---"
  kubectl delete -n "$NAMESPACE" --ignore-not-found \
    swiftrestore/snapshot-e2e-restore \
    swiftsnapshot/snapshot-e2e-snap \
    swiftguest/snapshot-e2e-restored \
    swiftguest/snapshot-e2e-source >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Use shared classes if present (don't recreate).
kubectl apply -n "$NAMESPACE" -f - >/dev/null <<'EOF'
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: snapshot-e2e-class
spec:
  cpu: 2
  memory: "2Gi"
  rootDisk:
    size: "10Gi"
    format: raw
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: snapshot-e2e-seed
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    users:
      - name: ubuntu
        sudo: ALL=(ALL) NOPASSWD:ALL
        ssh_authorized_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPlaceholder
EOF

echo "--- Step 1: Create source SwiftImage + SwiftGuest ---"
# Reuse ubuntu-noble if it already exists; else create.
if ! kubectl get swiftimage ubuntu-noble -n "$NAMESPACE" >/dev/null 2>&1; then
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
fi

echo "  Waiting for SwiftImage Ready (15m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftimage/ubuntu-noble -n "$NAMESPACE" --timeout=15m

kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: snapshot-e2e-source
spec:
  imageRef: {name: ubuntu-noble}
  guestClassRef: {name: snapshot-e2e-class}
  seedProfileRef: {name: snapshot-e2e-seed}
  runPolicy: Running
EOF

echo "  Waiting for source SwiftGuest Running (5m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Running swiftguest/snapshot-e2e-source -n "$NAMESPACE" --timeout=5m

echo ""
echo "--- Step 2: Create SwiftSnapshot ---"
kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftSnapshot
metadata:
  name: snapshot-e2e-snap
spec:
  guestRef: {name: snapshot-e2e-source}
  backend:
    type: csi-volume-snapshot
    csiVolumeSnapshot:
      volumeSnapshotClassName: $VSCLASS
EOF

echo "  Waiting for SwiftSnapshot Ready (10m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftsnapshot/snapshot-e2e-snap -n "$NAMESPACE" --timeout=10m

# Verify the underlying VolumeSnapshot exists and is readyToUse.
VS_NAME="swift-snap-snapshot-e2e-snap"
echo "  Verifying VolumeSnapshot $VS_NAME readyToUse..."
ready=$(kubectl get volumesnapshot "$VS_NAME" -n "$NAMESPACE" -o jsonpath='{.status.readyToUse}' 2>/dev/null || echo "")
if [[ "$ready" != "true" ]]; then
  echo "  FAIL: VolumeSnapshot $VS_NAME readyToUse=$ready"
  exit 1
fi

# Verify SwiftSnapshot.status.disks[0].handle matches.
got_handle=$(kubectl get swiftsnapshot snapshot-e2e-snap -n "$NAMESPACE" -o jsonpath='{.status.disks[0].handle}')
expected_handle="${NAMESPACE}/${VS_NAME}"
if [[ "$got_handle" != "$expected_handle" ]]; then
  echo "  FAIL: status.disks[0].handle = '$got_handle', want '$expected_handle'"
  exit 1
fi
echo "  OK: VolumeSnapshot ready, handle matches"

echo ""
echo "--- Step 3: Create SwiftRestore -> new SwiftGuest ---"
kubectl apply -n "$NAMESPACE" -f - >/dev/null <<EOF
apiVersion: snapshot.kubeswift.io/v1alpha1
kind: SwiftRestore
metadata:
  name: snapshot-e2e-restore
spec:
  snapshotRef: {name: snapshot-e2e-snap}
  targetGuest: {name: snapshot-e2e-restored}
  resumeAfterRestore: true
EOF

echo "  Waiting for SwiftRestore Ready (10m)..."
kubectl wait --for=jsonpath='{.status.phase}'=Ready swiftrestore/snapshot-e2e-restore -n "$NAMESPACE" --timeout=10m

# Verify target guest reached Running.
echo "  Verifying restored SwiftGuest Running..."
phase=$(kubectl get swiftguest snapshot-e2e-restored -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
if [[ "$phase" != "Running" ]]; then
  echo "  FAIL: restored guest phase=$phase, want Running"
  exit 1
fi

# Verify the per-guest PVC carries the restore-seeded label.
PVC_NAME="swiftguest-root-snapshot-e2e-restored"
label=$(kubectl get pvc "$PVC_NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.labels.swift\.kubeswift\.io/restore-seeded}' 2>/dev/null || echo "")
if [[ "$label" != "true" ]]; then
  echo "  FAIL: $PVC_NAME label swift.kubeswift.io/restore-seeded=$label, want true"
  exit 1
fi

# Verify dataSource is the snapshot's VolumeSnapshot.
ds_kind=$(kubectl get pvc "$PVC_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.dataSource.kind}')
ds_name=$(kubectl get pvc "$PVC_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.dataSource.name}')
if [[ "$ds_kind" != "VolumeSnapshot" || "$ds_name" != "$VS_NAME" ]]; then
  echo "  FAIL: $PVC_NAME dataSource = $ds_kind/$ds_name, want VolumeSnapshot/$VS_NAME"
  exit 1
fi

echo ""
echo "=== snapshot e2e PASS ==="
