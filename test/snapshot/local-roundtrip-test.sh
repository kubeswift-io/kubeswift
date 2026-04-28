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
IDENTITY="${KUBESWIFT_TEST_IDENTITY:-${HOME}/.ssh/id_ed25519}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --no-cleanup) NO_CLEANUP=true; shift ;;
    --namespace)  NAMESPACE="$2"; shift 2 ;;
    --identity)   IDENTITY="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

if [[ ! -r "$IDENTITY" ]]; then
  echo "ERROR: SSH identity $IDENTITY not readable. Override with --identity or KUBESWIFT_TEST_IDENTITY." >&2
  exit 2
fi

echo "=== KubeSwift Tier B round-trip e2e ==="
echo "Namespace:    $NAMESPACE"
echo "Samples:      $SAMPLES_DIR"
echo "Identity:     $IDENTITY (must match seed profile's ssh_authorized_keys)"
echo ""

# guest_exec runs a one-line bash command inside the guest VM by
# exec-ing into the launcher pod and running ssh from there. The
# host's private key is fed via stdin into the launcher, written to a
# tmpfile, used, then removed. swiftctl ssh insists on a TTY which
# doesn't compose with scripted command runs; this helper avoids that
# limitation.
#
# The input is piped: { key contents \0 SSH_COMMAND }. The launcher's
# sh splits the stdin into the key (everything up to a sentinel line)
# and the remote command (the rest of the lines).
#
# Usage: guest_exec <guest-name> <bash one-liner>
# Returns the remote command's exit code; remote stdout on stdout.
guest_exec() {
  local guest="$1"; shift
  local cmd="$*"
  local ip
  ip=$(kubectl get swiftguest "$guest" -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null)
  if [[ -z "$ip" ]]; then
    echo "guest_exec: $guest has no primaryIP" >&2
    return 1
  fi
  # Build the in-launcher script. The host's identity bytes are
  # interpolated via heredoc into the script body, so we don't depend
  # on env-var propagation across kubectl exec.
  # The remote command is base64-encoded so it can carry quotes,
  # newlines, and shell metachars unchanged through the heredoc.
  # The launcher decodes and runs via bash -c. Avoids fragile
  # shell-quoting in the kubectl/sh/ssh/bash chain.
  local cmd_b64
  cmd_b64=$(printf '%s' "$cmd" | base64 -w0)
  local launcher_script
  launcher_script=$(cat <<LAUNCHER_EOF
set -eu
KEY=\$(mktemp); chmod 600 "\$KEY"
cat > "\$KEY" <<'KUBESWIFT_TEST_KEY'
$(cat "$IDENTITY")
KUBESWIFT_TEST_KEY
CMD=\$(printf %s '$cmd_b64' | base64 -d)
RC=0
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=5 -i "\$KEY" kubeswift@$ip "\$CMD" || RC=\$?
rm -f "\$KEY"
exit \$RC
LAUNCHER_EOF
)
  printf '%s\n' "$launcher_script" \
    | kubectl exec -n "$NAMESPACE" -i "$guest" -c launcher -- sh
}

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
    swiftseedprofile/snapshot-local-test-seed >/dev/null 2>&1 || true
}
trap cleanup EXIT

# 1. Apply the combined seed profile (kubeswift user for swiftctl ssh +
#    clone-identity-regen bootcmd) and the source SwiftGuest.
echo "--- Step 1: Apply source manifests ---"
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/01-seed-profile.yaml" >/dev/null
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/02-source-guest.yaml" >/dev/null

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

# Wait for the guest to actually be reachable via SSH. The Running
# phase asserts CH is bound; SSH usability comes a few seconds later
# when sshd reports.
echo "  Waiting up to 3m for SSH reachability..."
SSH_READY=false
for _ in $(seq 1 36); do
  if guest_exec snapshot-local-source true >/dev/null 2>&1; then
    SSH_READY=true
    break
  fi
  sleep 5
done
if [[ "$SSH_READY" != "true" ]]; then
  echo "  FAIL: SSH never became reachable on snapshot-local-source"
  exit 1
fi
echo "  OK: SSH reachable"

# 2. Plant the tmpfs sentinel — RAM-only, NOT persisted to disk.
echo ""
echo "--- Step 2: Plant tmpfs sentinel inside the guest (in-memory only) ---"
SENTINEL_VALUE="kubeswift-roundtrip-$(date +%s)-$RANDOM"
guest_exec snapshot-local-source "sudo mkdir -p /run/kubeswift-mem"
guest_exec snapshot-local-source "echo $SENTINEL_VALUE | sudo tee /run/kubeswift-mem/sentinel >/dev/null"
fs_type=$(guest_exec snapshot-local-source "stat -fc %T /run/kubeswift-mem" | tr -d '\r\n')
if [[ "$fs_type" != "tmpfs" ]]; then
  echo "  FAIL: /run/kubeswift-mem is not tmpfs (got '$fs_type')"
  exit 1
fi
echo "  Planted: SENTINEL_VALUE=$SENTINEL_VALUE on tmpfs"

# 3. Snapshot with includeMemory: true.
echo ""
echo "--- Step 3: Take Tier B memory snapshot ---"
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/03-snapshot.yaml" >/dev/null
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
kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/04-restore-inplace.yaml" >/dev/null
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
echo "  Waiting up to 3m for SSH reachability post-restore..."
for _ in $(seq 1 36); do
  if guest_exec snapshot-local-source true >/dev/null 2>&1; then
    break
  fi
  sleep 5
done
GOT_VALUE=$(guest_exec snapshot-local-source "cat /run/kubeswift-mem/sentinel 2>/dev/null || echo MISSING" | tr -d '\r\n')
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
