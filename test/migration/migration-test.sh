#!/usr/bin/env bash
# KubeSwift SwiftMigration e2e test: offline (default) or live migration.
#
# Verifies the SwiftMigration lifecycle end-to-end against a real cluster: a
# guest is created on one node, sentinels are written, a SwiftMigration moves
# the guest to another node, and the sentinels survive the move.
#
#   offline  a file on the root disk survives (proves the PVC moved with it).
#   live     also a file on tmpfs survives and the guest's uptime keeps
#            counting (proves the running VM moved, not a reboot).
#
# Requires:
#   - kubectl, KubeSwift cluster with CRDs + controllers deployed.
#   - Two schedulable nodes that can run guests (/dev/kvm). By default the
#     first two nodes, sorted by name, that are not cordoned and carry
#     neither the control-plane label nor its taint; pass --source/--target
#     to choose. While the guest is created, every other schedulable node is
#     cordoned so its launcher lands on the source; they are uncordoned as
#     soon as the launcher is scheduled.
#   - offline: a storage class that attaches across nodes (Longhorn, Ceph
#     RBD, EBS — NOT local-path-provisioner). live: an RWX Block class
#     (--storage-class, e.g. longhorn-migratable) or an existing guest class
#     on one (--guest-class).
#   - An SSH identity (--identity, default $KUBESWIFT_TEST_IDENTITY or
#     ~/.ssh/id_ed25519). Its public key is put into the test's seed profile.
#   - live on Longhorn: Longhorn will not live-migrate a volume that is not
#     healthy, and a new volume can stay degraded for minutes while it builds
#     its replicas. The run waits for the guest's Longhorn volumes to be
#     healthy before it migrates, for up to LONGHORN_HEALTHY_WAIT_MIN minutes
#     (default 15). This needs read access to volumes.longhorn.io.
#
# Usage:
#   ./migration-test.sh [--mode offline|live] [--source NODE] [--target NODE]
#                       [--guest-class NAME] [--storage-class NAME]
#                       [--identity PATH] [--no-cleanup]

set -euo pipefail

NAMESPACE="${NAMESPACE:-migration-e2e}"
MODE="${MIGRATION_MODE:-offline}"
SOURCE_NODE="${SOURCE_NODE:-}"
TARGET_NODE="${TARGET_NODE:-}"
GUEST_CLASS="${GUEST_CLASS:-}"
STORAGE_CLASS="${STORAGE_CLASS:-}"
IDENTITY="${KUBESWIFT_TEST_IDENTITY:-${HOME}/.ssh/id_ed25519}"
LONGHORN_HEALTHY_WAIT_MIN="${LONGHORN_HEALTHY_WAIT_MIN:-15}"
NO_CLEANUP=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)          MODE="$2"; shift 2 ;;
    --source)        SOURCE_NODE="$2"; shift 2 ;;
    --target)        TARGET_NODE="$2"; shift 2 ;;
    --guest-class)   GUEST_CLASS="$2"; shift 2 ;;
    --storage-class) STORAGE_CLASS="$2"; shift 2 ;;
    --identity)      IDENTITY="$2"; shift 2 ;;
    --no-cleanup)    NO_CLEANUP=true; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

case "$MODE" in
  offline|live) ;;
  *) echo "--mode must be offline or live, got $MODE" >&2; exit 2 ;;
esac
if [[ ! -r "$IDENTITY" ]]; then
  echo "SSH identity $IDENTITY not readable; pass --identity or set KUBESWIFT_TEST_IDENTITY" >&2
  exit 2
fi
PUBKEY="$(cat "${IDENTITY}.pub" 2>/dev/null || ssh-keygen -y -f "$IDENTITY")"
if [[ "$MODE" == "live" && -z "$GUEST_CLASS" && -z "$STORAGE_CLASS" ]]; then
  echo "--mode live needs an RWX Block class: pass --storage-class or --guest-class" >&2
  exit 2
fi

