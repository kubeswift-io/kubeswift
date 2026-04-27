#!/usr/bin/env bash
# KubeSwift Tier B clone-identity-regeneration e2e test.
#
# Purpose: prove that two clones from a single memory snapshot end up
# with distinct identities (machine-id, SSH host keys, hostname, MAC)
# despite resuming from the same captured RAM. This is the contract
# the SwiftRestore stager + in-guest cloud-init bootcmd implement.
#
# The combined seed profile (config/samples/local-snapshots/
# swiftseedprofile-test.yaml) is applied to the source VM. When the
# controller patches the snapshot's config.json with kubeswift.clone=true
# and per-clone MAC rewrites, the stager init container materializes the
# patched copy in a pod-local emptyDir; the in-guest bootcmd regenerates
# machine-id, SSH host keys, and hostname on first wake.
#
# This script also captures and reports clone-from-snapshot timings
# (SwiftRestore -> launcher Running -> SwiftRestore Ready) so we get
# a feel for the cost of the Tier B clone path on this cluster.
#
# Requires:
#   - kubectl configured against a KubeSwift cluster.
#   - Source VM image: ubuntu-noble (created on first run if missing).
#   - SSH key for swiftctl-default user "kubeswift" (matches the
#     ssh_authorized_keys baked into the test seed profile).

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

echo "=== KubeSwift Tier B clone-identity e2e ==="
echo "Namespace: $NAMESPACE"
echo "Identity:  $IDENTITY"
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

# guest_exec — same pattern as local-roundtrip-test.sh: kubectl exec
# into the launcher and run ssh from inside, with the host's identity
# piped in and the remote command base64-encoded so quotes/newlines
# survive the shell chain.
guest_exec() {
  local guest="$1"; shift
  local cmd="$*"
  local ip
  ip=$(kubectl get swiftguest "$guest" -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null)
  if [[ -z "$ip" ]]; then
    echo "guest_exec: $guest has no primaryIP" >&2
    return 1
  fi
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

wait_for_ssh() {
  local guest="$1"
  local timeout="${2:-180}"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    if guest_exec "$guest" true >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done
  return 1
}

# identity_of: run a fixed bash one-liner inside the guest and parse
# four space-separated fields: machine-id sshfp hostname mac.
identity_of() {
  local guest="$1"
  guest_exec "$guest" "bash -c 'mid=\$(cat /etc/machine-id 2>/dev/null || echo missing); sshfp=\$(ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub 2>/dev/null | awk \"{print \\\$2}\"); if [ -z \"\$sshfp\" ]; then sshfp=\$(ssh-keygen -lf /etc/ssh/ssh_host_rsa_key.pub 2>/dev/null | awk \"{print \\\$2}\"); fi; [ -z \"\$sshfp\" ] && sshfp=missing; hn=\$(hostname 2>/dev/null || echo missing); mac=\$(cat /sys/class/net/eth0/address 2>/dev/null || echo missing); echo \"\$mid \$sshfp \$hn \$mac\"'" 2>/dev/null | tail -1
}

# epoch_now / elapsed_since: integer-second timing helpers.
epoch_now() { date +%s; }
elapsed_since() { echo $(($(epoch_now) - $1)); }

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

# 3. Restore two clones with full identity regeneration. Capture
#    timing milestones for each clone so we can report wall-clock
#    cost of the Tier B clone path.
echo ""
echo "--- Step 3: Create two clones (A and B) with identity.regenerate ---"

declare -A T_RESTORE_CREATED T_GUEST_RUNNING T_RESTORE_READY T_SSH_REACHABLE T_SENTINEL_PRESENT

# Wait helper for a SwiftRestore phase, returns the wall-clock seconds
# elapsed from the wait start to the phase reaching the target value.
wait_phase_match() {
  local kind="$1" name="$2" target="$3" timeout="$4"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    local cur
    cur=$(kubectl get "$kind" "$name" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "$cur" == "$target" ]]; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  return 1
}

# Wait helper for a SwiftGuest condition (e.g. GuestRunning=True).
wait_guest_running() {
  local name="$1" timeout="$2"
  local elapsed=0
  while [[ $elapsed -lt $timeout ]]; do
    local cur
    cur=$(kubectl get swiftguest "$name" -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="GuestRunning")].status}' 2>/dev/null || echo "")
    if [[ "$cur" == "True" ]]; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  return 1
}

