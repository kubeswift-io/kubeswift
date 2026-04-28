#!/usr/bin/env bash
# KubeSwift SwiftMigration e2e test (Phase 1: offline migration).
#
# Verifies the full SwiftMigration lifecycle end-to-end against a real
# cluster: a guest is created on one node, a sentinel file is written,
# a SwiftMigration moves the guest to a different node, and the
# sentinel survives the move (proves direct PVC reuse worked).
#
# The test mirrors the Phase 1 spike's experimental design but as an
# automated check that runs in CI (per the snapshot CI workflow).
#
# Requires:
#   - kubectl, KubeSwift cluster with CRDs + controllers deployed.
#   - At least two schedulable worker nodes labeled equivalently.
#   - A storage class that supports cross-node attach (Longhorn, Rook
#     Ceph RBD, EBS — NOT local-path-provisioner).
#   - SSH key at ~/.ssh/id_ed25519 matching the seed profile's
#     authorized_keys.
#
# Usage:
#   ./migration-test.sh [--no-cleanup]

set -euo pipefail

NAMESPACE="${NAMESPACE:-migration-e2e}"
NO_CLEANUP=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-cleanup) NO_CLEANUP=true; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

cleanup() {
  if [[ "$NO_CLEANUP" == "true" ]]; then
    echo "--no-cleanup: leaving ${NAMESPACE} intact"
    return
  fi
  echo "Cleaning up..."
  kubectl uncordon --all 2>/dev/null || true
  kubectl delete namespace "$NAMESPACE" --wait=false 2>/dev/null || true
}
trap cleanup EXIT

# Pick two worker nodes (skip control-plane).
mapfile -t WORKERS < <(kubectl get nodes -o jsonpath='{range .items[?(@.spec.taints[?(@.key=="node-role.kubernetes.io/control-plane")])].metadata.name}{""}{end}{range .items[?(!@.spec.taints[?(@.key=="node-role.kubernetes.io/control-plane")])].metadata.name}{.}{"\n"}{end}' | grep -v '^$')
if [[ "${#WORKERS[@]}" -lt 2 ]]; then
  echo "Need at least 2 schedulable worker nodes; found ${#WORKERS[@]}" >&2
  exit 2
fi
SOURCE_NODE="${WORKERS[0]}"
TARGET_NODE="${WORKERS[1]}"
echo "Source node: $SOURCE_NODE  Target node: $TARGET_NODE"

# Set up namespace + RBAC. Patch the rolebinding subject namespace
# (snapshot walkthrough F2 — the default rolebinding hardcodes
# "default" as the SA namespace).
kubectl create namespace "$NAMESPACE" 2>/dev/null || true
kubectl apply -k config/rbac -n "$NAMESPACE"
kubectl patch rolebinding swiftletd-reporter -n "$NAMESPACE" --type=json \
  -p '[{"op":"replace","path":"/subjects/0/namespace","value":"'"$NAMESPACE"'"}]' || true

# Cordon target so the source guest lands on SOURCE_NODE.
kubectl cordon "$TARGET_NODE"
trap 'kubectl uncordon "$TARGET_NODE" 2>/dev/null || true; cleanup' EXIT

# Apply the source manifest.
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
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
---
apiVersion: seed.kubeswift.io/v1alpha1
kind: SwiftSeedProfile
metadata:
  name: e2e-seed
spec:
  datasource: NoCloud
  userData: |
    #cloud-config
    hostname: e2e-source
    users:
      - name: kubeswift
        sudo: ALL=(ALL) NOPASSWD:ALL
        ssh_authorized_keys:
          - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ53Xu8rRSofCQqb91XUgZqQam+5Q2e7tOwr0egG/W5x
  metaData: |
    instance-id: migration-e2e-source
    local-hostname: e2e-source
---
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: e2e-guest
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: default
  seedProfileRef:
    name: e2e-seed
  runPolicy: Running
EOF

echo "Waiting for SwiftImage Ready (max 5min)..."
for i in $(seq 1 60); do
  phase=$(kubectl get swiftimage ubuntu-noble -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "$phase" == "Ready" ]]; then break; fi
  sleep 5
done
[[ "$phase" == "Ready" ]] || { echo "SwiftImage failed to reach Ready: phase=$phase" >&2; exit 1; }