# Only the nodes this script cordoned are uncordoned, and only the class it
# created is deleted: a shared cluster may have its own cordons and classes.
CORDONED=()
CREATED_CLASS=""
cordon_node() {
  kubectl cordon "$1" >/dev/null
  CORDONED+=("$1")
}
uncordon_node() {
  kubectl uncordon "$1" >/dev/null 2>&1 || true
  local keep=() n
  for n in "${CORDONED[@]}"; do [[ "$n" == "$1" ]] || keep+=("$n"); done
  CORDONED=("${keep[@]+"${keep[@]}"}")
}
uncordon_all() {
  local n
  for n in "${CORDONED[@]+"${CORDONED[@]}"}"; do
    kubectl uncordon "$n" >/dev/null 2>&1 || true
  done
  CORDONED=()
}
cleanup() {
  uncordon_all
  if [[ "$NO_CLEANUP" == "true" ]]; then
    echo "--no-cleanup: leaving ${NAMESPACE}${CREATED_CLASS:+ and SwiftGuestClass ${CREATED_CLASS}} intact"
    return
  fi
  echo "Cleaning up..."
  kubectl delete namespace "$NAMESPACE" --wait=false 2>/dev/null || true
  if [[ -n "$CREATED_CLASS" ]]; then
    kubectl delete swiftguestclass "$CREATED_CLASS" --wait=false 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Pick two nodes: schedulable, not a control plane by label or taint. A
# label selector and custom columns, not a jsonpath filter: negated jsonpath
# filters do not parse on every kubectl release.
if [[ -z "$SOURCE_NODE" || -z "$TARGET_NODE" ]]; then
  mapfile -t CANDIDATES < <(
    kubectl get nodes -l '!node-role.kubernetes.io/control-plane' --no-headers \
      -o custom-columns='NAME:.metadata.name,UNSCHEDULABLE:.spec.unschedulable,TAINTS:.spec.taints[*].key' |
      awk '$2 != "true" && $3 !~ /node-role\.kubernetes\.io\/control-plane/ { print $1 }' | sort)
  CANDIDATES=("${CANDIDATES[@]+"${CANDIDATES[@]}"}")
  for n in "${CANDIDATES[@]+"${CANDIDATES[@]}"}"; do
    if [[ -z "$SOURCE_NODE" && "$n" != "$TARGET_NODE" ]]; then SOURCE_NODE="$n"; continue; fi
    if [[ -z "$TARGET_NODE" && "$n" != "$SOURCE_NODE" ]]; then TARGET_NODE="$n"; fi
  done
fi
if [[ -z "$SOURCE_NODE" || -z "$TARGET_NODE" || "$SOURCE_NODE" == "$TARGET_NODE" ]]; then
  echo "Need two distinct schedulable nodes; got source=${SOURCE_NODE:-none} target=${TARGET_NODE:-none}. Pass --source/--target." >&2
  exit 2
fi
echo "Mode: $MODE  Source node: $SOURCE_NODE  Target node: $TARGET_NODE"

# Guest class: the one given, or one this test owns.
if [[ -z "$GUEST_CLASS" ]]; then
  GUEST_CLASS="migration-e2e-${MODE}"
  storage=""
  if [[ "$MODE" == "live" ]]; then
    storage=$'  storage:\n    accessMode: ReadWriteMany\n    volumeMode: Block\n    storageClassName: '"$STORAGE_CLASS"
  elif [[ -n "$STORAGE_CLASS" ]]; then
    storage=$'  storage:\n    storageClassName: '"$STORAGE_CLASS"
  fi
  cat <<EOF | kubectl apply -f -
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuestClass
metadata:
  name: ${GUEST_CLASS}
spec:
  cpu: "2"
  memory: "2Gi"
  rootDisk:
    size: "10Gi"
    format: raw
${storage}
EOF
  CREATED_CLASS="$GUEST_CLASS"
fi

kubectl create namespace "$NAMESPACE" 2>/dev/null || true

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
          - ${PUBKEY}
  metaData: |
    instance-id: migration-e2e-source
    local-hostname: e2e-source
EOF

echo "Waiting for SwiftImage Ready (max 5min)..."
for _ in $(seq 1 60); do
  phase=$(kubectl get swiftimage ubuntu-noble -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [[ "$phase" == "Ready" ]]; then break; fi
  sleep 5
done
[[ "$phase" == "Ready" ]] || { echo "SwiftImage failed to reach Ready: phase=$phase" >&2; exit 1; }

# Put the guest on SOURCE_NODE: every other schedulable node is cordoned while
# the guest is created, and uncordoned as soon as its launcher is scheduled.
# Cordoning only the target let the scheduler pick a third node. The guest is
# not pinned with spec.nodeName instead: the test exercises the unpinned
# guest, which is what a migration normally moves.
if [[ "$(kubectl get node "$SOURCE_NODE" -o jsonpath='{.spec.unschedulable}')" == "true" ]]; then
  echo "Source node $SOURCE_NODE is cordoned; the guest cannot start there" >&2
  exit 2
fi
mapfile -t OTHERS < <(
  kubectl get nodes --no-headers -o custom-columns='NAME:.metadata.name,UNSCHEDULABLE:.spec.unschedulable' |
    awk -v src="$SOURCE_NODE" '$1 != src && $2 != "true" { print $1 }')
OTHERS=("${OTHERS[@]+"${OTHERS[@]}"}")
for n in "${OTHERS[@]+"${OTHERS[@]}"}"; do
  cordon_node "$n"
done
echo "Cordoned while the guest is placed: ${OTHERS[*]+"${OTHERS[*]}"}"

cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: swift.kubeswift.io/v1alpha1
kind: SwiftGuest
metadata:
  name: e2e-guest
spec:
  imageRef:
    name: ubuntu-noble
  guestClassRef:
    name: ${GUEST_CLASS}
  seedProfileRef:
    name: e2e-seed
  runPolicy: Running
  migration:
    enabled: true
    preferredMode: ${MODE}
EOF

echo "Waiting for the launcher to be scheduled (max 5min)..."
placed=""
for _ in $(seq 1 60); do
  placed=$(kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.nodeName}' 2>/dev/null || true)
  if [[ -n "$placed" ]]; then break; fi
  sleep 5
done
uncordon_all
[[ -n "$placed" ]] || { echo "SwiftGuest launcher was not scheduled within 5min" >&2; exit 1; }
echo "Launcher scheduled on $placed; the other nodes are uncordoned"

echo "Waiting for SwiftGuest Running with primaryIP (max 3min)..."
for _ in $(seq 1 36); do
  phase=$(kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  ip=$(kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}' 2>/dev/null || true)
  if [[ "$phase" == "Running" && -n "$ip" ]]; then break; fi
  sleep 5
done
[[ "$phase" == "Running" && -n "$ip" ]] || { echo "SwiftGuest failed to reach Running with IP" >&2; exit 1; }

launcher_pod() {
  kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.podRef.name}'
}
pod=$(launcher_pod)
source_node=$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
echo "Guest running at IP=$ip on node=$source_node (pod $pod)"
[[ "$source_node" == "$SOURCE_NODE" ]] || { echo "guest landed on $source_node, expected $SOURCE_NODE" >&2; exit 1; }

# Run a command in the guest over SSH from its launcher pod (no route to the
# guest from here is required).
guest_ssh() {
  local p gip
  p=$(launcher_pod)
  gip=$(kubectl get swiftguest e2e-guest -n "$NAMESPACE" -o jsonpath='{.status.network.primaryIP}')
  kubectl cp "$IDENTITY" "$NAMESPACE/$p:/tmp/key" -c launcher >/dev/null
  kubectl exec "$p" -n "$NAMESPACE" -c launcher -- sh -c \
    "chmod 600 /tmp/key && ssh -i /tmp/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 kubeswift@$gip '$1'"
}
echo "Waiting for SSH (max 3min)..."
for _ in $(seq 1 36); do
  if guest_ssh true >/dev/null 2>&1; then break; fi
  sleep 5
done

SENTINEL="MIGRATION-E2E-SENTINEL-$(date +%s)"
guest_ssh "echo $SENTINEL | sudo tee /root/sentinel.txt >/dev/null && sync"
if [[ "$MODE" == "live" ]]; then
  guest_ssh "sudo mkdir -p /run/e2e && echo $SENTINEL | sudo tee /run/e2e/sentinel >/dev/null"
  uptime_before=$(guest_ssh "cut -d. -f1 /proc/uptime")
  echo "Guest uptime before: ${uptime_before}s"
fi

# Longhorn attaches a volume to a second node for a live migration only while
# the volume is healthy; a degraded one leaves the destination pod waiting on
# FailedAttachVolume until the migration fails DstNeverReady. Wait for each of
# the guest's Longhorn volumes to be healthy first. Volumes of other drivers
# are not checked.
wait_longhorn_healthy() {
  local p claim pv driver handle lhns rob
  p=$(launcher_pod)
  for claim in $(kubectl get pod "$p" -n "$NAMESPACE" \
      -o jsonpath='{range .spec.volumes[*]}{.persistentVolumeClaim.claimName}{" "}{end}'); do
    pv=$(kubectl get pvc "$claim" -n "$NAMESPACE" -o jsonpath='{.spec.volumeName}')
    driver=$(kubectl get pv "$pv" -o jsonpath='{.spec.csi.driver}' 2>/dev/null || true)
    [[ "$driver" == "driver.longhorn.io" ]] || continue
    handle=$(kubectl get pv "$pv" -o jsonpath='{.spec.csi.volumeHandle}')
    lhns=$(kubectl get volumes.longhorn.io -A --field-selector "metadata.name=$handle" \
      -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)
    if [[ -z "$lhns" ]]; then
      echo "WARN: cannot read Longhorn volume $handle (PVC $claim); migrating without checking that it is healthy"
      continue
    fi
    echo "Waiting for Longhorn volume $handle (PVC $claim) to be healthy (max ${LONGHORN_HEALTHY_WAIT_MIN}min)..."
    rob=""
    for _ in $(seq 1 $((LONGHORN_HEALTHY_WAIT_MIN * 12))); do
      rob=$(kubectl get volumes.longhorn.io "$handle" -n "$lhns" -o jsonpath='{.status.robustness}' 2>/dev/null || true)
      if [[ "$rob" == "healthy" ]]; then break; fi
      sleep 5
    done
    [[ "$rob" == "healthy" ]] || {
      echo "Longhorn volume $handle is ${rob:-unknown} after ${LONGHORN_HEALTHY_WAIT_MIN}min; Longhorn will not live-migrate it" >&2
      exit 1
    }
    echo "Longhorn volume $handle is healthy"
  done
}
if [[ "$MODE" == "live" ]]; then
  wait_longhorn_healthy
fi

# Pin the source and migrate.
cordon_node "$SOURCE_NODE"

T0=$(date +%s)
swiftctl_bin="${SWIFTCTL:-./bin/swiftctl}"
if [[ ! -x "$swiftctl_bin" ]]; then
  echo "swiftctl not found at $swiftctl_bin; building..."
  go build -o "$swiftctl_bin" ./cmd/swiftctl
fi
"$swiftctl_bin" -n "$NAMESPACE" migrate e2e-guest --to "$TARGET_NODE" --allow-ip-change \
  --preferred-mode "$MODE" --name e2e-mig

echo "Waiting for SwiftMigration Completed (max 10min)..."
for i in $(seq 1 120); do
  phase=$(kubectl get swiftmigration e2e-mig -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  detail=$(kubectl get swiftmigration e2e-mig -n "$NAMESPACE" -o jsonpath='{.status.phaseDetail}' 2>/dev/null || true)
  echo "  [$((i*5))s] phase=$phase detail=$detail"
  if [[ "$phase" == "Completed" ]]; then break; fi
  if [[ "$phase" == "Failed" || "$phase" == "Cancelled" ]]; then
    fail=$(kubectl get swiftmigration e2e-mig -n "$NAMESPACE" -o jsonpath='{.status.failureMessage}')
    echo "Migration $phase: $fail" >&2
    exit 1
  fi
  sleep 5
done
[[ "$phase" == "Completed" ]] || { echo "Migration did not complete: phase=$phase" >&2; exit 1; }
resolved=$(kubectl get swiftmigration e2e-mig -n "$NAMESPACE" -o jsonpath='{.status.mode}' 2>/dev/null || true)
echo "Migration completed in $(( $(date +%s) - T0 ))s (resolved mode: ${resolved:-unknown})"
if [[ "$MODE" == "live" && -n "$resolved" && "$resolved" != "live" ]]; then
  echo "Asked for a live migration, the controller resolved it to $resolved" >&2
  exit 1
fi
uncordon_node "$SOURCE_NODE"

# The guest runs on the target now: status.podRef follows the cutover (a live
# migration's pod is <guest>-mig-<uid>).
pod=$(launcher_pod)
new_node=$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
[[ "$new_node" == "$TARGET_NODE" ]] || { echo "post-migration node = $new_node (pod $pod), expected $TARGET_NODE" >&2; exit 1; }
echo "Post-migration pod $pod on $new_node"

got=$(guest_ssh "sudo cat /root/sentinel.txt" 2>/dev/null || echo MISSING)
[[ "$got" == "$SENTINEL" ]] || { echo "disk sentinel: expected $SENTINEL, got $got" >&2; exit 1; }
echo "PASS: disk sentinel survived the migration"
if [[ "$MODE" == "live" ]]; then
  got=$(guest_ssh "cat /run/e2e/sentinel" 2>/dev/null || echo MISSING)
  [[ "$got" == "$SENTINEL" ]] || { echo "tmpfs sentinel: expected $SENTINEL, got $got (the VM was restarted, not moved)" >&2; exit 1; }
  uptime_after=$(guest_ssh "cut -d. -f1 /proc/uptime")
  (( uptime_after >= uptime_before )) || { echo "guest uptime went back from ${uptime_before}s to ${uptime_after}s: it rebooted" >&2; exit 1; }
  echo "PASS: tmpfs sentinel survived and uptime kept counting (${uptime_before}s -> ${uptime_after}s): the running VM moved"
fi

# Webhook rejection check: a SwiftMigration of a guest whose migration policy
# is disabled must be refused at admission. Only where the webhook runs.
if kubectl get validatingwebhookconfiguration kubeswift-validating-webhook >/dev/null 2>&1; then
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
else
  echo "SKIP: webhook not installed; admission check not run"
fi

echo
echo "All checks passed."