for clone in snapshot-local-clone-a snapshot-local-clone-b; do
  T_RESTORE_CREATED[$clone]=$(epoch_now)
  echo "  [$clone] Apply SwiftRestore (clone path)"
  kubectl apply -n "$NAMESPACE" -f "$SAMPLES_DIR/swiftrestore-${clone#snapshot-local-}.yaml" >/dev/null
done

# Track each milestone independently.
for clone in snapshot-local-clone-a snapshot-local-clone-b; do
  echo ""
  echo "  [$clone] waiting for GuestRunning=True (CH socket bound, paused)..."
  if ! wait_guest_running "$clone" 300; then
    echo "    FAIL: $clone never reached GuestRunning=True"
    exit 1
  fi
  T_GUEST_RUNNING[$clone]=$(epoch_now)
  echo "    GuestRunning=True after $(($(epoch_now) - ${T_RESTORE_CREATED[$clone]}))s"

  echo "  [$clone] waiting for SwiftRestore Ready (resume action mirrored)..."
  if ! wait_phase_match swiftrestore "$clone" Ready 300; then
    echo "    FAIL: $clone SwiftRestore never reached Ready"
    kubectl describe swiftrestore "$clone" -n "$NAMESPACE" 2>&1 | tail -15 || true
    exit 1
  fi
  T_RESTORE_READY[$clone]=$(epoch_now)
  echo "    SwiftRestore Ready after $(($(epoch_now) - ${T_RESTORE_CREATED[$clone]}))s"
done

# Verify clone pods have the snapshot-stager init container.
for clone in snapshot-local-clone-a snapshot-local-clone-b; do
  HAS_STAGER=$(kubectl get pod "$clone" -n "$NAMESPACE" -o jsonpath='{.spec.initContainers[?(@.name=="snapshot-stager")].name}' 2>/dev/null || echo "")
  if [[ -z "$HAS_STAGER" ]]; then
    echo "  FAIL: clone $clone missing snapshot-stager init container"
    exit 1
  fi
done
echo ""
echo "  Both clones have snapshot-stager init container (clone path engaged)"

# 4. Wait for first-boot identity regeneration in each clone.
echo ""
echo "--- Step 4: Wait for in-guest identity regeneration sentinel ---"
for clone in snapshot-local-clone-a snapshot-local-clone-b; do
  echo "  [$clone] waiting for SSH..."
  if ! wait_for_ssh "$clone" 180; then
    echo "    FAIL: clone $clone SSH not reachable"
    exit 1
  fi
  T_SSH_REACHABLE[$clone]=$(epoch_now)
  echo "    SSH reachable after $(($(epoch_now) - ${T_RESTORE_CREATED[$clone]}))s"

  # Wait up to 90s for /var/lib/kubeswift/.clone-regenerated to appear.
  for _ in $(seq 1 18); do
    if guest_exec "$clone" "test -f /var/lib/kubeswift/.clone-regenerated" >/dev/null 2>&1; then
      T_SENTINEL_PRESENT[$clone]=$(epoch_now)
      echo "    clone-regenerated sentinel present after $(($(epoch_now) - ${T_RESTORE_CREATED[$clone]}))s"
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

# 6. Timing summary — the user wants to know how long Tier B clone
#    startup costs on this cluster. We report the wall-clock between
#    SwiftRestore creation and each milestone, for each clone.
echo ""
echo "--- Timing summary (seconds since SwiftRestore creation) ---"
printf "%-30s %12s %14s %16s %18s\n" "clone" "GuestRunning" "RestoreReady" "SSH-reachable" "regen-sentinel"
for clone in snapshot-local-clone-a snapshot-local-clone-b; do
  t0=${T_RESTORE_CREATED[$clone]}
  gr="${T_GUEST_RUNNING[$clone]:-}"
  rr="${T_RESTORE_READY[$clone]:-}"
  sr="${T_SSH_REACHABLE[$clone]:-}"
  sp="${T_SENTINEL_PRESENT[$clone]:-}"
  fmt() { [[ -z "$1" ]] && echo "—" || echo "$(($1 - t0))s"; }
  printf "%-30s %12s %14s %16s %18s\n" "$clone" "$(fmt "$gr")" "$(fmt "$rr")" "$(fmt "$sr")" "$(fmt "$sp")"
done

echo ""
echo "=== Tier B clone-identity e2e PASS ==="
echo "Two clones from one memory snapshot have distinct machine-id,"
echo "SSH host fingerprint, hostname, and MAC — confirming the"
echo "stager + cloud-init bootcmd regeneration end-to-end."