echo "Waiting for SwiftGuest Running with primaryIP (max 3min)..."
for i in $(seq 1 36); do
  phase=$(kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  ip=$(kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null || true)
  if [[ "$phase" == "Running" && -n "$ip" ]]; then break; fi
  sleep 5
done
[[ "$phase" == "Running" && -n "$ip" ]] || { echo "SwiftGuest failed to reach Running with IP" >&2; exit 1; }

source_node=$(kubectl get pod e2e-guest -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
echo "Guest running at IP=$ip on node=$source_node"
[[ "$source_node" == "$SOURCE_NODE" ]] || { echo "guest landed on $source_node, expected $SOURCE_NODE" >&2; exit 1; }

# Write a sentinel via the launcher pod (no public IP path required).
kubectl cp ~/.ssh/id_ed25519 "$NAMESPACE"/e2e-guest:/tmp/key -c launcher
SENTINEL="MIGRATION-E2E-SENTINEL-$(date +%s)"
kubectl exec e2e-guest -n "$NAMESPACE" -c launcher -- sh -c \
  "chmod 600 /tmp/key && ssh -i /tmp/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null kubeswift@$ip 'echo $SENTINEL | sudo tee /root/sentinel.txt'"

# Now: uncordon target, cordon source (so the new pod must land on
# TARGET_NODE). Submit the migration.
kubectl uncordon "$TARGET_NODE"
kubectl cordon "$SOURCE_NODE"

T0=$(date +%s)
swiftctl_bin="${SWIFTCTL:-./bin/swiftctl}"
if [[ ! -x "$swiftctl_bin" ]]; then
  echo "swiftctl not found at $swiftctl_bin; building..."
  go build -o "$swiftctl_bin" ./cmd/swiftctl
fi
"$swiftctl_bin" -n "$NAMESPACE" migrate e2e-guest --to "$TARGET_NODE" --allow-ip-change \
  --name e2e-mig

echo "Waiting for SwiftMigration Completed (max 5min)..."
for i in $(seq 1 60); do
  phase=$(kubectl get swiftmigration e2e-mig -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  detail=$(kubectl get swiftmigration e2e-mig -n "$NAMESPACE" -o jsonpath='{.status.phaseDetail}' 2>/dev/null || true)
  echo "  [$((i*5))s] phase=$phase detail=$detail"
  if [[ "$phase" == "Completed" ]]; then break; fi
  if [[ "$phase" == "Failed" ]]; then
    fail=$(kubectl get swiftmigration e2e-mig -n "$NAMESPACE" -o jsonpath='{.status.failureMessage}')
    echo "Migration Failed: $fail" >&2
    exit 1
  fi
  sleep 5
done
[[ "$phase" == "Completed" ]] || { echo "Migration did not complete: phase=$phase" >&2; exit 1; }

T1=$(date +%s)
echo "Migration completed in $((T1 - T0))s"

# Verify the guest now runs on TARGET_NODE.
new_node=$(kubectl get pod e2e-guest -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
[[ "$new_node" == "$TARGET_NODE" ]] || { echo "post-migration node = $new_node, expected $TARGET_NODE" >&2; exit 1; }

# Verify the sentinel survived the cross-node attach.
new_ip=$(kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}')
echo "Post-migration IP: $new_ip"
kubectl cp ~/.ssh/id_ed25519 "$NAMESPACE"/e2e-guest:/tmp/key -c launcher
got_sentinel=$(kubectl exec e2e-guest -n "$NAMESPACE" -c launcher -- sh -c \
  "chmod 600 /tmp/key && ssh -i /tmp/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null kubeswift@$new_ip 'sudo cat /root/sentinel.txt'" \
  2>/dev/null || echo "MISSING")
if [[ "$got_sentinel" != "$SENTINEL" ]]; then
  echo "Sentinel survival check FAILED: expected $SENTINEL, got $got_sentinel" >&2
  exit 1
fi
echo "PASS: sentinel \"$got_sentinel\" survived the cross-node migration"

# Webhook rejection check: a SwiftMigration of a guest with
# enabled=false on its migration policy must be rejected at admission.
kubectl patch swiftguest e2e-guest -n "$NAMESPACE" --type merge \
  -p '{"spec":{"migration":{"enabled":false}}}'
if kubectl create -n "$NAMESPACE" -f - <<EOF >/dev/null 2>&1
apiVersion: migration.kubeswift.io/v1alpha1
kind: SwiftMigration
metadata:
  name: e2e-mig-disabled
spec:
  guestRef:
    name: e2e-guest
  target:
    nodeName: $SOURCE_NODE
EOF
then
  echo "Webhook should have rejected migration of disabled guest, but accepted it" >&2
  exit 1
fi
echo "PASS: webhook rejected migration of guest with migration.enabled=false"

echo
echo "All checks passed."
